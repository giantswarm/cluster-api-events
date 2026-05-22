/*
Copyright 2026.

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

package controller

import "testing"

func TestParseFlatcarImageLookupFormat(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantFlatcar string
		wantKube    string
		wantOK      bool
	}{
		{
			name:        "stable x86_64 (no arch infix)",
			input:       "flatcar-stable-4459.2.1-kube-1.33.6-tooling-1.26.2-gs",
			wantFlatcar: "4459.2.1",
			wantKube:    "1.33.6",
			wantOK:      true,
		},
		{
			name:        "stable arm64",
			input:       "flatcar-stable-4459.2.1-kube-1.33.6-tooling-1.26.2-arm64-gs",
			wantFlatcar: "4459.2.1",
			wantKube:    "1.33.6",
			wantOK:      true,
		},
		{
			name:   "empty string",
			input:  "",
			wantOK: false,
		},
		{
			name:   "missing -gs suffix",
			input:  "flatcar-stable-4459.2.1-kube-1.33.6-tooling-1.26.2",
			wantOK: false,
		},
		{
			name:   "missing tooling segment",
			input:  "flatcar-stable-4459.2.1-kube-1.33.6-gs",
			wantOK: false,
		},
		{
			name:   "non-numeric flatcar version",
			input:  "flatcar-stable-foo.bar.baz-kube-1.33.6-tooling-1.26.2-gs",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotFlatcar, gotKube, gotOK := parseFlatcarImageLookupFormat(tt.input)
			if gotOK != tt.wantOK {
				t.Fatalf("ok = %v, want %v", gotOK, tt.wantOK)
			}
			if !gotOK {
				return
			}
			if gotFlatcar != tt.wantFlatcar {
				t.Errorf("flatcarVersion = %q, want %q", gotFlatcar, tt.wantFlatcar)
			}
			if gotKube != tt.wantKube {
				t.Errorf("kubeVersion = %q, want %q", gotKube, tt.wantKube)
			}
		})
	}
}

func TestNormalizeKubeletVersion(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", ""},
		{"v1.33.6", "v1.33.6"},
		{"1.33.6", "v1.33.6"},
	}
	for _, tt := range tests {
		if got := normalizeKubeletVersion(tt.in); got != tt.want {
			t.Errorf("normalizeKubeletVersion(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestUpgradingEventReason(t *testing.T) {
	tests := []struct {
		name        string
		upgradeType UpgradeType
		want        string
	}{
		{"patch", UpgradeTypePatch, "UpgradingWithoutNodeRoll"},
		{"minor (CP rolls even if workers don't)", UpgradeTypeMinor, "UpgradingWithNodeRoll"},
		{"major", UpgradeTypeMajor, "UpgradingWithNodeRoll"},
		{"unknown defaults to with node roll", UpgradeTypeUnknown, "UpgradingWithNodeRoll"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := upgradingEventReason(tt.upgradeType); got != tt.want {
				t.Errorf("upgradingEventReason(%v) = %q, want %q", tt.upgradeType, got, tt.want)
			}
		})
	}
}
