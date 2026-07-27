// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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

func TestTOSS3UploaderProductionMultipartDefaults(t *testing.T) {
	uploader := NewTOSS3Uploader(time.Minute)
	if got := uploader.effectiveMultipartThreshold(); got != 128*1024*1024 {
		t.Fatalf("multipart threshold = %d, want 128 MiB", got)
	}
	if got := uploader.effectiveMultipartPartSize(); got != 64*1024*1024 {
		t.Fatalf("multipart part size = %d, want 64 MiB", got)
	}
	if got := uploader.effectivePartAttempts(); got != 3 {
		t.Fatalf("multipart part attempts = %d, want 3", got)
	}
}

func TestMultipartPartSizeLimitsPartCount(t *testing.T) {
	base := int64(64 * 1024 * 1024)
	size := base*tosMultipartMaxParts + 1
	got := multipartPartSize(size, base)
	if got != 2*base {
		t.Fatalf("multipartPartSize() = %d, want %d", got, 2*base)
	}
	if count := multipartPartCount(size, got); count > tosMultipartMaxParts {
		t.Fatalf("multipart part count = %d, want <= %d", count, tosMultipartMaxParts)
	}
}

func TestTOSS3UploaderSmallObjectStreamsSinglePut(t *testing.T) {
	payload := []byte("small")
	payloadHash := sha256Hex(payload)
	requestCount := 0
	uploader := newTestTOSUploader(t, func(req *http.Request) (*http.Response, error) {
		requestCount++
		if req.Method != http.MethodPut || req.URL.RawQuery != "" {
			t.Fatalf("request = %s %s, want single PUT without query", req.Method, req.URL.String())
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		if !bytes.Equal(body, payload) {
			t.Fatalf("request body = %q, want %q", body, payload)
		}
		if got := req.Header.Get("x-tos-content-sha256"); got != payloadHash {
			t.Fatalf("payload hash header = %q, want %q", got, payloadHash)
		}
		return testTOSResponse(http.StatusOK, "", map[string]string{"ETag": `"single-etag"`}), nil
	})
	uploader.multipartThreshold = int64(len(payload) + 1)

	etag, err := uploader.PutObject(context.Background(), testTOSUploadTarget(), bytes.NewReader(payload), int64(len(payload)), payloadHash, nil)
	if err != nil {
		t.Fatalf("PutObject() error = %v", err)
	}
	if etag != "single-etag" || requestCount != 1 {
		t.Fatalf("PutObject() etag/requests = %q/%d, want single-etag/1", etag, requestCount)
	}
}

func TestTOSS3UploaderMultipartHappyPath(t *testing.T) {
	payload := []byte("abcdefghij")
	wantParts := map[string]string{
		"1": "abcd",
		"2": "efgh",
		"3": "ij",
	}
	created := 0
	completed := 0
	aborted := 0
	uploadedParts := 0
	uploader := newTestTOSUploader(t, func(req *http.Request) (*http.Response, error) {
		query := req.URL.Query()
		switch {
		case req.Method == http.MethodPost && query.Has("uploads"):
			created++
			assertSignedTOSRequest(t, req)
			return testTOSResponse(http.StatusOK, `{"Bucket":"bucket-a","Key":"raw-data/test.mcap","UploadId":"upload-1"}`, map[string]string{"Content-Type": "application/json"}), nil
		case req.Method == http.MethodPut && query.Get("uploadId") == "upload-1":
			uploadedParts++
			partNumber := query.Get("partNumber")
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("read part %s: %v", partNumber, err)
			}
			if string(body) != wantParts[partNumber] {
				t.Fatalf("part %s body = %q, want %q", partNumber, body, wantParts[partNumber])
			}
			if got := req.Header.Get("x-tos-content-sha256"); got != sha256Hex(body) {
				t.Fatalf("part %s hash = %q, want %q", partNumber, got, sha256Hex(body))
			}
			assertSignedTOSRequest(t, req)
			return testTOSResponse(http.StatusOK, "", map[string]string{"ETag": fmt.Sprintf(`"part-%s"`, partNumber)}), nil
		case req.Method == http.MethodPost && query.Get("uploadId") == "upload-1":
			completed++
			if got := req.Header.Get("Content-Type"); got != "application/json" {
				t.Fatalf("complete content type = %q, want application/json", got)
			}
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("read complete body: %v", err)
			}
			var complete tosCompleteMultipartUpload
			if err := json.Unmarshal(body, &complete); err != nil {
				t.Fatalf("decode complete body %q: %v", body, err)
			}
			if len(complete.Parts) != 3 {
				t.Fatalf("complete parts = %+v, want 3 parts", complete.Parts)
			}
			for index, part := range complete.Parts {
				partNumber := index + 1
				if part.PartNumber != partNumber || part.ETag != fmt.Sprintf(`"part-%d"`, partNumber) {
					t.Fatalf("complete part %d = %+v", partNumber, part)
				}
			}
			assertSignedTOSRequest(t, req)
			return testTOSResponse(http.StatusOK, `{"Bucket":"bucket-a","Key":"raw-data/test.mcap","ETag":"\"final-etag\""}`, map[string]string{"Content-Type": "application/json"}), nil
		case req.Method == http.MethodDelete:
			aborted++
			return testTOSResponse(http.StatusNoContent, "", nil), nil
		default:
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
			return nil, nil
		}
	})
	uploader.multipartThreshold = int64(len(payload))
	uploader.multipartPartSize = 4

	var progress []int64
	etag, err := uploader.PutObject(context.Background(), testTOSUploadTarget(), bytes.NewReader(payload), int64(len(payload)), sha256Hex(payload), func(uploadedBytes, _ int64) {
		progress = append(progress, uploadedBytes)
	})
	if err != nil {
		t.Fatalf("PutObject() error = %v", err)
	}
	if etag != "final-etag" {
		t.Fatalf("PutObject() etag = %q, want final-etag", etag)
	}
	if created != 1 || uploadedParts != 3 || completed != 1 || aborted != 0 {
		t.Fatalf("multipart calls create/parts/complete/abort = %d/%d/%d/%d", created, uploadedParts, completed, aborted)
	}
	if got := fmt.Sprint(progress); got != "[4 8 10 10]" {
		t.Fatalf("progress = %s, want [4 8 10 10]", got)
	}
}

