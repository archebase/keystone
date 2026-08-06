// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package dgwcompat

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const testCameraSerial = "CAMERA-SN-001"

func TestParseCalibrationUploadIntentNormalizesCameraSerial(t *testing.T) {
	hints := validCalibrationUploadHints()
	hints["camera_serial"] = "  " + testCameraSerial + "  "

	intent, err := parseUploadIntent(hints)
	if err != nil {
		t.Fatalf("parseUploadIntent() error = %v", err)
	}
	if intent.CameraSerial != testCameraSerial || hints["camera_serial"] != testCameraSerial {
		t.Fatalf("camera_serial intent=%q hint=%q", intent.CameraSerial, hints["camera_serial"])
	}
}

func TestParseCalibrationUploadIntentRejectsInvalidCameraSerial(t *testing.T) {
	tests := []struct {
		name   string
		serial string
	}{
		{name: "missing"},
		{name: "blank", serial: " \t "},
		{name: "oversized", serial: strings.Repeat("相", 256)},
		{name: "control character", serial: "CAMERA\n001"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hints := validCalibrationUploadHints()
			hints["camera_serial"] = tt.serial

			_, err := parseUploadIntent(hints)
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("parseUploadIntent() error = %v, want InvalidArgument", err)
			}
		})
	}
}

func TestParseTaskUploadIntentDoesNotRequireCameraSerial(t *testing.T) {
	intent, err := parseUploadIntent(map[string]string{
		"upload_kind": "task_episode",
		"capture_id":  "capture-1",
	})
	if err != nil {
		t.Fatalf("parseUploadIntent() error = %v", err)
	}
	if intent.Kind != uploadKindTaskEpisode {
		t.Fatalf("kind = %q, want %q", intent.Kind, uploadKindTaskEpisode)
	}
}

func TestParseTaskUploadIntentNormalizesOptionalCameraSerial(t *testing.T) {
	hints := map[string]string{
		"upload_kind":   "task_episode",
		"capture_id":    "capture-1",
		"camera_serial": "  " + testCameraSerial + "  ",
	}

	intent, err := parseUploadIntent(hints)
	if err != nil {
		t.Fatalf("parseUploadIntent() error = %v", err)
	}
	if intent.CameraSerial != testCameraSerial || hints["camera_serial"] != testCameraSerial {
		t.Fatalf("camera_serial intent=%q hint=%q", intent.CameraSerial, hints["camera_serial"])
	}
}

func TestParseTaskUploadIntentRejectsInvalidOptionalCameraSerial(t *testing.T) {
	hints := map[string]string{
		"upload_kind":   "task_episode",
		"capture_id":    "capture-1",
		"camera_serial": "CAMERA\n001",
	}

	_, err := parseUploadIntent(hints)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("parseUploadIntent() error = %v, want InvalidArgument", err)
	}
}

func TestParseCalibrationUploadIntentRequiresCanonicalUUIDv4(t *testing.T) {
	tests := []struct {
		name      string
		sessionID string
		captureID string
	}{
		{
			name:      "version 1 session",
			sessionID: "f47ac10b-58cc-1372-8567-0e02b2c3d479",
			captureID: "92cd6f2f-d131-4bf0-9b4a-d96258d09011",
		},
		{
			name:      "noncanonical capture",
			sessionID: "7f9af590-75c2-47ad-b6e0-76ebf05c44f7",
			captureID: "92CD6F2F-D131-4BF0-9B4A-D96258D09011",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hints := validCalibrationUploadHints()
			hints["calibration_session_id"] = tt.sessionID
			hints["capture_id"] = tt.captureID
			_, err := parseUploadIntent(hints)
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("parseUploadIntent() error = %v, want InvalidArgument", err)
			}
		})
	}
}

func validCalibrationUploadHints() map[string]string {
	return map[string]string{
		"upload_kind":            "calibration_capture",
		"camera_serial":          testCameraSerial,
		"calibration_session_id": "7f9af590-75c2-47ad-b6e0-76ebf05c44f7",
		"capture_id":             "92cd6f2f-d131-4bf0-9b4a-d96258d09011",
		"attempt_no":             "1",
		"checksum_sha256":        "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
	}
}
