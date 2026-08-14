// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"archebase.com/keystone-edge/internal/config"
)

func TestEpisodeQATOSReaderSignsWithTOSAuthorization(t *testing.T) {
	reader := newEpisodeQATOSReader(config.StorageConfig{
		Endpoint:  "tos-cn-beijing.volces.com",
		Region:    "cn-beijing",
		AccessKey: "test-ak",
		SecretKey: "test-sk",
		UseSSL:    true,
	})

	objectURL, err := reader.objectURL("bucket-a", "device-uploads/capture 1.mcap")
	if err != nil {
		t.Fatalf("objectURL: %v", err)
	}
	req, err := http.NewRequest(http.MethodHead, objectURL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if err := reader.sign(req, nil, time.Date(2026, 7, 15, 6, 0, 0, 0, time.UTC), episodeQATOSCredentials{
		accessKeyID:     "temp-ak",
		accessKeySecret: "temp-sk",
		securityToken:   "temp-token",
	}); err != nil {
		t.Fatalf("sign: %v", err)
	}

	if req.URL.String() != "https://bucket-a.tos-cn-beijing.volces.com/device-uploads/capture%201.mcap" {
		t.Fatalf("url = %q", req.URL.String())
	}
	auth := req.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "TOS4-HMAC-SHA256 Credential=temp-ak/20260715/cn-beijing/tos/request") {
		t.Fatalf("Authorization = %q", auth)
	}
	if !strings.Contains(auth, "SignedHeaders=host;x-tos-content-sha256;x-tos-date;x-tos-security-token") {
		t.Fatalf("Authorization does not sign security token: %q", auth)
	}
	if strings.Contains(auth, "AWS4-HMAC-SHA256") || strings.Contains(auth, "/s3/") || strings.Contains(auth, "us-east-1") {
		t.Fatalf("Authorization uses AWS S3 signing scope: %q", auth)
	}
	if req.Header.Get("x-tos-content-sha256") == "" || req.Header.Get("x-tos-date") == "" {
		t.Fatalf("missing TOS signing headers")
	}
	if req.Header.Get("x-tos-security-token") != "temp-token" {
		t.Fatalf("x-tos-security-token = %q, want temp-token", req.Header.Get("x-tos-security-token"))
	}
}

func TestEpisodeQATOSReaderRequiresStorageRole(t *testing.T) {
	reader := newEpisodeQATOSReader(config.StorageConfig{
		Endpoint:  "tos-cn-beijing.volces.com",
		Region:    "cn-beijing",
		AccessKey: "test-ak",
		SecretKey: "test-sk",
		UseSSL:    true,
	})

	_, err := reader.credentials(context.Background(), "bucket-a", "device-uploads/capture.mcap")
	if err == nil {
		t.Fatal("credentials() error = nil, want storage role configuration error")
	}
	if !strings.Contains(err.Error(), "KEYSTONE_DGW_VOLCENGINE_STORAGE_STS_ROLE_TRN") {
		t.Fatalf("credentials() error = %q, want storage role configuration error", err.Error())
	}
}