func TestTOSS3UploaderMultipartCreateEmptyResponseIncludesDiagnostics(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		wantError  string
	}{
		{name: "empty 200 response", statusCode: http.StatusOK, wantError: "empty response body"},
		{name: "empty 204 response", statusCode: http.StatusNoContent, wantError: "tos status=204"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := []byte("part")
			requestCount := 0
			uploader := newTestTOSUploader(t, func(req *http.Request) (*http.Response, error) {
				requestCount++
				if req.Method != http.MethodPost || !req.URL.Query().Has("uploads") {
					t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
				}
				return testTOSResponse(tt.statusCode, "", map[string]string{
					"Content-Type":     "application/json",
					"x-tos-request-id": "request-empty-create",
				}), nil
			})
			uploader.multipartThreshold = int64(len(payload))

			_, err := uploader.PutObject(context.Background(), testTOSUploadTarget(), bytes.NewReader(payload), int64(len(payload)), sha256Hex(payload), nil)
			if err == nil {
				t.Fatal("PutObject() error = nil, want create multipart error")
			}
			for _, want := range []string{
				tt.wantError,
				fmt.Sprintf("status=%d", tt.statusCode),
				"request_id=request-empty-create",
				`content_type="application/json"`,
				"body_length=0",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("PutObject() error = %q, want substring %q", err, want)
				}
			}
			if requestCount != 1 {
				t.Fatalf("request count = %d, want 1", requestCount)
			}
		})
	}
}

