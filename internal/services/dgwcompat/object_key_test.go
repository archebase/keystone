// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package dgwcompat

import "testing"

func TestBuildObjectKeySanitizesSegments(t *testing.T) {
	hints := map[string]string{
		"device_id":  "robot/001",
		"capture_id": "capture:alpha",
	}

	got := buildObjectKey("ego-portal-lite", hints, "upload-123")
	want := "ego-portal-lite/robot_001/capture_alpha/upload-123/capture.mcap"
	if got != want {
		t.Fatalf("buildObjectKey() = %q, want %q", got, want)
	}
}

func TestBuildObjectKeyUsesFallbacks(t *testing.T) {
	got := buildObjectKey("", nil, "upload-123")
	want := "ego-portal-lite/unknown-device/unknown-capture/upload-123/capture.mcap"
	if got != want {
		t.Fatalf("buildObjectKey() = %q, want %q", got, want)
	}
}
