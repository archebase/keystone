// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package cloud

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	tosNativeAlgorithm   = "TOS4-HMAC-SHA256"
	tosNativeService     = "tos"
	tosNativeRequestTerm = "request"
)

// TOSS3UploadTarget describes a single Hilbert-issued TOS upload target.
type TOSS3UploadTarget struct {
	Endpoint        string
	Region          string
	Bucket          string
	Key             string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// TOSS3Uploader uploads objects to Volcengine TOS through native TOS endpoints.
type TOSS3Uploader struct {
	timeout time.Duration
}

// NewTOSS3Uploader creates a native TOS uploader.
func NewTOSS3Uploader(timeout time.Duration) *TOSS3Uploader {
	return &TOSS3Uploader{timeout: timeout}
}

// PutObject streams one object to the Hilbert-issued target and returns the object ETag.
func (u *TOSS3Uploader) PutObject(ctx context.Context, target TOSS3UploadTarget, reader io.Reader, size int64, progress UploadProgressFunc) (string, error) {
	if size <= 0 {
		return "", fmt.Errorf("tos upload size must be positive")
	}
	endpoint, secure, err := normalizeTOSNativeEndpoint(target.Endpoint)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(target.Bucket) == "" || strings.TrimSpace(target.Key) == "" {
		return "", fmt.Errorf("tos upload target missing bucket or key")
	}
	if strings.TrimSpace(target.AccessKeyID) == "" || strings.TrimSpace(target.SecretAccessKey) == "" {
		return "", fmt.Errorf("tos upload target missing temporary credentials")
	}

	spooled, payloadHash, err := spoolTOSPayload(reader, size)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = spooled.Close()
		_ = os.Remove(spooled.Name())
	}()
	if _, err := spooled.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("rewind tos upload payload: %w", err)
	}

	req, err := newTOSPutObjectRequest(ctx, endpoint, secure, target, payloadHash, &progressReadCloser{reader: spooled, total: size, progress: progress}, size)
	if err != nil {
		return "", err
	}
	if err := signTOSRequest(req, target, payloadHash, time.Now().UTC()); err != nil {
		return "", err
	}

	client := &http.Client{Timeout: u.timeout}
	if u.timeout <= 0 {
		client.Timeout = 300 * time.Second
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("put tos object: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("put tos object: %w", tosUploadErrorFromResponse(resp))
	}
	if progress != nil {
		progress(size, size)
	}
	return strings.Trim(strings.TrimSpace(resp.Header.Get("ETag")), `"`), nil
}

func normalizeTOSNativeEndpoint(raw string) (endpoint string, secure bool, err error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", false, fmt.Errorf("tos endpoint is empty")
	}
	secure = true
	if strings.Contains(value, "://") {
		parsed, parseErr := url.Parse(value)
		if parseErr != nil {
			return "", false, fmt.Errorf("parse tos endpoint: %w", parseErr)
		}
		if parsed.Host == "" {
			return "", false, fmt.Errorf("tos endpoint missing host")
		}
		secure = parsed.Scheme != "http"
		value = parsed.Host
	}
	if strings.HasPrefix(value, "tos-s3-") && strings.HasSuffix(value, ".ivolces.com") {
		region := strings.TrimSuffix(strings.TrimPrefix(value, "tos-s3-"), ".ivolces.com")
		if region != "" {
			value = "tos-" + region + ".volces.com"
		}
	}
	return value, secure, nil
}

func spoolTOSPayload(reader io.Reader, size int64) (*os.File, string, error) {
	file, err := os.CreateTemp("", "keystone-hilbert-tos-*")
	if err != nil {
		return nil, "", fmt.Errorf("create tos upload temp file: %w", err)
	}
	remove := true
	defer func() {
		if remove {
			_ = file.Close()
			_ = os.Remove(file.Name())
		}
	}()

	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(file, hash), reader)
	if err != nil {
		return nil, "", fmt.Errorf("spool tos upload payload: %w", err)
	}
	if written != size {
		return nil, "", fmt.Errorf("spool tos upload payload size=%d, want %d", written, size)
	}
	remove = false
	return file, hex.EncodeToString(hash.Sum(nil)), nil
}

