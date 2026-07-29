// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package objectrange

import (
	"net/http"
	"strings"
	"testing"
)

func TestValidateResponseAcceptsExactPinnedRange(t *testing.T) {
	header := make(http.Header)
	header.Set("Content-Range", "bytes 64-67/100")
	header.Set("Content-Length", "4")
	header.Set("ETag", `"source-etag"`)

	if err := ValidateResponse(header, 64, 4, 100, "source-etag"); err != nil {
		t.Fatalf("ValidateResponse() error = %v", err)
	}
}

func TestValidateResponseRejectsChangedObjectETag(t *testing.T) {
	header := make(http.Header)
	header.Set("Content-Range", "bytes 64-67/100")
	header.Set("Content-Length", "4")
	header.Set("ETag", `"changed-etag"`)

	err := ValidateResponse(header, 64, 4, 100, "source-etag")
	if err == nil || !strings.Contains(err.Error(), "ETag") {
		t.Fatalf("ValidateResponse() error = %v, want changed ETag rejection", err)
	}
}

func TestValidateResponseRejectsWrongContentLength(t *testing.T) {
	header := make(http.Header)
	header.Set("Content-Range", "bytes 64-67/100")
	header.Set("Content-Length", "100")
	header.Set("ETag", `"source-etag"`)

	err := ValidateResponse(header, 64, 4, 100, "source-etag")
	if err == nil || !strings.Contains(err.Error(), "content-length") {
		t.Fatalf("ValidateResponse() error = %v, want content-length rejection", err)
	}
}
