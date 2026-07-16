// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package tos provides a small native Volcengine TOS object reader.
package tos

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"archebase.com/keystone-edge/internal/config"
	"archebase.com/keystone-edge/internal/logger"
	"github.com/volcengine/volcengine-go-sdk/service/sts"
	"github.com/volcengine/volcengine-go-sdk/volcengine"
	"github.com/volcengine/volcengine-go-sdk/volcengine/credentials"
	"github.com/volcengine/volcengine-go-sdk/volcengine/request"
	"github.com/volcengine/volcengine-go-sdk/volcengine/session"
	"github.com/volcengine/volcengine-go-sdk/volcengine/volcengineerr"
)

const (
	algorithm   = "TOS4-HMAC-SHA256"
	service     = "tos"
	requestTerm = "request"
)

type volcengineSTSClient interface {
	AssumeRoleWithContext(ctx volcengine.Context, input *sts.AssumeRoleInput, opts ...request.Option) (*sts.AssumeRoleOutput, error)
}

type credentialsSet struct {
	accessKeyID     string
	accessKeySecret string
	securityToken   string
	expiration      time.Time
}

// Reader reads objects from native TOS endpoints using TOS4 request signing.
type Reader struct {
	endpoint  string
	region    string
	accessKey string
	secretKey string
	roleTRN   string
	stsClient volcengineSTSClient
	useSSL    bool
	client    *http.Client
}

// Error is a parsed TOS HTTP error response.
type Error struct {
	StatusCode int
	Code       string
	Message    string
	RequestID  string
	EC         string
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("tos status=%d code=%s message=%s request_id=%s ec=%s", e.StatusCode, e.Code, e.Message, e.RequestID, e.EC)
}

// NewReader creates a native TOS reader from Keystone storage config.
func NewReader(cfg config.StorageConfig, timeout time.Duration) *Reader {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	reader := &Reader{
		endpoint:  strings.TrimSpace(cfg.Endpoint),
		region:    strings.TrimSpace(cfg.Region),
		accessKey: strings.TrimSpace(cfg.AccessKey),
		secretKey: strings.TrimSpace(cfg.SecretKey),
		roleTRN:   strings.TrimSpace(cfg.STSRoleTRN),
		useSSL:    cfg.UseSSL,
		client:    &http.Client{Timeout: timeout},
	}
	if reader.roleTRN != "" {
		sdkConfig := volcengine.NewConfig().
			WithRegion(reader.region).
			WithCredentials(credentials.NewStaticCredentials(reader.accessKey, reader.secretKey, ""))
		if strings.TrimSpace(cfg.STSEndpoint) != "" {
			sdkConfig = sdkConfig.WithEndpoint(strings.TrimSpace(cfg.STSEndpoint))
		}
		if sess, err := session.NewSession(sdkConfig); err == nil {
			reader.stsClient = sts.New(sess)
		}
	}
	return reader
}

// StatObject returns the object size.
func (r *Reader) StatObject(ctx context.Context, bucket, objectName string) (int64, error) {
	req, err := r.newRequest(ctx, http.MethodHead, bucket, objectName, nil)
	if err != nil {
		return 0, err
	}
	resp, err := r.client.Do(req) // #nosec G704 -- TOS endpoint comes from Keystone storage config and objectURL validation.
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, errorFromResponse(resp)
	}
	size, err := strconv.ParseInt(strings.TrimSpace(resp.Header.Get("Content-Length")), 10, 64)
	if err != nil || size < 0 {
		return 0, fmt.Errorf("tos head missing valid content length")
	}
	return size, nil
}

// OpenObject opens the object body for streaming.
func (r *Reader) OpenObject(ctx context.Context, bucket, objectName string) (io.ReadCloser, error) {
	req, err := r.newRequest(ctx, http.MethodGet, bucket, objectName, nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.client.Do(req) // #nosec G704 -- TOS endpoint comes from Keystone storage config and objectURL validation.
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		logger.Printf("[TOS] GetObject failed bucket=%s object=%s status=%d request_id=%s",
			bucket, objectName, resp.StatusCode, resp.Header.Get("x-tos-request-id"))
		return nil, errorFromResponse(resp)
	}
	return resp.Body, nil
}

