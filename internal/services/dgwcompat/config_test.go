// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package dgwcompat

import (
	"testing"
	"time"
)

func TestConfigValidateAllowsDefaultCredentialChain(t *testing.T) {
	cfg := Config{
		Enabled:          true,
		GRPCAddr:         ":50053",
		TOSBucket:        "tos-bucket",
		TOSEndpoint:      "https://tos-cn-beijing.volces.com",
		TOSRegion:        "cn-beijing",
		UploadPartSize:   8 * 1024 * 1024,
		STSRoleTRN:       "trn:iam::123:role/upload",
		STSSessionTTL:    15 * time.Minute,
		DeviceJWTSecret:  "jwt-secret",
		DeviceJWTTTL:     15 * time.Minute,
		HilbertBaseURL:   "https://hilbert.example",
		HilbertAccessKey: "hilbert-ak",
		HilbertSecretKey: "hilbert-sk",
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
}

func TestConfigValidateRejectsPartialStaticCredentials(t *testing.T) {
	cfg := Config{
		Enabled:          true,
		GRPCAddr:         ":50053",
		TOSBucket:        "tos-bucket",
		TOSEndpoint:      "https://tos-cn-beijing.volces.com",
		TOSRegion:        "cn-beijing",
		UploadPartSize:   8 * 1024 * 1024,
		STSRoleTRN:       "trn:iam::123:role/upload",
		STSSessionTTL:    15 * time.Minute,
		AccessKeyID:      "ak",
		DeviceJWTSecret:  "jwt-secret",
		DeviceJWTTTL:     15 * time.Minute,
		HilbertBaseURL:   "https://hilbert.example",
		HilbertAccessKey: "hilbert-ak",
		HilbertSecretKey: "hilbert-sk",
	}

	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want partial static credential error")
	}
}
