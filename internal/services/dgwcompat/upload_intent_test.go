// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package dgwcompat

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

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
			_, err := parseUploadIntent(map[string]string{
				"upload_kind":            "calibration_capture",
				"calibration_session_id": tt.sessionID,
				"capture_id":             tt.captureID,
				"attempt_no":             "1",
				"checksum_sha256":        "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
			})
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("parseUploadIntent() error = %v, want InvalidArgument", err)
			}
		})
	}
}
