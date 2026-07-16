// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package cloud

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestNormalizeTOSNativeEndpoint(t *testing.T) {
	tests := []struct {
		name         string
		raw          string
		wantEndpoint string
		wantSecure   bool
	}{
		{
			name:         "s3-compatible endpoint rewrites to public native endpoint",
			raw:          "tos-s3-cn-beijing.ivolces.com",
			wantEndpoint: "tos-cn-beijing.volces.com",
			wantSecure:   true,
		},
		{
			name:         "https s3-compatible endpoint rewrites to public native endpoint",
			raw:          "https://tos-s3-cn-beijing.ivolces.com",
			wantEndpoint: "tos-cn-beijing.volces.com",
			wantSecure:   true,
		},
		{
			name:         "native endpoint remains native",
			raw:          "https://tos-cn-beijing.volces.com",
			wantEndpoint: "tos-cn-beijing.volces.com",
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
			gotEndpoint, gotSecure, err := normalizeTOSNativeEndpoint(tt.raw)
			if err != nil {
				t.Fatalf("normalizeTOSNativeEndpoint() error = %v", err)
			}
			if gotEndpoint != tt.wantEndpoint || gotSecure != tt.wantSecure {
				t.Fatalf("normalizeTOSNativeEndpoint() = %q, %t; want %q, %t", gotEndpoint, gotSecure, tt.wantEndpoint, tt.wantSecure)
			}
		})
	}
}

func TestTOSNativePutRequestUsesNativeHostAndTOSSignature(t *testing.T) {
	target := TOSS3UploadTarget{
		Endpoint:        "tos-s3-cn-beijing.ivolces.com",
		Region:          "cn-beijing",
		Bucket:          "bucket-a",
		Key:             "motion-store/media/uploads/2/src.mcap",
		AccessKeyID:     "temp-ak",
		SecretAccessKey: "temp-sk",
		TemporaryToken:  "temp-token",
	}
	endpoint, secure, err := normalizeTOSNativeEndpoint(target.Endpoint)
	if err != nil {
		t.Fatalf("normalizeTOSNativeEndpoint() error = %v", err)
	}
	req, err := newTOSPutObjectRequest(context.Background(), endpoint, secure, target, strings.Repeat("a", 64), strings.NewReader("body"), 4)
	if err != nil {
		t.Fatalf("newTOSPutObjectRequest() error = %v", err)
	}
	if err := signTOSRequest(req, target, strings.Repeat("a", 64), time.Date(2026, 7, 15, 5, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("signTOSRequest() error = %v", err)
	}

	if req.URL.String() != "https://bucket-a.tos-cn-beijing.volces.com/motion-store/media/uploads/2/src.mcap" {
		t.Fatalf("request URL = %q", req.URL.String())
	}
	if got := req.Header.Get("Authorization"); !strings.HasPrefix(got, "TOS4-HMAC-SHA256 Credential=temp-ak/20260715/cn-beijing/tos/request") {
		t.Fatalf("Authorization = %q", got)
	}
	if !strings.Contains(req.Header.Get("Authorization"), "SignedHeaders=host;x-tos-content-sha256;x-tos-date;x-tos-security-token") {
		t.Fatalf("Authorization missing signed headers: %q", req.Header.Get("Authorization"))
	}
	if req.Header.Get("x-tos-security-token") != "temp-token" {
		t.Fatalf("x-tos-security-token = %q", req.Header.Get("x-tos-security-token"))
	}
}
