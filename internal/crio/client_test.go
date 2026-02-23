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

package crio

import (
	"fmt"
	"testing"
)

func TestParsePID(t *testing.T) {
	tests := []struct {
		name    string
		info    map[string]string
		wantPID int
		wantErr bool
	}{
		{
			name:    "valid pid",
			info:    map[string]string{"info": `{"pid": 12345}`},
			wantPID: 12345,
		},
		{
			name:    "pid with extra fields",
			info:    map[string]string{"info": `{"pid": 99, "runtimeSpec": {}, "sandboxID": "abc"}`},
			wantPID: 99,
		},
		{
			name:    "missing info key",
			info:    map[string]string{"other": "data"},
			wantErr: true,
		},
		{
			name:    "zero pid is rejected",
			info:    map[string]string{"info": `{"pid": 0}`},
			wantErr: true,
		},
		{
			name:    "negative pid is rejected",
			info:    map[string]string{"info": `{"pid": -1}`},
			wantErr: true,
		},
		{
			name:    "malformed json",
			info:    map[string]string{"info": `not-json`},
			wantErr: true,
		},
		{
			name:    "empty info value",
			info:    map[string]string{"info": ""},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePID(tc.info)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error, got PID %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantPID {
				t.Errorf("got PID %d, want %d", got, tc.wantPID)
			}
		})
	}
}

func TestMatchesSelector(t *testing.T) {
	labels := map[string]string{
		"environment":                 "production",
		"tier":                        "frontend",
		"io.kubernetes.pod.namespace": "default",
	}

	tests := []struct {
		name     string
		selector map[string]string
		want     bool
	}{
		{
			name:     "empty selector always matches",
			selector: map[string]string{},
			want:     true,
		},
		{
			name:     "nil selector always matches",
			selector: nil,
			want:     true,
		},
		{
			name:     "single matching label",
			selector: map[string]string{"environment": "production"},
			want:     true,
		},
		{
			name:     "multiple matching labels",
			selector: map[string]string{"environment": "production", "tier": "frontend"},
			want:     true,
		},
		{
			name:     "single non-matching label",
			selector: map[string]string{"environment": "staging"},
			want:     false,
		},
		{
			name:     "one match one miss",
			selector: map[string]string{"environment": "production", "tier": "backend"},
			want:     false,
		},
		{
			name:     "key absent from labels",
			selector: map[string]string{"missing-key": "value"},
			want:     false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchesSelector(labels, tc.selector); got != tc.want {
				t.Errorf("matchesSelector() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestContainsStr(t *testing.T) {
	tests := []struct {
		slice []string
		s     string
		want  bool
	}{
		{[]string{"a", "b", "c"}, "b", true},
		{[]string{"a", "b", "c"}, "d", false},
		{[]string{}, "a", false},
		{nil, "a", false},
		{[]string{"only"}, "only", true},
	}

	for _, tc := range tests {
		name := fmt.Sprintf("slice=%v s=%q", tc.slice, tc.s)
		t.Run(name, func(t *testing.T) {
			if got := containsStr(tc.slice, tc.s); got != tc.want {
				t.Errorf("containsStr() = %v, want %v", got, tc.want)
			}
		})
	}
}
