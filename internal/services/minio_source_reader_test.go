// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"archebase.com/keystone-edge/internal/storage/s3"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

func TestMinioSourceObjectReaderOpensPinnedRange(t *testing.T) {
	var gotRequest *http.Request
	reader := newTestMinioSourceObjectReader(t, minioRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotRequest = req
		return minioRangeResponse(req, http.StatusPartialContent, "bytes 64-67/100", "4", `"source-etag"`, "data"), nil
	}))

	body, err := reader.OpenObjectRange(context.Background(), "source-bucket", "path/capture.mcap", 64, 4, 100, "source-etag")
	if err != nil {
		t.Fatalf("OpenObjectRange() error = %v", err)
	}
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read range body: %v", err)
	}
	if err := body.Close(); err != nil {
		t.Fatalf("close range body: %v", err)
	}
	if string(data) != "data" {
		t.Fatalf("range body = %q, want data", data)
	}
	if gotRequest == nil {
		t.Fatal("range request was not sent")
	}
	if got := gotRequest.Header.Get("Range"); got != "bytes=64-67" {
		t.Fatalf("Range header = %q, want bytes=64-67", got)
	}
	if got := gotRequest.Header.Get("If-Match"); got != `"source-etag"` {
		t.Fatalf("If-Match header = %q, want quoted source ETag", got)
	}
}

func TestMinioSourceObjectReaderRejectsIgnoredRange(t *testing.T) {
	reader := newTestMinioSourceObjectReader(t, minioRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return minioRangeResponse(req, http.StatusOK, "", "12", `"source-etag"`, "whole object"), nil
	}))

	_, err := reader.OpenObjectRange(context.Background(), "source-bucket", "path/capture.mcap", 4, 4, 12, "source-etag")
	if err == nil || !strings.Contains(err.Error(), "content-range") {
		t.Fatalf("OpenObjectRange() error = %v, want ignored range rejection", err)
	}
}

func TestMinioSourceObjectReaderRejectsWrongRangeMetadata(t *testing.T) {
	reader := newTestMinioSourceObjectReader(t, minioRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return minioRangeResponse(req, http.StatusPartialContent, "bytes 0-3/12", "4", `"source-etag"`, "data"), nil
	}))

	_, err := reader.OpenObjectRange(context.Background(), "source-bucket", "path/capture.mcap", 4, 4, 12, "source-etag")
	if err == nil || !strings.Contains(err.Error(), "content-range") {
		t.Fatalf("OpenObjectRange() error = %v, want mismatched content-range rejection", err)
	}
}

func newTestMinioSourceObjectReader(t *testing.T, transport http.RoundTripper) minioSourceObjectReader {
	t.Helper()
	client, err := minio.New("minio.example", &minio.Options{
		Creds:        credentials.NewStaticV4("test-ak", "test-sk", ""),
		Secure:       true,
		Region:       "us-east-1",
		BucketLookup: minio.BucketLookupPath,
		Transport:    transport,
	})
	if err != nil {
		t.Fatalf("create MinIO client: %v", err)
	}
	return minioSourceObjectReader{client: &s3.Client{Client: client}}
}

func minioRangeResponse(req *http.Request, status int, contentRange, contentLength, etag, body string) *http.Response {
	header := make(http.Header)
	header.Set("Content-Range", contentRange)
	header.Set("Content-Length", contentLength)
	header.Set("ETag", etag)
	header.Set("Last-Modified", time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC).Format(http.TimeFormat))
	return &http.Response{
		StatusCode: status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

type minioRoundTripFunc func(*http.Request) (*http.Response, error)

func (f minioRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
