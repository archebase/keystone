// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package cloud

import "testing"

func TestNormalizeTOSS3Endpoint(t *testing.T) {
	tests := []struct {
		name         string
		raw          string
		wantEndpoint string
		wantSecure   bool
	}{
		{
			name:         "bare endpoint defaults secure",
			raw:          "tos-s3-cn-beijing.ivolces.com",
			wantEndpoint: "tos-s3-cn-beijing.ivolces.com",
			wantSecure:   true,
		},
		{
			name:         "https endpoint strips scheme",
			raw:          "https://tos-s3-cn-beijing.ivolces.com",
			wantEndpoint: "tos-s3-cn-beijing.ivolces.com",
			wantSecure:   true,
		},
		{
			name:         "http endpoint disables secure",
			raw:          "http://localhost:9000",
			wantEndpoint: "localhost:9000",
			wantSecure:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotEndpoint, gotSecure, err := normalizeTOSS3Endpoint(tt.raw)
			if err != nil {
				t.Fatalf("normalizeTOSS3Endpoint() error = %v", err)
			}
			if gotEndpoint != tt.wantEndpoint || gotSecure != tt.wantSecure {
				t.Fatalf("normalizeTOSS3Endpoint() = %q, %t; want %q, %t", gotEndpoint, gotSecure, tt.wantEndpoint, tt.wantSecure)
			}
		})
	}
}