func newTOSPutObjectRequest(ctx context.Context, endpoint string, secure bool, target TOSS3UploadTarget, payloadHash string, body io.Reader, size int64) (*http.Request, error) {
	scheme := "http"
	if secure {
		scheme = "https"
	}
	objectKey := strings.Trim(strings.TrimSpace(target.Key), "/")
	u := url.URL{
		Scheme: scheme,
		Host:   strings.TrimSpace(target.Bucket) + "." + endpoint,
		Path:   "/" + objectKey,
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), body)
	if err != nil {
		return nil, fmt.Errorf("create tos put request: %w", err)
	}
	req.ContentLength = size
	req.Host = req.URL.Host
	req.Header.Set("Host", req.URL.Host)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("x-tos-content-sha256", payloadHash)
	return req, nil
}

func signTOSRequest(req *http.Request, target TOSS3UploadTarget, payloadHash string, now time.Time) error {
	region := strings.TrimSpace(target.Region)
	if req == nil || req.URL == nil || req.URL.Host == "" || region == "" || strings.TrimSpace(target.AccessKeyID) == "" || strings.TrimSpace(target.SecretAccessKey) == "" {
		return fmt.Errorf("tos signing configuration is incomplete")
	}
	timestamp := now.Format("20060102T150405Z")
	shortDate := timestamp[:8]
	req.Header.Set("x-tos-date", timestamp)
	if strings.TrimSpace(target.SessionToken) != "" {
		req.Header.Set("x-tos-security-token", strings.TrimSpace(target.SessionToken))
	}

	signedHeaderNames := []string{"host", "x-tos-content-sha256", "x-tos-date"}
	if strings.TrimSpace(target.SessionToken) != "" {
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
	scope := strings.Join([]string{shortDate, region, tosNativeService, tosNativeRequestTerm}, "/")
	stringToSign := strings.Join([]string{
		tosNativeAlgorithm,
		timestamp,
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	dateKey := hmacSHA256([]byte(shortDate), []byte(target.SecretAccessKey))
	regionKey := hmacSHA256([]byte(region), dateKey)
	serviceKey := hmacSHA256([]byte(tosNativeService), regionKey)
	signingKey := hmacSHA256([]byte(tosNativeRequestTerm), serviceKey)
	signature := hex.EncodeToString(hmacSHA256([]byte(stringToSign), signingKey))
	req.Header.Set("Authorization", fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s", tosNativeAlgorithm, target.AccessKeyID, scope, signedHeaders, signature))
	return nil
}

func tosUploadErrorFromResponse(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	parsed := struct {
		Code      string `json:"Code" xml:"Code"`
		Message   string `json:"Message" xml:"Message"`
		RequestID string `json:"RequestId" xml:"RequestId"`
		EC        string `json:"EC" xml:"EC"`
	}{}
	_ = json.Unmarshal(data, &parsed)
	if parsed.Code == "" {
		_ = xml.Unmarshal(data, &parsed)
	}
	if parsed.Code == "" {
		parsed.Code = http.StatusText(resp.StatusCode)
	}
	return fmt.Errorf("tos status=%d code=%s message=%s request_id=%s ec=%s body=%q", resp.StatusCode, parsed.Code, parsed.Message, parsed.RequestID, parsed.EC, strings.TrimSpace(string(data)))
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

type progressReadCloser struct {
	reader   io.Reader
	total    int64
	read     int64
	progress UploadProgressFunc
}

func (r *progressReadCloser) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.read += int64(n)
		if r.progress != nil {
			if r.read > r.total {
				r.read = r.total
			}
			r.progress(r.read, r.total)
		}
	}
	return n, err
}
