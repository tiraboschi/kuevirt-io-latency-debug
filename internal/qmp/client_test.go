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

package qmp

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
)

// --- fake QMP server ---

// fakeQMPError is the JSON structure QEMU returns for error responses.
type fakeQMPError struct {
	Class string `json:"class"`
	Desc  string `json:"desc"`
}

// commandHandler is called for each incoming QMP command.
// Returning a non-nil error causes the server to send a QMP error response.
type commandHandler func(cmd string, args json.RawMessage) (any, *fakeQMPError)

// newFakeQMPServer starts a fake QMP server on a temporary UNIX socket and
// returns the socket path.  The server runs until the test ends.
func newFakeQMPServer(t *testing.T, handler commandHandler) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "qemu.monitor")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go fakeAcceptLoop(ln, handler)
	return path
}

func fakeAcceptLoop(ln net.Listener, handler commandHandler) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go fakeServeConn(conn, handler)
	}
}

func fakeServeConn(conn net.Conn, handler commandHandler) {
	defer conn.Close()
	enc := json.NewEncoder(conn)
	scanner := bufio.NewScanner(conn)

	// Send the QMP greeting immediately after the connection is accepted.
	_ = enc.Encode(map[string]any{
		"QMP": map[string]any{
			"version":      map[string]any{"micro": 0, "minor": 2, "major": 9},
			"capabilities": []any{},
		},
	})

	for scanner.Scan() {
		var msg struct {
			Execute   string          `json:"execute"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			return
		}
		ret, qmpErr := handler(msg.Execute, msg.Arguments)
		if qmpErr != nil {
			_ = enc.Encode(map[string]any{"error": qmpErr})
		} else {
			_ = enc.Encode(map[string]any{"return": ret})
		}
	}
}

// defaultHandler handles the commands the client sends during normal operation.
func defaultHandler(cmd string, _ json.RawMessage) (any, *fakeQMPError) {
	switch cmd {
	case "query-blockstats":
		return fakeBlockStats(), nil
	}
	// qmp_capabilities, block-latency-histogram-set, and anything else → OK.
	return map[string]any{}, nil
}

// fakeBlockStats returns a single block device entry with realistic fields.
func fakeBlockStats() []map[string]any {
	return []map[string]any{
		{
			"device": "drive-ua-rootdisk",
			"qdev":   "/machine/peripheral/ua-rootdisk/virtio-backend",
			"stats": map[string]any{
				"rd_bytes":             int64(1024),
				"rd_operations":        int64(100),
				"rd_total_time_ns":     int64(50_000_000),
				"wr_bytes":             int64(2048),
				"wr_operations":        int64(50),
				"wr_total_time_ns":     int64(25_000_000),
				"flush_operations":     int64(10),
				"flush_total_time_ns":  int64(5_000_000),
				"rd_latency_histogram": map[string]any{"boundaries": []int64{1_000_000, 10_000_000}, "bins": []int64{80, 15, 5}},
				"wr_latency_histogram": map[string]any{"boundaries": []int64{1_000_000, 10_000_000}, "bins": []int64{40, 8, 2}},
				"flush_latency_histogram": map[string]any{
					"boundaries": []int64{1_000_000, 10_000_000},
					"bins":       []int64{9, 1, 0},
				},
			},
		},
	}
}

// --- tests ---

func TestDial(t *testing.T) {
	path := newFakeQMPServer(t, defaultHandler)
	c, err := Dial(path)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	c.Close()
}

func TestDialNonExistentSocket(t *testing.T) {
	_, err := Dial("/tmp/does-not-exist/qemu.monitor")
	if err == nil {
		t.Fatal("expected an error for a non-existent socket path")
	}
}

func TestQueryBlockStats(t *testing.T) {
	path := newFakeQMPServer(t, defaultHandler)
	c, err := Dial(path)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	stats, err := c.QueryBlockStats()
	if err != nil {
		t.Fatalf("QueryBlockStats: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("expected 1 device, got %d", len(stats))
	}

	bs := stats[0]
	if bs.Device != "drive-ua-rootdisk" {
		t.Errorf("device: got %q, want %q", bs.Device, "drive-ua-rootdisk")
	}
	if bs.Stats.RdOperations != 100 {
		t.Errorf("rd_operations: got %d, want 100", bs.Stats.RdOperations)
	}
	if bs.Stats.RdTotalTimeNs != 50_000_000 {
		t.Errorf("rd_total_time_ns: got %d, want 50000000", bs.Stats.RdTotalTimeNs)
	}

	hist := bs.Stats.ReadHistogram()
	if hist == nil {
		t.Fatal("expected read histogram, got nil")
	}
	if len(hist.Bins) != 3 {
		t.Errorf("histogram bins: got %d, want 3", len(hist.Bins))
	}
	if hist.Bins[0] != 80 || hist.Bins[1] != 15 || hist.Bins[2] != 5 {
		t.Errorf("histogram bins: got %v, want [80 15 5]", hist.Bins)
	}
}

func TestSetHistogramBoundaries(t *testing.T) {
	var capturedID string
	var capturedBoundaries []int64

	handler := func(cmd string, args json.RawMessage) (any, *fakeQMPError) {
		if cmd == "block-latency-histogram-set" {
			var a struct {
				ID         string  `json:"id"`
				Boundaries []int64 `json:"boundaries"`
			}
			_ = json.Unmarshal(args, &a)
			capturedID = a.ID
			capturedBoundaries = a.Boundaries
		}
		return defaultHandler(cmd, args)
	}

	path := newFakeQMPServer(t, handler)
	c, err := Dial(path)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	want := []int64{1_000_000, 10_000_000, 100_000_000}
	if err := c.SetHistogramBoundaries("drive-ua-rootdisk", want); err != nil {
		t.Fatalf("SetHistogramBoundaries: %v", err)
	}
	if capturedID != "drive-ua-rootdisk" {
		t.Errorf("id: got %q, want %q", capturedID, "drive-ua-rootdisk")
	}
	if len(capturedBoundaries) != len(want) {
		t.Errorf("boundaries length: got %d, want %d", len(capturedBoundaries), len(want))
	}
}

func TestQMPErrorResponse(t *testing.T) {
	handler := func(cmd string, args json.RawMessage) (any, *fakeQMPError) {
		if cmd == "query-blockstats" {
			return nil, &fakeQMPError{Class: "CommandNotFound", Desc: "unknown command"}
		}
		return defaultHandler(cmd, args)
	}

	path := newFakeQMPServer(t, handler)
	c, err := Dial(path)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	_, err = c.QueryBlockStats()
	if err == nil {
		t.Fatal("expected error from QMP error response, got nil")
	}
}

// TestSkipsAsyncEvents verifies that asynchronous QMP events arriving before
// the response to a command are silently discarded.
func TestSkipsAsyncEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.monitor")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		enc := json.NewEncoder(conn)
		scanner := bufio.NewScanner(conn)

		// Greeting.
		_ = enc.Encode(map[string]any{
			"QMP": map[string]any{
				"version":      map[string]any{"micro": 0, "minor": 2, "major": 9},
				"capabilities": []any{},
			},
		})

		for scanner.Scan() {
			var msg struct {
				Execute string `json:"execute"`
			}
			_ = json.Unmarshal(scanner.Bytes(), &msg)

			if msg.Execute == "qmp_capabilities" {
				_ = enc.Encode(map[string]any{"return": map[string]any{}})
				continue
			}

			// Send an async event before the actual command response.
			_ = enc.Encode(map[string]any{
				"event":     "BLOCK_IO_ERROR",
				"data":      map[string]any{},
				"timestamp": map[string]any{"seconds": int64(1234567890), "microseconds": int64(0)},
			})
			// Now send the real response.
			_ = enc.Encode(map[string]any{"return": fakeBlockStats()})
		}
	}()

	c, err := Dial(path)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	stats, err := c.QueryBlockStats()
	if err != nil {
		t.Fatalf("QueryBlockStats after async event: %v", err)
	}
	if len(stats) == 0 {
		t.Error("expected stats despite preceding async event")
	}
}
