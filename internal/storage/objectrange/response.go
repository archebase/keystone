// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package objectrange validates bounded object-storage range responses.
package objectrange

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// NormalizeETag removes the optional HTTP quotes around an object ETag.
func NormalizeETag(value string) string {
	return strings.Trim(strings.TrimSpace(value), `"`)
}

// QuoteETag formats a normalized S3/TOS ETag for an If-Match header.
func QuoteETag(value string) string {
	return `"` + NormalizeETag(value) + `"`
}

// ValidateResponse verifies that a single-range response contains exactly the
// requested bytes from the expected immutable object snapshot.
func ValidateResponse(header http.Header, offset, length, totalSize int64, expectedETag string) error {
	if offset < 0 || length <= 0 || totalSize <= 0 || offset > totalSize || length > totalSize-offset {
		return fmt.Errorf("invalid expected object range offset=%d length=%d total=%d", offset, length, totalSize)
	}
	end := offset + length - 1
	wantContentRange := fmt.Sprintf("bytes %d-%d/%d", offset, end, totalSize)
	if got := strings.TrimSpace(header.Get("Content-Range")); got != wantContentRange {
		return fmt.Errorf("object range content-range %q, want %q", got, wantContentRange)
	}
	contentLength, err := strconv.ParseInt(strings.TrimSpace(header.Get("Content-Length")), 10, 64)
	if err != nil || contentLength != length {
		return fmt.Errorf("object range content-length %q, want %d", header.Get("Content-Length"), length)
	}
	wantETag := NormalizeETag(expectedETag)
	if wantETag == "" {
		return fmt.Errorf("expected object ETag is empty")
	}
	if got := NormalizeETag(header.Get("ETag")); got != wantETag {
		return fmt.Errorf("object range ETag %q, want %q", got, wantETag)
	}
	return nil
}