func TestTOSS3UploaderMultipartCompleteEmptyResponseAbortsWithDiagnostics(t *testing.T) {
	payload := []byte("part")
	aborted := 0
	uploader := newTestTOSUploader(t, func(req *http.Request) (*http.Response, error) {
		query := req.URL.Query()
		switch {
		case req.Method == http.MethodPost && query.Has("uploads"):
			return testTOSResponse(http.StatusOK, `{"UploadId":"upload-empty-complete"}`, nil), nil
		case req.Method == http.MethodPut:
			return testTOSResponse(http.StatusOK, "", map[string]string{"ETag": `"part-1"`}), nil
		case req.Method == http.MethodPost && query.Get("uploadId") == "upload-empty-complete":
			return testTOSResponse(http.StatusOK, "", map[string]string{
				"Content-Type":     "application/json",
				"x-tos-request-id": "request-empty-complete",
			}), nil
		case req.Method == http.MethodDelete && query.Get("uploadId") == "upload-empty-complete":
			aborted++
			return testTOSResponse(http.StatusNoContent, "", nil), nil
		default:
			return nil, fmt.Errorf("unexpected request: %s %s", req.Method, req.URL.String())
		}
	})
	uploader.multipartThreshold = int64(len(payload))
	uploader.multipartPartSize = int64(len(payload))

	_, err := uploader.PutObject(context.Background(), testTOSUploadTarget(), bytes.NewReader(payload), int64(len(payload)), sha256Hex(payload), nil)
	if err == nil {
		t.Fatal("PutObject() error = nil, want complete multipart error")
	}
	for _, want := range []string{
		"decode complete tos multipart response: empty response body",
		"status=200",
		"request_id=request-empty-complete",
		`content_type="application/json"`,
		"body_length=0",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("PutObject() error = %q, want substring %q", err, want)
		}
	}
	if aborted != 1 {
		t.Fatalf("abort calls = %d, want 1", aborted)
	}
}

func TestTOSS3UploaderMultipartRetriesRetryablePartFailure(t *testing.T) {
	payload := []byte("part")
	partAttempts := 0
	aborted := 0
	uploader := newTestTOSUploader(t, func(req *http.Request) (*http.Response, error) {
		query := req.URL.Query()
		switch {
		case req.Method == http.MethodPost && query.Has("uploads"):
			return testTOSResponse(http.StatusOK, `{"UploadId":"upload-retry"}`, nil), nil
		case req.Method == http.MethodPut:
			partAttempts++
			_, _ = io.ReadAll(req.Body)
			if partAttempts == 1 {
				return testTOSResponse(http.StatusServiceUnavailable, `{"Code":"ServiceUnavailable"}`, nil), nil
			}
			return testTOSResponse(http.StatusOK, "", map[string]string{"ETag": `"part-1"`}), nil
		case req.Method == http.MethodPost && query.Get("uploadId") == "upload-retry":
			return testTOSResponse(http.StatusOK, `{"ETag":"\"final\""}`, nil), nil
		case req.Method == http.MethodDelete:
			aborted++
			return testTOSResponse(http.StatusNoContent, "", nil), nil
		default:
			return nil, fmt.Errorf("unexpected request: %s %s", req.Method, req.URL.String())
		}
	})
	uploader.multipartThreshold = int64(len(payload))
	uploader.multipartPartSize = int64(len(payload))

	if _, err := uploader.PutObject(context.Background(), testTOSUploadTarget(), bytes.NewReader(payload), int64(len(payload)), sha256Hex(payload), nil); err != nil {
		t.Fatalf("PutObject() error = %v", err)
	}
	if partAttempts != 2 || aborted != 0 {
		t.Fatalf("part attempts/aborts = %d/%d, want 2/0", partAttempts, aborted)
	}
}

