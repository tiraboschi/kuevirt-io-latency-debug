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

package main

import (
	"reflect"
	"testing"
)

func TestSplitTrimmed(t *testing.T) {
	tests := []struct {
		input string
		sep   string
		want  []string
	}{
		{input: "", sep: ",", want: nil},
		{input: "a,b,c", sep: ",", want: []string{"a", "b", "c"}},
		{input: " a , b , c ", sep: ",", want: []string{"a", "b", "c"}},
		{input: "a,,b", sep: ",", want: []string{"a", "b"}}, // empty parts dropped
		{input: "only", sep: ",", want: []string{"only"}},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := splitTrimmed(tc.input, tc.sep)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("splitTrimmed(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

func TestParseLabelFilter(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want map[string]string
	}{
		{
			name: "empty string",
			raw:  "",
			want: map[string]string{},
		},
		{
			name: "single pair",
			raw:  "environment=production",
			want: map[string]string{"environment": "production"},
		},
		{
			name: "multiple pairs",
			raw:  "environment=production,tier=frontend",
			want: map[string]string{"environment": "production", "tier": "frontend"},
		},
		{
			name: "whitespace around separator",
			raw:  " environment = production , tier = frontend ",
			want: map[string]string{"environment": "production", "tier": "frontend"},
		},
		{
			name: "value with equals sign",
			raw:  "label=foo=bar",
			want: map[string]string{"label": "foo=bar"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseLabelFilter(tc.raw)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseLabelFilter(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestParseBoundaries(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    []int64
		wantErr bool
	}{
		{
			name: "raw nanosecond integers",
			raw:  "1000000,10000000,100000000",
			want: []int64{1_000_000, 10_000_000, 100_000_000},
		},
		{
			name: "go duration syntax",
			raw:  "1ms,10ms,100ms,1s",
			want: []int64{1_000_000, 10_000_000, 100_000_000, 1_000_000_000},
		},
		{
			name: "mixed integers and durations",
			raw:  "1000000,10ms",
			want: []int64{1_000_000, 10_000_000},
		},
		{
			name:    "invalid value",
			raw:     "1ms,notanumber",
			wantErr: true,
		},
		{
			name: "single boundary",
			raw:  "500000",
			want: []int64{500_000},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseBoundaries(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseBoundaries(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}
