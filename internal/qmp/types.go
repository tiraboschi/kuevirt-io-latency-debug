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

// BlockStatsList is the top-level return value of query-blockstats.
type BlockStatsList []BlockStat

// BlockStat represents a single block device entry.
type BlockStat struct {
	Device string      `json:"device"`
	QDev   string      `json:"qdev"`
	Stats  DeviceStats `json:"stats"`
}

// DeviceStats carries I/O counters and optional latency histograms.
// QEMU has used both stable ("rd_latency_histogram") and experimental
// ("x-rd-latency-histogram") field names across versions; both are decoded.
type DeviceStats struct {
	RdBytes          int64 `json:"rd_bytes"`
	RdOperations     int64 `json:"rd_operations"`
	RdTotalTimeNs    int64 `json:"rd_total_time_ns"`
	WrBytes          int64 `json:"wr_bytes"`
	WrOperations     int64 `json:"wr_operations"`
	WrTotalTimeNs    int64 `json:"wr_total_time_ns"`
	FlushOperations  int64 `json:"flush_operations"`
	FlushTotalTimeNs int64 `json:"flush_total_time_ns"`

	// Stable naming (QEMU 7+).
	RdLatencyHistogram    *LatencyHistogram `json:"rd_latency_histogram"`
	WrLatencyHistogram    *LatencyHistogram `json:"wr_latency_histogram"`
	FlushLatencyHistogram *LatencyHistogram `json:"flush_latency_histogram"`

	// Experimental naming used in older QEMU builds (x- prefix, hyphen-separated).
	XRdLatencyHistogram    *LatencyHistogram `json:"x-rd-latency-histogram"`
	XWrLatencyHistogram    *LatencyHistogram `json:"x-wr-latency-histogram"`
	XFlushLatencyHistogram *LatencyHistogram `json:"x-flush-latency-histogram"`
}

func (s *DeviceStats) ReadHistogram() *LatencyHistogram {
	if s.RdLatencyHistogram != nil {
		return s.RdLatencyHistogram
	}
	return s.XRdLatencyHistogram
}

func (s *DeviceStats) WriteHistogram() *LatencyHistogram {
	if s.WrLatencyHistogram != nil {
		return s.WrLatencyHistogram
	}
	return s.XWrLatencyHistogram
}

func (s *DeviceStats) FlushHistogram() *LatencyHistogram {
	if s.FlushLatencyHistogram != nil {
		return s.FlushLatencyHistogram
	}
	return s.XFlushLatencyHistogram
}

// LatencyHistogram holds QEMU per-bucket counts (non-cumulative) and the
// corresponding boundary values in nanoseconds.
//
// QEMU layout: len(Boundaries) == N, len(Bins) == N+1.
// Bin[i] counts observations in (Boundaries[i-1], Boundaries[i]], with
// Bin[0] covering [0, Boundaries[0]] and Bin[N] the overflow bucket.
type LatencyHistogram struct {
	Boundaries []int64 `json:"boundaries"`
	Bins       []int64 `json:"bins"`
}

// CumulativeBins converts QEMU's per-bucket counts into the cumulative counts
// required by Prometheus histogram semantics, where each bucket with label
// le="X" must contain all observations with value ≤ X.
func (h *LatencyHistogram) CumulativeBins() []int64 {
	if h == nil || len(h.Bins) == 0 {
		return nil
	}
	out := make([]int64, len(h.Bins))
	var running int64
	for i, v := range h.Bins {
		running += v
		out[i] = running
	}
	return out
}
