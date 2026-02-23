/*
Copyright 2026 The KubeVirt Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package qmp implements a QMP (QEMU Machine Protocol) client.
//
// Production use: DialViaLibvirt routes commands through the local virtqemud
// instance via the libvirt remote protocol.  In modern KubeVirt, virtqemud
// holds the sole QMP connection to QEMU, so external clients must proxy
// through it rather than connecting to QEMU's monitor socket directly.
//
// Testing use: Dial connects directly to a raw QMP UNIX socket and speaks
// the QMP JSON protocol, enabling unit tests with a fake QMP server.
package qmp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"

	libvirt "github.com/digitalocean/go-libvirt"
	"github.com/digitalocean/go-libvirt/socket/dialers"
)

// qmpFlag=0 is the correct value for QMP mode.  Despite the constant name
// VIR_DOMAIN_QEMU_MONITOR_COMMAND_QMP=1 in the libvirt C API, virtqemud
// accepts QMP JSON when flags=0 (the virsh default, confirmed via
// LIBVIRT_DEBUG=1 output: flags=0x0).  flags=1 causes virtqemud to wrap
// the payload as an HMP command, making QEMU treat the JSON as a literal
// unknown HMP command name.
const qmpFlag uint32 = 0

// Client is a stateful connection to a single QEMU domain.  It may be backed
// by a raw QMP socket (tests) or by virtqemud via the libvirt remote protocol
// (production).
type Client struct {
	execFn  func(ctx context.Context, command string, args any) (json.RawMessage, error)
	closeFn func() error
}

// Close releases the underlying connection.
func (c *Client) Close() error { return c.closeFn() }

// DialViaLibvirt connects to the virtqemud socket at virtqemudSockPath
// (which may be a /proc/<pid>/root/… path), authenticates, looks up the
// domain by its libvirt name (<namespace>_<vmiName>), and returns a Client
// ready to issue QMP commands.
func DialViaLibvirt(virtqemudSockPath, domainName string) (*Client, error) {
	conn, err := net.Dial("unix", virtqemudSockPath)
	if err != nil {
		return nil, fmt.Errorf("dialing virtqemud %s: %w", virtqemudSockPath, err)
	}

	lv := libvirt.NewWithDialer(dialers.NewAlreadyConnected(conn))
	// virtqemud inside virt-launcher runs as a session daemon; Connect() uses
	// QEMUSystem by default, which is rejected.  Use ConnectToURI with the
	// pre-defined QEMUSession constant instead.
	if err := lv.ConnectToURI(libvirt.QEMUSession); err != nil {
		conn.Close()
		return nil, fmt.Errorf("libvirt connect: %w", err)
	}

	dom, err := lv.DomainLookupByName(domainName)
	if err != nil {
		lv.Disconnect() //nolint:errcheck
		return nil, fmt.Errorf("domain lookup %q: %w", domainName, err)
	}

	return &Client{
		execFn: func(ctx context.Context, command string, args any) (json.RawMessage, error) {
			if dl, ok := ctx.Deadline(); ok {
				conn.SetDeadline(dl)
				defer conn.SetDeadline(time.Time{})
			}
			return execViaLibvirt(lv, dom, command, args)
		},
		closeFn: func() error { return lv.Disconnect() },
	}, nil
}

// Dial connects directly to a raw QMP UNIX socket and performs the QMP
// capability negotiation handshake.  This is intended for unit tests; in
// production use DialViaLibvirt.
func Dial(sockPath string) (*Client, error) {
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("dialing QMP socket %s: %w", sockPath, err)
	}
	raw, err := newRawQMP(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return &Client{
		execFn: func(ctx context.Context, command string, args any) (json.RawMessage, error) {
			if dl, ok := ctx.Deadline(); ok {
				conn.SetDeadline(dl)
				defer conn.SetDeadline(time.Time{})
			}
			return raw.execute(command, args)
		},
		closeFn: conn.Close,
	}, nil
}

// SetHistogramBoundaries arms the latency histogram for a block device.
// id is the QMP device identifier: either the "device" field from
// QueryBlockStats when non-empty, or the "qdev" path (e.g.
// "/machine/peripheral/ua-rootdisk/virtio-backend") which QEMU also accepts
// as the id parameter.  Calling this more than once resets the counters; the
// collector tracks which devices have been initialised and calls this once.
// ctx is used to enforce a deadline on the underlying socket write+read.
func (c *Client) SetHistogramBoundaries(ctx context.Context, id string, boundaries []int64) error {
	_, err := c.execFn(ctx, "block-latency-histogram-set", map[string]any{
		"id":         id,
		"boundaries": boundaries,
	})
	return err
}

// QueryBlockStats returns the current block statistics for all devices.
// ctx is used to enforce a deadline on the underlying socket write+read.
func (c *Client) QueryBlockStats(ctx context.Context) (BlockStatsList, error) {
	ret, err := c.execFn(ctx, "query-blockstats", nil)
	if err != nil {
		return nil, err
	}
	var stats BlockStatsList
	if err := json.Unmarshal(ret, &stats); err != nil {
		return nil, fmt.Errorf("parsing query-blockstats: %w", err)
	}
	return stats, nil
}

// --- libvirt transport ---

func execViaLibvirt(lv *libvirt.Libvirt, dom libvirt.Domain, command string, args any) (json.RawMessage, error) {
	cmdJSON, err := json.Marshal(qmpCommand{Execute: command, Arguments: args})
	if err != nil {
		return nil, fmt.Errorf("marshalling %q: %w", command, err)
	}
	result, err := lv.QEMUDomainMonitorCommand(dom, string(cmdJSON), qmpFlag)
	if err != nil {
		return nil, fmt.Errorf("QMP %q via virtqemud: %w", command, err)
	}
	return parseQMPResponse(command, result)
}

// --- raw QMP transport (for tests) ---

type rawQMP struct {
	enc     *json.Encoder
	scanner *bufio.Scanner
}

func newRawQMP(conn net.Conn) (*rawQMP, error) {
	r := &rawQMP{
		enc:     json.NewEncoder(conn),
		scanner: bufio.NewScanner(conn),
	}
	// Read and discard the QMP greeting {"QMP": {...}}.
	if !r.scanner.Scan() {
		return nil, fmt.Errorf("reading QMP greeting: %w", r.scanner.Err())
	}
	// Exit capability-negotiation mode.
	if err := r.enc.Encode(qmpCommand{Execute: "qmp_capabilities"}); err != nil {
		return nil, fmt.Errorf("sending qmp_capabilities: %w", err)
	}
	// Read the {"return":{}} acknowledgement.
	if !r.scanner.Scan() {
		return nil, fmt.Errorf("reading qmp_capabilities response: %w", r.scanner.Err())
	}
	return r, nil
}

func (r *rawQMP) execute(command string, args any) (json.RawMessage, error) {
	if err := r.enc.Encode(qmpCommand{Execute: command, Arguments: args}); err != nil {
		return nil, fmt.Errorf("writing QMP command %q: %w", command, err)
	}
	// Read lines, skipping asynchronous events, until return or error.
	for r.scanner.Scan() {
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(r.scanner.Bytes(), &msg); err != nil {
			return nil, fmt.Errorf("parsing QMP response for %q: %w", command, err)
		}
		if _, isEvent := msg["event"]; isEvent {
			continue
		}
		return parseQMPResponse(command, string(r.scanner.Bytes()))
	}
	if err := r.scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading QMP response for %q: %w", command, err)
	}
	return nil, fmt.Errorf("QMP connection closed while waiting for response to %q", command)
}

// --- shared helpers ---

type qmpCommand struct {
	Execute   string `json:"execute"`
	Arguments any    `json:"arguments,omitempty"`
}

// parseQMPResponse extracts the "return" field from a QMP response JSON string,
// or returns an error if the response contains an "error" field.
func parseQMPResponse(command, result string) (json.RawMessage, error) {
	var msg map[string]json.RawMessage
	if err := json.Unmarshal([]byte(result), &msg); err != nil {
		return nil, fmt.Errorf("parsing QMP response for %q (%s): %w", command, result, err)
	}
	if errRaw, hasErr := msg["error"]; hasErr {
		var qmpErr struct {
			Class string `json:"class"`
			Desc  string `json:"desc"`
		}
		_ = json.Unmarshal(errRaw, &qmpErr)
		return nil, fmt.Errorf("QMP error [%s]: %s", qmpErr.Class, qmpErr.Desc)
	}
	ret, ok := msg["return"]
	if !ok {
		return nil, fmt.Errorf("unexpected QMP response for %q: no 'return' field in %s", command, result)
	}
	return ret, nil
}