func (r *Reader) newRequest(ctx context.Context, method, bucket, objectName string, body []byte) (*http.Request, error) {
	u, err := r.objectURL(bucket, objectName)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	creds, err := r.credentials(ctx, bucket, objectName)
	if err != nil {
		return nil, err
	}
	if err := r.sign(req, body, time.Now().UTC(), creds); err != nil {
		return nil, err
	}
	return req, nil
}

func (r *Reader) credentials(ctx context.Context, bucket, objectName string) (credentialsSet, error) {
	if strings.TrimSpace(r.roleTRN) == "" {
		if r.accessKey == "" || r.secretKey == "" {
			return credentialsSet{}, fmt.Errorf("tos static credentials are incomplete")
		}
		return credentialsSet{accessKeyID: r.accessKey, accessKeySecret: r.secretKey}, nil
	}
	if r.stsClient == nil {
		return credentialsSet{}, fmt.Errorf("volcengine STS client is not configured for TOS reads")
	}
	policy, err := readPolicy(bucket, objectName)
	if err != nil {
		return credentialsSet{}, err
	}
	sessionName := fmt.Sprintf("keystone-sync-read-%d", time.Now().UTC().Unix())
	logger.Printf("[TOS] AssumeRole start role_trn=%s session_name=%s bucket=%s object=%s policy_sha256=%s policy_bytes=%d",
		r.roleTRN, sessionName, bucket, objectName, sha256Hex([]byte(policy)), len(policy))
	output, err := r.stsClient.AssumeRoleWithContext(ctx, (&sts.AssumeRoleInput{}).
		SetDurationSeconds(900).
		SetPolicy(policy).
		SetRoleSessionName(sessionName).
		SetRoleTrn(r.roleTRN))
	if err != nil {
		var sdkErr volcengineerr.Error
		if errors.As(err, &sdkErr) {
			logger.Printf("[TOS] AssumeRole failed role_trn=%s bucket=%s object=%s error_code=%s",
				r.roleTRN, bucket, objectName, sdkErr.Code())
			return credentialsSet{}, fmt.Errorf("volcengine STS AssumeRole for TOS read failed: %s", sdkErr.Code())
		}
		logger.Printf("[TOS] AssumeRole failed role_trn=%s bucket=%s object=%s", r.roleTRN, bucket, objectName)
		return credentialsSet{}, fmt.Errorf("volcengine STS AssumeRole for TOS read failed")
	}
	if output == nil || output.Credentials == nil ||
		output.Credentials.AccessKeyId == nil ||
		output.Credentials.SecretAccessKey == nil ||
		output.Credentials.SessionToken == nil {
		return credentialsSet{}, fmt.Errorf("volcengine STS response missing TOS read credentials")
	}
	result := credentialsSet{
		accessKeyID:     strings.TrimSpace(*output.Credentials.AccessKeyId),
		accessKeySecret: strings.TrimSpace(*output.Credentials.SecretAccessKey),
		securityToken:   strings.TrimSpace(*output.Credentials.SessionToken),
	}
	if output.Credentials.ExpiredTime != nil {
		if expiration, err := time.Parse(time.RFC3339, strings.TrimSpace(*output.Credentials.ExpiredTime)); err == nil {
			result.expiration = expiration.UTC()
		}
	}
	if result.accessKeyID == "" || result.accessKeySecret == "" || result.securityToken == "" {
		return credentialsSet{}, fmt.Errorf("volcengine STS response contains empty TOS read credentials")
	}
	logger.Printf("[TOS] AssumeRole success role_trn=%s bucket=%s object=%s expires_at=%s access_key_suffix=%s",
		r.roleTRN, bucket, objectName, result.expiration.Format(time.RFC3339), suffix(result.accessKeyID, 6))
	return result, nil
}

func (r *Reader) objectURL(bucket, objectName string) (string, error) {
	bucket = strings.TrimSpace(bucket)
	objectName = strings.Trim(strings.TrimSpace(objectName), "/")
	if bucket == "" || objectName == "" || r.endpoint == "" {
		return "", fmt.Errorf("tos object location is incomplete")
	}
	scheme := "http"
	if r.useSSL {
		scheme = "https"
	}
	host := strings.TrimPrefix(strings.TrimPrefix(strings.TrimRight(r.endpoint, "/"), "https://"), "http://")
	u := url.URL{
		Scheme: scheme,
		Host:   bucket + "." + host,
		Path:   "/" + objectName,
	}
	return u.String(), nil
}

