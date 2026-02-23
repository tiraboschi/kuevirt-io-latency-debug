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
	"reflect"
	"testing"
)

func TestCumulativeBins(t *testing.T) {
	tests := []struct {
		name string
		hist *LatencyHistogram
		want []int64
	}{
		{
			name: "nil histogram",
			hist: nil,
			want: nil,
		},
		{
			name: "empty bins",
			hist: &LatencyHistogram{Bins: []int64{}},
			want: nil,
		},
		{
			name: "single bin",
			hist: &LatencyHistogram{Bins: []int64{7}},
			want: []int64{7},
		},
		{
			name: "normal three bins",
			hist: &LatencyHistogram{Bins: []int64{3, 5, 2}},
			want: []int64{3, 8, 10},
		},
		{
			name: "bins with zeros",
			hist: &LatencyHistogram{Bins: []int64{0, 5, 0, 3}},
			want: []int64{0, 5, 5, 8},
		},
		{
			name: "all zero bins",
			hist: &LatencyHistogram{Bins: []int64{0, 0, 0}},
			want: []int64{0, 0, 0},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.hist.CumulativeBins()
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("CumulativeBins() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHistogramAccessors(t *testing.T) {
	stable := &LatencyHistogram{Bins: []int64{1}}
	experimental := &LatencyHistogram{Bins: []int64{2}}

	t.Run("ReadHistogram prefers stable field", func(t *testing.T) {
		s := DeviceStats{RdLatencyHistogram: stable, XRdLatencyHistogram: experimental}
		if s.ReadHistogram() != stable {
			t.Error("expected stable histogram, got experimental")
		}
	})
	t.Run("ReadHistogram falls back to experimental", func(t *testing.T) {
		s := DeviceStats{XRdLatencyHistogram: experimental}
		if s.ReadHistogram() != experimental {
			t.Error("expected experimental histogram, got nil")
		}
	})
	t.Run("ReadHistogram returns nil when both absent", func(t *testing.T) {
		if (&DeviceStats{}).ReadHistogram() != nil {
			t.Error("expected nil")
		}
	})

	t.Run("WriteHistogram prefers stable field", func(t *testing.T) {
		s := DeviceStats{WrLatencyHistogram: stable, XWrLatencyHistogram: experimental}
		if s.WriteHistogram() != stable {
			t.Error("expected stable histogram")
		}
	})
	t.Run("WriteHistogram falls back to experimental", func(t *testing.T) {
		s := DeviceStats{XWrLatencyHistogram: experimental}
		if s.WriteHistogram() != experimental {
			t.Error("expected experimental histogram")
		}
	})

	t.Run("FlushHistogram prefers stable field", func(t *testing.T) {
		s := DeviceStats{FlushLatencyHistogram: stable, XFlushLatencyHistogram: experimental}
		if s.FlushHistogram() != stable {
			t.Error("expected stable histogram")
		}
	})
	t.Run("FlushHistogram falls back to experimental", func(t *testing.T) {
		s := DeviceStats{XFlushLatencyHistogram: experimental}
		if s.FlushHistogram() != experimental {
			t.Error("expected experimental histogram")
		}
	})
}
