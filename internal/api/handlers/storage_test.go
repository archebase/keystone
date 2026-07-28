// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"archebase.com/keystone-edge/internal/auth"
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

func TestStorageHandlerGetObjectAllowsTOSWithoutS3(t *testing.T) {
	gin.SetMode(gin.TestMode)

	authCfg := &config.AuthConfig{JWTSecret: "test-secret"}
	handler := NewStorageHandler(nil, authCfg, &config.StorageConfig{
		Type:       "tos",
		Endpoint:   "tos-cn-beijing.volces.com",
		Bucket:     "tos-bucket",
		Region:     "cn-beijing",
		AccessKey:  "test-ak",
		SecretKey:  "test-sk",
		UseSSL:     true,
		STSRoleTRN: "trn:iam::123:role/qa-read",
	})
	handler.tos.stsClient = fakeEpisodeQASTSClient{}
	handler.tos.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.URL.Host; got != "tos-bucket.tos-cn-beijing.volces.com" {
			t.Fatalf("host = %q, want tos-bucket.tos-cn-beijing.volces.com", got)
		}
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        make(http.Header),
			Body:          io.NopCloser(strings.NewReader("mcap")),
			ContentLength: 4,
		}, nil
	})}

	token, err := auth.SignStorageDownloadToken("tos-bucket", "device-uploads/capture.mcap", time.Minute, authCfg)
	if err != nil {
		t.Fatalf("sign download token: %v", err)
	}
	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/storage/object?bucket=tos-bucket&object=device-uploads/capture.mcap&dl_token="+url.QueryEscape(token),
		nil,
	)
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = req

	handler.GetObject(ctx)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if got := w.Body.String(); got != "mcap" {
		t.Fatalf("body = %q, want mcap", got)
	}
}

func TestStorageHandlerPresignReturnsProxyURLForTOSWithoutS3(t *testing.T) {
	gin.SetMode(gin.TestMode)

	authCfg := &config.AuthConfig{JWTSecret: "test-secret", JWTExpiryHours: 1}
	handler := NewStorageHandler(nil, authCfg, &config.StorageConfig{
		Type:       "tos",
		Endpoint:   "tos-cn-beijing.volces.com",
		Bucket:     "tos-bucket",
		Region:     "cn-beijing",
		AccessKey:  "test-ak",
		SecretKey:  "test-sk",
		UseSSL:     true,
		STSRoleTRN: "trn:iam::123:role/qa-read",
	})
	token, err := auth.GenerateToken(auth.NewCollectorClaims(1, "operator-1"), authCfg)
	if err != nil {
		t.Fatalf("sign bearer token: %v", err)
	}

	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/storage/presign?bucket=tos-bucket&object=device-uploads/capture.mcap&expires_seconds=60",
		nil,
	)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = req

	handler.PresignGetObject(ctx)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	var body presignResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	parsed, err := url.Parse(body.URL)
	if err != nil {
		t.Fatalf("parse proxy url: %v", err)
	}
	if got := parsed.Path; got != "/api/v1/storage/object" {
		t.Fatalf("path = %q, want /api/v1/storage/object", got)
	}
	values := parsed.Query()
	if got := values.Get("bucket"); got != "tos-bucket" {
		t.Fatalf("bucket = %q, want tos-bucket", got)
	}
	if got := values.Get("object"); got != "device-uploads/capture.mcap" {
		t.Fatalf("object = %q, want device-uploads/capture.mcap", got)
	}
	if err := auth.ParseStorageDownloadToken(values.Get("dl_token"), authCfg, "tos-bucket", "device-uploads/capture.mcap"); err != nil {
		t.Fatalf("download token invalid: %v", err)
	}
}

func TestStorageHandlersRouteOnlyConfiguredTOSBucketToTOS(t *testing.T) {
	tosCfg := &config.StorageConfig{
		Type:       "tos",
		Endpoint:   "tos-cn-beijing.volces.com",
		Bucket:     "tos-bucket",
		Region:     "cn-beijing",
		STSRoleTRN: "trn:iam::123:role/qa-read",
	}

	storageHandler := NewStorageHandler(nil, nil, tosCfg)
	if storageHandler.usesTOSBucket("minio-bucket") {
		t.Fatal("MinIO bucket routed to TOS")
	}
	if !storageHandler.usesTOSBucket("tos-bucket") {
		t.Fatal("configured TOS bucket was not routed to TOS")
	}

	qaHandler := NewEpisodeQAHandler(nil, nil, "minio-bucket", nil, tosCfg)
	if qaHandler.usesTOSBucket("minio-bucket") {
		t.Fatal("QA routed MinIO bucket to TOS")
	}
	if !qaHandler.usesTOSBucket("tos-bucket") {
		t.Fatal("QA did not route configured TOS bucket to TOS")
	}
}
