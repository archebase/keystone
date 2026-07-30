// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package tos

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"archebase.com/keystone-edge/internal/config"
	"github.com/volcengine/volcengine-go-sdk/service/sts"
	"github.com/volcengine/volcengine-go-sdk/volcengine"
	"github.com/volcengine/volcengine-go-sdk/volcengine/request"
)

func TestNewReaderRoutesSourceEndpointUsingMode(t *testing.T) {
	tests := []struct {
		name            string
		mode            string
		endpoint        string
		wantRequestHost string
	}{
		{
			name:            "cloud routes configured public endpoint privately",
			mode:            config.ModeCloud,
			endpoint:        "tos-cn-beijing.volces.com",
			wantRequestHost: "source-bucket.tos-cn-beijing.ivolces.com",
		},
		{
			name:            "edge keeps configured public endpoint",
			mode:            config.ModeEdge,
			endpoint:        "tos-cn-beijing.volces.com",
			wantRequestHost: "source-bucket.tos-cn-beijing.volces.com",
		},
		{
			name:            "edge routes configured private endpoint publicly",
			mode:            config.ModeEdge,
			endpoint:        "tos-cn-beijing.ivolces.com",
			wantRequestHost: "source-bucket.tos-cn-beijing.volces.com",
		},
		{
			name:            "cloud leaves custom endpoint unchanged",
			mode:            config.ModeCloud,
			endpoint:        "tos.internal.example",
			wantRequestHost: "source-bucket.tos.internal.example",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotRequest *http.Request
			reader := NewReader(config.StorageConfig{
				Endpoint:  tt.endpoint,
				Region:    "cn-beijing",
				AccessKey: "test-ak",
				SecretKey: "test-sk",
				UseSSL:    true,
			}, tt.mode, time.Minute)
			reader.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				gotRequest = req
				header := make(http.Header)
				header.Set("Content-Length", "100")
				header.Set("ETag", `"source-etag"`)
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     header,
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    req,
				}, nil
			})}

			if _, _, err := reader.StatObject(context.Background(), "source-bucket", "path/capture.mcap"); err != nil {
				t.Fatalf("StatObject() error = %v", err)
			}
			if gotRequest == nil {
				t.Fatal("StatObject() did not send a request")
			}
			if gotRequest.URL.Host != tt.wantRequestHost {
				t.Fatalf("request host = %q, want %q", gotRequest.URL.Host, tt.wantRequestHost)
			}
		})
	}
}

func TestOpenObjectRangeSendsBoundedRangeRequest(t *testing.T) {
	var gotRequest *http.Request
	reader := &Reader{
		endpoint:  "tos-cn-beijing.volces.com",
		region:    "cn-beijing",
		accessKey: "test-ak",
		secretKey: "test-sk",
		useSSL:    true,
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			gotRequest = req
			header := make(http.Header)
			header.Set("Content-Range", "bytes 64-67/100")
			header.Set("Content-Length", "4")
			header.Set("ETag", `"source-etag"`)
			return &http.Response{
				StatusCode: http.StatusPartialContent,
				Header:     header,
				Body:       io.NopCloser(strings.NewReader("data")),
				Request:    req,
			}, nil
		})},
	}

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
	if got := gotRequest.URL.String(); got != "https://source-bucket.tos-cn-beijing.volces.com/path/capture.mcap" {
		t.Fatalf("request URL = %q", got)
	}
	if got := gotRequest.Header.Get("Authorization"); !strings.HasPrefix(got, "TOS4-HMAC-SHA256 Credential=test-ak/") {
		t.Fatalf("Authorization = %q", got)
	}
}

func TestStatObjectReturnsSizeAndNormalizedETag(t *testing.T) {
	var gotRequest *http.Request
	reader := &Reader{
		endpoint:  "tos-cn-beijing.volces.com",
		region:    "cn-beijing",
		accessKey: "test-ak",
		secretKey: "test-sk",
		useSSL:    true,
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			gotRequest = req
			header := make(http.Header)
			header.Set("Content-Length", "100")
			header.Set("ETag", `"source-etag"`)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     header,
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    req,
			}, nil
		})},
	}

	size, etag, err := reader.StatObject(context.Background(), "source-bucket", "path/capture.mcap")
	if err != nil {
		t.Fatalf("StatObject() error = %v", err)
	}
	if size != 100 || etag != "source-etag" {
		t.Fatalf("StatObject() = size:%d etag:%q, want size:100 etag:source-etag", size, etag)
	}
	if gotRequest == nil || gotRequest.Method != http.MethodHead {
		t.Fatalf("StatObject request = %#v, want HEAD", gotRequest)
	}
}