func TestTOSS3UploaderMultipartDoesNotRetryAccessDeniedAndAborts(t *testing.T) {
	payload := []byte("part")
	partAttempts := 0
	aborted := 0
	uploader := newTestTOSUploader(t, func(req *http.Request) (*http.Response, error) {
		query := req.URL.Query()
		switch {
		case req.Method == http.MethodPost && query.Has("uploads"):
			return testTOSResponse(http.StatusOK, `{"UploadId":"upload-denied"}`, nil), nil
		case req.Method == http.MethodPut:
			partAttempts++
			return testTOSResponse(http.StatusForbidden, `{"Code":"AccessDenied","Message":"denied"}`, nil), nil
		case req.Method == http.MethodDelete && query.Get("uploadId") == "upload-denied":
			aborted++
			return testTOSResponse(http.StatusNoContent, "", nil), nil
		default:
			return nil, fmt.Errorf("unexpected request: %s %s", req.Method, req.URL.String())
		}
	})
	uploader.multipartThreshold = int64(len(payload))
	uploader.multipartPartSize = int64(len(payload))

	_, err := uploader.PutObject(context.Background(), testTOSUploadTarget(), bytes.NewReader(payload), int64(len(payload)), sha256Hex(payload), nil)
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("PutObject() error = %v, want AccessDenied", err)
	}
	if partAttempts != 1 || aborted != 1 {
		t.Fatalf("part attempts/aborts = %d/%d, want 1/1", partAttempts, aborted)
	}
}

func TestTOSS3UploaderMultipartChecksumMismatchAbortsBeforeComplete(t *testing.T) {
	payload := []byte("part")
	completed := 0
	aborted := 0
	uploader := newTestTOSUploader(t, func(req *http.Request) (*http.Response, error) {
		query := req.URL.Query()
		switch {
		case req.Method == http.MethodPost && query.Has("uploads"):
			return testTOSResponse(http.StatusOK, `{"UploadId":"upload-checksum"}`, nil), nil
		case req.Method == http.MethodPut:
			return testTOSResponse(http.StatusOK, "", map[string]string{"ETag": `"part-1"`}), nil
		case req.Method == http.MethodPost && query.Get("uploadId") == "upload-checksum":
			completed++
			return testTOSResponse(http.StatusOK, "", nil), nil
		case req.Method == http.MethodDelete && query.Get("uploadId") == "upload-checksum":
			aborted++
			return testTOSResponse(http.StatusNoContent, "", nil), nil
		default:
			return nil, fmt.Errorf("unexpected request: %s %s", req.Method, req.URL.String())
		}
	})
	uploader.multipartThreshold = int64(len(payload))
	uploader.multipartPartSize = int64(len(payload))

	_, err := uploader.PutObject(context.Background(), testTOSUploadTarget(), bytes.NewReader(payload), int64(len(payload)), strings.Repeat("0", 64), nil)
	if !errors.Is(err, ErrTOSPayloadChecksumMismatch) {
		t.Fatalf("PutObject() error = %v, want ErrTOSPayloadChecksumMismatch", err)
	}
	if completed != 0 || aborted != 1 {
		t.Fatalf("complete/abort calls = %d/%d, want 0/1", completed, aborted)
	}
}

type tosRoundTripFunc func(*http.Request) (*http.Response, error)

func (f tosRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func newTestTOSUploader(t *testing.T, roundTrip tosRoundTripFunc) *TOSS3Uploader {
	t.Helper()
	return &TOSS3Uploader{
		timeout:        time.Minute,
		client:         &http.Client{Transport: roundTrip},
		partAttempts:   3,
		retryBaseDelay: -1,
	}
}

func testTOSUploadTarget() TOSS3UploadTarget {
	return TOSS3UploadTarget{
		Endpoint:        "http://tos.example.test",
		Region:          "cn-beijing",
		Bucket:          "bucket-a",
		Key:             "raw-data/test.mcap",
		AccessKeyID:     "temp-ak",
		SecretAccessKey: "temp-sk",
		TemporaryToken:  "temp-token",
	}
}

func testTOSResponse(status int, body string, headers map[string]string) *http.Response {
	header := make(http.Header)
	for name, value := range headers {
		header.Set(name, value)
	}
	return &http.Response{
		StatusCode: status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func assertSignedTOSRequest(t *testing.T, req *http.Request) {
	t.Helper()
	if !strings.HasPrefix(req.Header.Get("Authorization"), tosNativeAlgorithm+" ") {
		t.Fatalf("request authorization = %q, want TOS4 signature", req.Header.Get("Authorization"))
	}
	if req.Header.Get("x-tos-content-sha256") == "" || req.Header.Get("x-tos-date") == "" {
		t.Fatalf("request missing signed TOS headers: %+v", req.Header)
	}
}
