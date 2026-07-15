// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

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
	episodeQATOSAlgorithm = "TOS4-HMAC-SHA256"
	episodeQATOSService   = "tos"
	episodeQOSRequestTerm = "request"
)

type episodeQATOSReader struct {
	endpoint  string
	region    string
	accessKey string
	secretKey string
	roleTRN   string
	stsClient episodeQAVolcengineSTSClient
	useSSL    bool
	client    *http.Client
}

type episodeQAVolcengineSTSClient interface {
	AssumeRoleWithContext(ctx volcengine.Context, input *sts.AssumeRoleInput, opts ...request.Option) (*sts.AssumeRoleOutput, error)
}

type episodeQATOSCredentials struct {
	accessKeyID     string
	accessKeySecret string
	securityToken   string
	expiration      time.Time
}

type httpRange struct {
	start int64
	end   int64
}

type episodeQATOSObject struct {
	Data          []byte
	ContentRange  string
	ContentLength int64
}

type episodeQATOSError struct {
	StatusCode int
	Code       string
	Message    string
	RequestID  string
	EC         string
}

func (e *episodeQATOSError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("tos status=%d code=%s message=%s request_id=%s ec=%s", e.StatusCode, e.Code, e.Message, e.RequestID, e.EC)
}

func newEpisodeQATOSReader(cfg config.StorageConfig) *episodeQATOSReader {
	reader := &episodeQATOSReader{
		endpoint:  strings.TrimSpace(cfg.Endpoint),
		region:    strings.TrimSpace(cfg.Region),
		accessKey: strings.TrimSpace(cfg.AccessKey),
		secretKey: strings.TrimSpace(cfg.SecretKey),
		roleTRN:   strings.TrimSpace(cfg.STSRoleTRN),
		useSSL:    cfg.UseSSL,
		client:    &http.Client{Timeout: defaultEpisodeQATimeout},
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

func (r *episodeQATOSReader) StatObject(ctx context.Context, bucket, objectName string) (int64, error) {
	req, err := r.newRequest(ctx, http.MethodHead, bucket, objectName, nil, nil)
	if err != nil {
		return 0, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, tosErrorFromResponse(resp)
	}
	size, err := strconv.ParseInt(strings.TrimSpace(resp.Header.Get("Content-Length")), 10, 64)
	if err != nil || size < 0 {
		return 0, fmt.Errorf("tos head missing valid content length")
	}
	return size, nil
}

func (r *episodeQATOSReader) GetObject(ctx context.Context, bucket, objectName string, byteRange *httpRange) ([]byte, error) {
	object, err := r.GetObjectWithMetadata(ctx, bucket, objectName, byteRange)
	if err != nil {
		return nil, err
	}
	return object.Data, nil
}

func (r *episodeQATOSReader) GetObjectWithMetadata(ctx context.Context, bucket, objectName string, byteRange *httpRange) (episodeQATOSObject, error) {
	headers := http.Header{}
	if byteRange != nil {
		headers.Set("Range", fmt.Sprintf("bytes=%d-%d", byteRange.start, byteRange.end))
	}
	req, err := r.newRequest(ctx, http.MethodGet, bucket, objectName, headers, nil)
	if err != nil {
		return episodeQATOSObject{}, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return episodeQATOSObject{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		logger.Printf("[EPISODE-QA] TOS GetObject failed bucket=%s object=%s status=%d request_id=%s range=%s",
			bucket, objectName, resp.StatusCode, resp.Header.Get("x-tos-request-id"), headers.Get("Range"))
		return episodeQATOSObject{}, tosErrorFromResponse(resp)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return episodeQATOSObject{}, err
	}
	return episodeQATOSObject{
		Data:          data,
		ContentRange:  strings.TrimSpace(resp.Header.Get("Content-Range")),
		ContentLength: resp.ContentLength,
	}, nil
}

func (r *episodeQATOSReader) newRequest(ctx context.Context, method, bucket, objectName string, headers http.Header, body []byte) (*http.Request, error) {
	u, err := r.objectURL(bucket, objectName)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
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

func (r *episodeQATOSReader) credentials(ctx context.Context, bucket, objectName string) (episodeQATOSCredentials, error) {
	if strings.TrimSpace(r.roleTRN) == "" {
		return episodeQATOSCredentials{}, fmt.Errorf("KEYSTONE_DGW_VOLCENGINE_QA_READ_STS_ROLE_TRN is required for TOS QA reads")
	}
	if r.stsClient == nil {
		return episodeQATOSCredentials{}, fmt.Errorf("volcengine STS client is not configured for TOS QA reads")
	}
	policy, err := tosReadPolicy(bucket, objectName)
	if err != nil {
		return episodeQATOSCredentials{}, err
	}
	sessionName := fmt.Sprintf("keystone-qa-read-%d", time.Now().UTC().Unix())
	logger.Printf("[EPISODE-QA] TOS AssumeRole start role_trn=%s session_name=%s bucket=%s object=%s policy_sha256=%s policy_bytes=%d",
		r.roleTRN, sessionName, bucket, objectName, sha256Hex([]byte(policy)), len(policy))
	output, err := r.stsClient.AssumeRoleWithContext(ctx, (&sts.AssumeRoleInput{}).
		SetDurationSeconds(900).
		SetPolicy(policy).
		SetRoleSessionName(sessionName).
		SetRoleTrn(r.roleTRN))
	if err != nil {
		var sdkErr volcengineerr.Error
		if errors.As(err, &sdkErr) {
			logger.Printf("[EPISODE-QA] TOS AssumeRole failed role_trn=%s bucket=%s object=%s error_code=%s",
				r.roleTRN, bucket, objectName, sdkErr.Code())
			return episodeQATOSCredentials{}, fmt.Errorf("volcengine STS AssumeRole for QA read failed: %s", sdkErr.Code())
		}
		logger.Printf("[EPISODE-QA] TOS AssumeRole failed role_trn=%s bucket=%s object=%s",
			r.roleTRN, bucket, objectName)
		return episodeQATOSCredentials{}, fmt.Errorf("volcengine STS AssumeRole for QA read failed")
	}
	if output == nil || output.Credentials == nil ||
		output.Credentials.AccessKeyId == nil ||
		output.Credentials.SecretAccessKey == nil ||
		output.Credentials.SessionToken == nil {
		return episodeQATOSCredentials{}, fmt.Errorf("volcengine STS response missing QA read credentials")
	}
	result := episodeQATOSCredentials{
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
		return episodeQATOSCredentials{}, fmt.Errorf("volcengine STS response contains empty QA read credentials")
	}
	logger.Printf("[EPISODE-QA] TOS AssumeRole success role_trn=%s bucket=%s object=%s expires_at=%s access_key_suffix=%s",
		r.roleTRN,
		bucket,
		objectName,
		result.expiration.Format(time.RFC3339),
		suffix(result.accessKeyID, 6),
	)
	return result, nil
}

func (r *episodeQATOSReader) objectURL(bucket, objectName string) (string, error) {
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

func (r *episodeQATOSReader) sign(req *http.Request, payload []byte, now time.Time, creds episodeQATOSCredentials) error {
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
	scope := strings.Join([]string{shortDate, r.region, episodeQATOSService, episodeQOSRequestTerm}, "/")
	stringToSign := strings.Join([]string{
		episodeQATOSAlgorithm,
		timestamp,
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	dateKey := hmacSHA256([]byte(shortDate), []byte(creds.accessKeySecret))
	regionKey := hmacSHA256([]byte(r.region), dateKey)
	serviceKey := hmacSHA256([]byte(episodeQATOSService), regionKey)
	signingKey := hmacSHA256([]byte(episodeQOSRequestTerm), serviceKey)
	signature := hex.EncodeToString(hmacSHA256([]byte(stringToSign), signingKey))
	req.Header.Set("Authorization", fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s", episodeQATOSAlgorithm, creds.accessKeyID, scope, signedHeaders, signature))
	return nil
}

func tosReadPolicy(bucket, objectName string) (string, error) {
	bucket = strings.TrimSpace(bucket)
	objectName = strings.TrimSpace(objectName)
	if bucket == "" || objectName == "" {
		return "", fmt.Errorf("TOS QA read scope requires bucket and object key")
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
		return "", fmt.Errorf("encode TOS QA read STS policy: %w", err)
	}
	return string(encoded), nil
}

func tosErrorFromResponse(resp *http.Response) error {
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
	return &episodeQATOSError{
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
