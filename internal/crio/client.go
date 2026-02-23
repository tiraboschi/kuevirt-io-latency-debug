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

// Package crio provides a thin wrapper around the CRI gRPC API for
// discovering virt-launcher compute containers running on the local node.
package crio

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	runtimev1 "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// ContainerMeta holds the information the collector needs for each running VMI.
type ContainerMeta struct {
	ContainerID string
	VMIName     string
	Namespace   string
	// PID is the host-namespace PID of the container's init process.
	// Used to reach the container's filesystem via /proc/<PID>/root/…
	PID int
}

// Client talks to the local CRI-O daemon via its gRPC socket.
type Client struct {
	rc runtimev1.RuntimeServiceClient
}

// NewClient dials the CRI-O socket and returns a ready Client.
func NewClient(socketPath string) (*Client, error) {
	conn, err := grpc.NewClient(
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("dialing CRI-O socket %s: %w", socketPath, err)
	}
	return &Client{rc: runtimev1.NewRuntimeServiceClient(conn)}, nil
}

// ListComputeContainers returns all running virt-launcher compute containers on
// this node, optionally restricted to the supplied namespaces.
// labelSelector filters on pod labels that CRI-O propagates to container labels
// (e.g. {"environment": "production"}); an empty map means no filtering.
func (c *Client) ListComputeContainers(
	ctx context.Context,
	namespaces []string,
	labelSelector map[string]string,
) ([]ContainerMeta, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	resp, err := c.rc.ListContainers(ctx, &runtimev1.ListContainersRequest{
		Filter: &runtimev1.ContainerFilter{
			State: &runtimev1.ContainerStateValue{
				State: runtimev1.ContainerState_CONTAINER_RUNNING,
			},
			LabelSelector: map[string]string{
				// CRI-O only propagates standard Kubernetes container labels,
				// not pod labels (e.g. kubevirt.io=virt-launcher is absent).
				// Filtering on container name is sufficient; non-virt-launcher
				// containers will fail gracefully at QMP socket discovery.
				"io.kubernetes.container.name": "compute",
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("ListContainers: %w", err)
	}

	var result []ContainerMeta
	for _, ctr := range resp.Containers {
		labels := ctr.GetLabels()

		ns := labels["io.kubernetes.pod.namespace"]
		if len(namespaces) > 0 && !containsStr(namespaces, ns) {
			continue
		}
		if !matchesSelector(labels, labelSelector) {
			continue
		}

		pid, err := c.pidForContainer(ctx, ctr.GetId())
		if err != nil {
			// The container may be starting or stopping; skip and retry next cycle.
			continue
		}

		// Pod labels (kubevirt.io/domain, vm.kubevirt.io/name) are not propagated
		// to CRI-O container labels. Derive the VMI name from the pod name instead:
		// virt-launcher-<vmi-name>-<5-char-random-suffix>
		vmiName := vmiNameFromPodName(labels["io.kubernetes.pod.name"])

		result = append(result, ContainerMeta{
			ContainerID: ctr.GetId(),
			VMIName:     vmiName,
			Namespace:   ns,
			PID:         pid,
		})
	}
	return result, nil
}

// pidForContainer retrieves the host-namespace PID of the container's init
// process from CRI-O's verbose ContainerStatus response.
//
// CRI-O stores extended info as a JSON blob under info["info"]:
//
//	{"pid": 12345, "runtimeSpec": {…}, …}
func (c *Client) pidForContainer(ctx context.Context, containerID string) (int, error) {
	resp, err := c.rc.ContainerStatus(ctx, &runtimev1.ContainerStatusRequest{
		ContainerId: containerID,
		Verbose:     true,
	})
	if err != nil {
		return 0, fmt.Errorf("ContainerStatus %s: %w", containerID[:12], err)
	}
	return parsePID(resp.GetInfo())
}

// parsePID extracts the host PID from the CRI-O-specific verbose info map.
func parsePID(info map[string]string) (int, error) {
	raw, ok := info["info"]
	if !ok {
		return 0, fmt.Errorf("no 'info' key in ContainerStatus.Info")
	}
	var parsed struct {
		PID int `json:"pid"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return 0, fmt.Errorf("parsing container info JSON: %w", err)
	}
	if parsed.PID <= 0 {
		return 0, fmt.Errorf("unexpected PID %d in container info", parsed.PID)
	}
	return parsed.PID, nil
}

func containsStr(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

// matchesSelector returns true if every key=value pair in selector is present
// in labels.  An empty selector always matches.
func matchesSelector(labels, selector map[string]string) bool {
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// vmiNameFromPodName extracts the VMI name from a virt-launcher pod name.
// KubeVirt pod names follow the pattern: virt-launcher-<vmi-name>-<5-char-suffix>
// Returns an empty string if the name doesn't match the expected pattern.
func vmiNameFromPodName(podName string) string {
	const prefix = "virt-launcher-"
	if !strings.HasPrefix(podName, prefix) {
		return ""
	}
	withoutPrefix := podName[len(prefix):]
	idx := strings.LastIndex(withoutPrefix, "-")
	if idx <= 0 {
		return ""
	}
	return withoutPrefix[:idx]
}
