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

package collector

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/kubevirt/kuevirt-io-latency-debug/internal/qmp"
)

// --- fake QMP server (minimal, reused from qmp package tests) ---

func newFakeServer(t *testing.T, handler func(cmd string) any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "qemu.monitor")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveConn(conn, handler)
		}
	}()
	return path
}

func serveConn(conn net.Conn, handler func(cmd string) any) {
	defer conn.Close()
	enc := json.NewEncoder(conn)
	scanner := bufio.NewScanner(conn)
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
		_ = enc.Encode(map[string]any{"return": handler(msg.Execute)})
	}
}

// blockStatsResponse returns test data with a single disk and full histogram.
func blockStatsResponse() []map[string]any {
	return []map[string]any{
		{
			"device": "drive-ua-rootdisk",
			"qdev":   "/machine/peripheral/ua-rootdisk/virtio-backend",
			"stats": map[string]any{
				"rd_operations":        int64(100),
				"rd_total_time_ns":     int64(50_000_000),
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

// --- metric helpers ---

// gatherMetrics registers col in a fresh registry, calls Gather, and returns
// the metric families indexed by name.
func gatherMetrics(t *testing.T, col *Collector) map[string][]*dto.Metric {
	t.Helper()
	reg := prometheus.NewRegistry()
	if err := reg.Register(col); err != nil {
		t.Fatalf("register collector: %v", err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	result := make(map[string][]*dto.Metric, len(mfs))
	for _, mf := range mfs {
		result[mf.GetName()] = mf.GetMetric()
	}
	return result
}

// labelVal returns the value of label name in m, or "" if absent.
func labelVal(m *dto.Metric, name string) string {
	for _, lp := range m.GetLabel() {
		if lp.GetName() == name {
			return lp.GetValue()
		}
	}
	return ""
}

// findMetric returns the first metric in the slice where all supplied
// key=value label pairs match, or nil if none is found.
func findMetric(metrics []*dto.Metric, labels map[string]string) *dto.Metric {
	for _, m := range metrics {
		match := true
		for k, v := range labels {
			if labelVal(m, k) != v {
				match = false
				break
			}
		}
		if match {
			return m
		}
	}
	return nil
}

// --- tests ---

func TestExtractDiskAlias(t *testing.T) {
	tests := []struct {
		name      string
		device    string
		qdev      string
		wantAlias string
		wantOK    bool
	}{
		// Standard cases — qdev is the primary source.
		{
			name:      "virtio-blk backend",
			device:    "drive-ua-rootdisk",
			qdev:      "/machine/peripheral/ua-rootdisk/virtio-backend",
			wantAlias: "rootdisk",
			wantOK:    true,
		},
		{
			name:      "alias with hyphens",
			device:    "drive-ua-data-disk",
			qdev:      "/machine/peripheral/ua-data-disk/virtio-backend",
			wantAlias: "data-disk",
			wantOK:    true,
		},
		{
			name:      "scsi backend variant",
			device:    "drive-ua-disk0",
			qdev:      "/machine/peripheral/ua-disk0/scsi-disk0",
			wantAlias: "disk0",
			wantOK:    true,
		},
		{
			name:      "cloud-init disk",
			device:    "drive-ua-cloudinitdisk",
			qdev:      "/machine/peripheral/ua-cloudinitdisk/virtio-backend",
			wantAlias: "cloudinitdisk",
			wantOK:    true,
		},
		// qdev wins over device when both are present.
		{
			name:      "qdev takes precedence over device",
			device:    "drive-ua-device-name",
			qdev:      "/machine/peripheral/ua-qdev-name/virtio-backend",
			wantAlias: "qdev-name",
			wantOK:    true,
		},
		// Fallback to device field when qdev is empty or too short.
		{
			name:      "empty qdev falls back to drive-ua prefix",
			device:    "drive-ua-rootdisk",
			qdev:      "",
			wantAlias: "rootdisk",
			wantOK:    true,
		},
		{
			name:      "empty qdev falls back to ua prefix",
			device:    "ua-rootdisk",
			qdev:      "",
			wantAlias: "rootdisk",
			wantOK:    true,
		},
		{
			name:      "short qdev (no backend component) falls back to device",
			device:    "drive-ua-disk",
			qdev:      "/machine/peripheral/ua-disk", // only 4 path components, not 5
			wantAlias: "disk",
			wantOK:    true,
		},
		// Non-KubeVirt devices — must return false.
		{
			name:   "firmware pflash",
			device: "pflash0",
			qdev:   "",
			wantOK: false,
		},
		{
			name:   "IDE CDROM",
			device: "ide0-cd0",
			qdev:   "",
			wantOK: false,
		},
		{
			name:   "qdev without ua prefix",
			device: "floppy0",
			qdev:   "/machine/peripheral/floppy0/floppy",
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			alias, ok := extractDiskAlias(tc.device, tc.qdev)
			if ok != tc.wantOK {
				t.Fatalf("ok: got %v, want %v", ok, tc.wantOK)
			}
			if ok && alias != tc.wantAlias {
				t.Errorf("alias: got %q, want %q", alias, tc.wantAlias)
			}
		})
	}
}

// TestCollectMetrics is an end-to-end test that starts a fake QMP server,
// injects a domain into the collector, and verifies the emitted Prometheus
// metrics via a registry Gather call.
func TestCollectMetrics(t *testing.T) {
	socketPath := newFakeServer(t, func(cmd string) any {
		if cmd == "query-blockstats" {
			return blockStatsResponse()
		}
		return map[string]any{}
	})

	qmpClient, err := qmp.Dial(socketPath)
	if err != nil {
		t.Fatalf("qmp.Dial: %v", err)
	}
	t.Cleanup(func() { qmpClient.Close() })

	col := New(Config{
		NodeName:   "test-node",
		Boundaries: []int64{1_000_000, 10_000_000},
	})
	// Inject the domain directly (white-box).
	col.domains["test-container"] = &domain{
		vmiName:   "test-vm",
		namespace: "test-ns",
		qmpClient: qmpClient,
		armed:     make(map[string]bool),
	}

	metrics := gatherMetrics(t, col)

	// Expect three metric families.
	for _, name := range []string{
		"kubevirt_vmi_storage_latency_ns_bucket",
		"kubevirt_vmi_storage_latency_ns_count",
		"kubevirt_vmi_storage_latency_ns_sum",
	} {
		if _, ok := metrics[name]; !ok {
			t.Errorf("metric family %q not found in output", name)
		}
	}

	// 3 operations × 3 buckets (le=1ms, le=10ms, +Inf) = 9 bucket metrics.
	if got := len(metrics["kubevirt_vmi_storage_latency_ns_bucket"]); got != 9 {
		t.Errorf("bucket metrics: got %d, want 9", got)
	}
	// 3 operations × 1 count each = 3 count metrics.
	if got := len(metrics["kubevirt_vmi_storage_latency_ns_count"]); got != 3 {
		t.Errorf("count metrics: got %d, want 3", got)
	}

	// Spot-check: read count = 100.
	rdCount := findMetric(metrics["kubevirt_vmi_storage_latency_ns_count"], map[string]string{
		"vmi": "test-vm", "namespace": "test-ns", "drive": "rootdisk", "operation": "read",
	})
	if rdCount == nil {
		t.Fatal("read count metric not found")
	}
	if v := rdCount.GetCounter().GetValue(); v != 100 {
		t.Errorf("read count: got %.0f, want 100", v)
	}

	// Spot-check: read +Inf bucket must equal read count (invariant).
	rdInf := findMetric(metrics["kubevirt_vmi_storage_latency_ns_bucket"], map[string]string{
		"vmi": "test-vm", "namespace": "test-ns", "drive": "rootdisk", "operation": "read", "le": "+Inf",
	})
	if rdInf == nil {
		t.Fatal("+Inf read bucket not found")
	}
	if v := rdInf.GetCounter().GetValue(); v != 100 {
		t.Errorf("+Inf bucket: got %.0f, want 100 (must equal count)", v)
	}

	// Spot-check: read le=1ms bucket = first bin = 80 (cumulative[0]).
	rdLe1ms := findMetric(metrics["kubevirt_vmi_storage_latency_ns_bucket"], map[string]string{
		"vmi": "test-vm", "namespace": "test-ns", "drive": "rootdisk", "operation": "read", "le": "1000000",
	})
	if rdLe1ms == nil {
		t.Fatal("read le=1000000 bucket not found")
	}
	if v := rdLe1ms.GetCounter().GetValue(); v != 80 {
		t.Errorf("le=1ms bucket: got %.0f, want 80", v)
	}

	// Spot-check: read le=10ms bucket = cumulative[1] = 80+15 = 95.
	rdLe10ms := findMetric(metrics["kubevirt_vmi_storage_latency_ns_bucket"], map[string]string{
		"vmi": "test-vm", "namespace": "test-ns", "drive": "rootdisk", "operation": "read", "le": "10000000",
	})
	if rdLe10ms == nil {
		t.Fatal("read le=10000000 bucket not found")
	}
	if v := rdLe10ms.GetCounter().GetValue(); v != 95 {
		t.Errorf("le=10ms bucket: got %.0f, want 95", v)
	}

	// Spot-check: write sum.
	wrSum := findMetric(metrics["kubevirt_vmi_storage_latency_ns_sum"], map[string]string{
		"vmi": "test-vm", "namespace": "test-ns", "drive": "rootdisk", "operation": "write",
	})
	if wrSum == nil {
		t.Fatal("write sum metric not found")
	}
	if v := wrSum.GetCounter().GetValue(); v != 25_000_000 {
		t.Errorf("write sum: got %.0f, want 25000000", v)
	}

	// Verify constant node label is present on all metrics.
	for name, ms := range metrics {
		for _, m := range ms {
			if labelVal(m, "node") != "test-node" {
				t.Errorf("%s: node label: got %q, want %q", name, labelVal(m, "node"), "test-node")
			}
		}
	}
}

// TestCollectSkipsNonKubeVirtDevices verifies that QEMU devices without the
// "ua-" prefix are not exposed as metrics.
func TestCollectSkipsNonKubeVirtDevices(t *testing.T) {
	socketPath := newFakeServer(t, func(cmd string) any {
		if cmd == "query-blockstats" {
			return []map[string]any{
				// Non-KubeVirt device: should be ignored.
				{"device": "pflash0", "qdev": "", "stats": map[string]any{
					"rd_operations": int64(1), "rd_total_time_ns": int64(100),
					"wr_operations": int64(0), "wr_total_time_ns": int64(0),
					"flush_operations": int64(0), "flush_total_time_ns": int64(0),
				}},
			}
		}
		return map[string]any{}
	})

	qmpClient, err := qmp.Dial(socketPath)
	if err != nil {
		t.Fatalf("qmp.Dial: %v", err)
	}
	t.Cleanup(func() { qmpClient.Close() })

	col := New(Config{NodeName: "n", Boundaries: []int64{1_000_000}})
	col.domains["test"] = &domain{vmiName: "vm", namespace: "ns", qmpClient: qmpClient, armed: make(map[string]bool)}

	metrics := gatherMetrics(t, col)
	if len(metrics) != 0 {
		t.Errorf("expected no metrics for non-KubeVirt device, got %v", metrics)
	}
}

// TestCollectWithoutHistogram verifies that count and sum are emitted even
// when block-latency-histogram-set has not been called (no histogram data).
func TestCollectWithoutHistogram(t *testing.T) {
	socketPath := newFakeServer(t, func(cmd string) any {
		if cmd == "query-blockstats" {
			return []map[string]any{
				{
					"device": "drive-ua-rootdisk",
					"qdev":   "/machine/peripheral/ua-rootdisk/virtio-backend",
					"stats": map[string]any{
						"rd_operations": int64(42), "rd_total_time_ns": int64(1_000_000),
						"wr_operations": int64(0), "wr_total_time_ns": int64(0),
						"flush_operations": int64(0), "flush_total_time_ns": int64(0),
						// No histogram fields.
					},
				},
			}
		}
		return map[string]any{}
	})

	qmpClient, err := qmp.Dial(socketPath)
	if err != nil {
		t.Fatalf("qmp.Dial: %v", err)
	}
	t.Cleanup(func() { qmpClient.Close() })

	col := New(Config{NodeName: "n", Boundaries: []int64{1_000_000}})
	col.domains["test"] = &domain{vmiName: "vm", namespace: "ns", qmpClient: qmpClient, armed: make(map[string]bool)}

	metrics := gatherMetrics(t, col)

	// Buckets must be absent; count and sum must be present.
	if _, ok := metrics["kubevirt_vmi_storage_latency_ns_bucket"]; ok {
		t.Error("expected no bucket metrics when histogram is not armed")
	}
	if _, ok := metrics["kubevirt_vmi_storage_latency_ns_count"]; !ok {
		t.Error("expected count metrics even without histogram")
	}
}
