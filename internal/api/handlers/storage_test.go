// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"archebase.com/keystone-edge/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/volcengine/volcengine-go-sdk/service/sts"
	"github.com/volcengine/volcengine-go-sdk/volcengine"
	"github.com/volcengine/volcengine-go-sdk/volcengine/request"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type fakeEpisodeQASTSClient struct{}

func (fakeEpisodeQASTSClient) AssumeRoleWithContext(volcengine.Context, *sts.AssumeRoleInput, ...request.Option) (*sts.AssumeRoleOutput, error) {
	return (&sts.AssumeRoleOutput{}).SetCredentials(
		(&sts.CredentialsForAssumeRoleOutput{}).
			SetAccessKeyId("temp-ak").
			SetSecretAccessKey("temp-sk").
			SetSessionToken("temp-token").
			SetExpiredTime("2026-07-15T08:00:00Z"),
	), nil
}

func TestStorageHandlerProxiesTOSRangeResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := NewStorageHandler(nil, nil, &config.StorageConfig{
		Type:       "tos",
		Endpoint:   "tos-cn-beijing.volces.com",
		Region:     "cn-beijing",
		AccessKey:  "test-ak",
		SecretKey:  "test-sk",
		UseSSL:     true,
		STSRoleTRN: "trn:iam::123:role/qa-read",
	})
	handler.tos.stsClient = fakeEpisodeQASTSClient{}
	handler.tos.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.Header.Get("Range"); got != "bytes=0-0" {
			t.Fatalf("Range = %q, want bytes=0-0", got)
		}
		if got := req.URL.Host; got != "bucket-a.tos-cn-beijing.volces.com" {
			t.Fatalf("host = %q, want bucket-a.tos-cn-beijing.volces.com", got)
		}
		resp := &http.Response{
			StatusCode:    http.StatusPartialContent,
			Header:        make(http.Header),
			Body:          io.NopCloser(strings.NewReader("x")),
			ContentLength: 1,
		}
		resp.Header.Set("Content-Range", "bytes 0-0/1024")
		resp.Header.Set("Content-Type", "application/octet-stream")
		return resp, nil
	})}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/storage/object", nil)
	req.Header.Set("Range", "bytes=0-0")
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = req

	handler.getTOSObject(ctx, "bucket-a", "device-uploads/capture.mcap", false)

	if w.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want %d body=%s", w.Code, http.StatusPartialContent, w.Body.String())
	}
	if got := w.Header().Get("Content-Range"); got != "bytes 0-0/1024" {
		t.Fatalf("Content-Range = %q, want bytes 0-0/1024", got)
	}
	if got := w.Header().Get("Content-Length"); got != "1" {
		t.Fatalf("Content-Length = %q, want 1", got)
	}
	if got := w.Body.String(); got != "x" {
		t.Fatalf("body = %q, want x", got)
	}
}
