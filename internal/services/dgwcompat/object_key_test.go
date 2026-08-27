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

func TestBuildObjectKeyUsesTarForEgoPortalE2(t *testing.T) {
	hints := map[string]string{"device_id": "robot-1", "capture_id": "capture-1"}
	got := buildObjectKey("device-uploads", hints, "upload-1", "Ego Portal E2")
	want := "device-uploads/robot-1/capture-1/upload-1/capture.tar"
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