func TestOpenObjectRangeRejectsNonPartialSuccess(t *testing.T) {
	reader := &Reader{
		endpoint:  "tos-cn-beijing.volces.com",
		region:    "cn-beijing",
		accessKey: "test-ak",
		secretKey: "test-sk",
		useSSL:    true,
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("whole object")),
				Request:    req,
			}, nil
		})},
	}

	_, err := reader.OpenObjectRange(context.Background(), "source-bucket", "path/capture.mcap", 4, 4, 12, "source-etag")
	if err == nil || !strings.Contains(err.Error(), "want 206") {
		t.Fatalf("OpenObjectRange() error = %v, want non-partial response error", err)
	}
}

func TestOpenObjectRangeRejectsWrongContentRange(t *testing.T) {
	reader := &Reader{
		endpoint:  "tos-cn-beijing.volces.com",
		region:    "cn-beijing",
		accessKey: "test-ak",
		secretKey: "test-sk",
		useSSL:    true,
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			header := make(http.Header)
			header.Set("Content-Range", "bytes 0-3/12")
			header.Set("Content-Length", "4")
			header.Set("ETag", `"source-etag"`)
			return &http.Response{
				StatusCode: http.StatusPartialContent,
				Header:     header,
				Body:       io.NopCloser(strings.NewReader("data")),
				Request:    req,
			}, nil
		})},
	}

	_, err := reader.OpenObjectRange(context.Background(), "source-bucket", "path/capture.mcap", 4, 4, 12, "source-etag")
	if err == nil || !strings.Contains(err.Error(), "content-range") {
		t.Fatalf("OpenObjectRange() error = %v, want mismatched content-range error", err)
	}
}

func TestReaderReusesUnexpiredSTSCredentialsForObjectRanges(t *testing.T) {
	expiresAt := time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339)
	stsClient := &fakeSTSClient{output: (&sts.AssumeRoleOutput{}).SetCredentials(
		(&sts.CredentialsForAssumeRoleOutput{}).
			SetAccessKeyId("temporary-ak").
			SetSecretAccessKey("temporary-sk").
			SetSessionToken("temporary-token").
			SetExpiredTime(expiresAt),
	)}
	reader := &Reader{
		roleTRN:   "trn:iam::123:role/tos-read",
		stsClient: stsClient,
	}

	first, err := reader.credentials(context.Background(), "source-bucket", "path/capture.mcap")
	if err != nil {
		t.Fatalf("first credentials() error = %v", err)
	}
	second, err := reader.credentials(context.Background(), "source-bucket", "path/capture.mcap")
	if err != nil {
		t.Fatalf("second credentials() error = %v", err)
	}
	if first.accessKeyID != "temporary-ak" || second.accessKeyID != first.accessKeyID {
		t.Fatalf("cached credentials = %q/%q, want temporary-ak", first.accessKeyID, second.accessKeyID)
	}
	if stsClient.calls != 1 {
		t.Fatalf("AssumeRole calls = %d, want 1 for repeated ranges of one object", stsClient.calls)
	}

	if _, err := reader.credentials(context.Background(), "source-bucket", "path/other.mcap"); err != nil {
		t.Fatalf("other object credentials() error = %v", err)
	}
	if stsClient.calls != 2 {
		t.Fatalf("AssumeRole calls = %d, want separate credentials for another object", stsClient.calls)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type fakeSTSClient struct {
	calls  int
	output *sts.AssumeRoleOutput
}

func (c *fakeSTSClient) AssumeRoleWithContext(volcengine.Context, *sts.AssumeRoleInput, ...request.Option) (*sts.AssumeRoleOutput, error) {
	c.calls++
	return c.output, nil
}
