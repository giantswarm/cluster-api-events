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

import (
	"testing"
)

func TestDetermineUpgradeType(t *testing.T) {
	tests := []struct {
		name        string
		fromVersion string
		toVersion   string
		expected    UpgradeType
	}{
		// Patch upgrades - nodes unlikely to roll
		{
			name:        "patch upgrade 33.1.1 to 33.1.2",
			fromVersion: "33.1.1",
			toVersion:   "33.1.2",
			expected:    UpgradeTypePatch,
		},
		{
			name:        "patch upgrade 33.1.0 to 33.1.10",
			fromVersion: "33.1.0",
			toVersion:   "33.1.10",
			expected:    UpgradeTypePatch,
		},
		{
			name:        "same version (no-op)",
			fromVersion: "33.1.2",
			toVersion:   "33.1.2",
			expected:    UpgradeTypePatch, // Technically patch with 0 difference
		},

		// Minor upgrades - nodes will roll
		{
			name:        "minor upgrade 33.1.2 to 33.2.0",
			fromVersion: "33.1.2",
			toVersion:   "33.2.0",
			expected:    UpgradeTypeMinor,
		},
		{
			name:        "minor upgrade 33.0.0 to 33.1.0",
			fromVersion: "33.0.0",
			toVersion:   "33.1.0",
			expected:    UpgradeTypeMinor,
		},

		// Major upgrades - nodes will roll
		{
			name:        "major upgrade 33.1.2 to 34.0.0",
			fromVersion: "33.1.2",
			toVersion:   "34.0.0",
			expected:    UpgradeTypeMajor,
		},
		{
			name:        "major upgrade 32.5.3 to 33.0.0",
			fromVersion: "32.5.3",
			toVersion:   "33.0.0",
			expected:    UpgradeTypeMajor,
		},

		// With v prefix
		{
			name:        "patch upgrade with v prefix",
			fromVersion: "v33.1.1",
			toVersion:   "v33.1.2",
			expected:    UpgradeTypePatch,
		},
		{
			name:        "minor upgrade with v prefix",
			fromVersion: "v33.1.0",
			toVersion:   "v33.2.0",
			expected:    UpgradeTypeMinor,
		},
		{
			name:        "mixed v prefix",
			fromVersion: "33.1.1",
			toVersion:   "v33.1.2",
			expected:    UpgradeTypePatch,
		},

		// Invalid versions
		{
			name:        "invalid from version",
			fromVersion: "invalid",
			toVersion:   "33.1.2",
			expected:    UpgradeTypeUnknown,
		},
		{
			name:        "invalid to version",
			fromVersion: "33.1.1",
			toVersion:   "invalid",
			expected:    UpgradeTypeUnknown,
		},
		{
			name:        "empty versions",
			fromVersion: "",
			toVersion:   "",
			expected:    UpgradeTypeUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := determineUpgradeType(tt.fromVersion, tt.toVersion)
			if result != tt.expected {
				t.Errorf("determineUpgradeType(%q, %q) = %v, want %v",
					tt.fromVersion, tt.toVersion, result, tt.expected)
			}
		})
	}
}