func (r *Reader) sign(req *http.Request, payload []byte, now time.Time, creds credentialsSet) error {
	if req == nil || req.URL == nil || req.URL.Host == "" || r.region == "" || creds.accessKeyID == "" || creds.accessKeySecret == "" {
		return fmt.Errorf("tos signing configuration is incomplete")
	}
	timestamp := now.Format("20060102T150405Z")
	shortDate := timestamp[:8]
	payloadHash := sha256Hex(payload)
	req.Host = req.URL.Host
	req.Header.Set("Host", req.URL.Host)
	req.Header.Set("x-tos-date", timestamp)
	req.Header.Set("x-tos-content-sha256", payloadHash)
	if strings.TrimSpace(creds.securityToken) != "" {
		req.Header.Set("x-tos-security-token", strings.TrimSpace(creds.securityToken))
	}

	signedHeaderNames := []string{"host", "x-tos-content-sha256", "x-tos-date"}
	if strings.TrimSpace(creds.securityToken) != "" {
		signedHeaderNames = append(signedHeaderNames, "x-tos-security-token")
	}
	sort.Strings(signedHeaderNames)
	canonicalHeaders := strings.Builder{}
	for _, name := range signedHeaderNames {
		canonicalHeaders.WriteString(name)
		canonicalHeaders.WriteString(":")
		canonicalHeaders.WriteString(strings.TrimSpace(req.Header.Get(name)))
		canonicalHeaders.WriteString("\n")
	}
	signedHeaders := strings.Join(signedHeaderNames, ";")
	canonicalRequest := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		req.URL.RawQuery,
		canonicalHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")
	scope := strings.Join([]string{shortDate, r.region, service, requestTerm}, "/")
	stringToSign := strings.Join([]string{
		algorithm,
		timestamp,
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	dateKey := hmacSHA256([]byte(shortDate), []byte(creds.accessKeySecret))
	regionKey := hmacSHA256([]byte(r.region), dateKey)
	serviceKey := hmacSHA256([]byte(service), regionKey)
	signingKey := hmacSHA256([]byte(requestTerm), serviceKey)
	signature := hex.EncodeToString(hmacSHA256([]byte(stringToSign), signingKey))
	req.Header.Set("Authorization", fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s", algorithm, creds.accessKeyID, scope, signedHeaders, signature))
	return nil
}

func readPolicy(bucket, objectName string) (string, error) {
	bucket = strings.TrimSpace(bucket)
	objectName = strings.TrimSpace(objectName)
	if bucket == "" || objectName == "" {
		return "", fmt.Errorf("TOS read scope requires bucket and object key")
	}
	policy := map[string]any{
		"Statement": []map[string]any{{
			"Effect": "Allow",
			"Action": []string{
				"tos:GetObject",
				"tos:HeadObject",
			},
			"Resource": []string{"trn:tos:::" + bucket + "/" + objectName},
		}},
	}
	encoded, err := json.Marshal(policy)
	if err != nil {
		return "", fmt.Errorf("encode TOS read STS policy: %w", err)
	}
	return string(encoded), nil
}

func errorFromResponse(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var parsed struct {
		Code      string `xml:"Code"`
		Message   string `xml:"Message"`
		RequestID string `xml:"RequestId"`
		EC        string `xml:"EC"`
	}
	_ = xml.Unmarshal(data, &parsed)
	if parsed.Code == "" {
		parsed.Code = http.StatusText(resp.StatusCode)
	}
	return &Error{
		StatusCode: resp.StatusCode,
		Code:       parsed.Code,
		Message:    parsed.Message,
		RequestID:  parsed.RequestID,
		EC:         parsed.EC,
	}
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(data, key []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(data)
	return mac.Sum(nil)
}

func suffix(value string, n int) string {
	if n <= 0 || len(value) <= n {
		return value
	}
	return value[len(value)-n:]
}
