// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package cloud

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
)

const (
	tosNativeAlgorithm   = "TOS4-HMAC-SHA256"
	tosNativeService     = "tos"
	tosNativeRequestTerm = "request"

	tosMultipartThreshold      int64 = 128 * 1024 * 1024
	tosMultipartBasePartSize   int64 = 64 * 1024 * 1024
	tosMultipartMaxParts             = 10000
	tosMultipartPartAttempts         = 3
	tosMultipartRetryBaseDelay       = 200 * time.Millisecond
	tosMultipartAbortTimeout         = 30 * time.Second
)

// ErrTOSPayloadChecksumMismatch indicates that the source object no longer
// matches the SHA-256 checksum persisted for the episode.
var ErrTOSPayloadChecksumMismatch = errors.New("tos upload payload checksum mismatch")

// TOSS3UploadTarget describes a single Hilbert-issued TOS upload target.
type TOSS3UploadTarget struct {
	Endpoint        string
	Region          string
	Bucket          string
	Key             string
	AccessKeyID     string
	SecretAccessKey string
	TemporaryToken  string
}

// TOSS3Uploader uploads objects to Volcengine TOS through native TOS endpoints.
type TOSS3Uploader struct {
	timeout time.Duration

	// The remaining fields allow small, deterministic unit tests while the
	// production constructor continues to use the constants above.
	client             *http.Client
	multipartThreshold int64
	multipartPartSize  int64
	partAttempts       int
	retryBaseDelay     time.Duration
}

// NewTOSS3Uploader creates a native TOS uploader.
func NewTOSS3Uploader(timeout time.Duration) *TOSS3Uploader {
	return &TOSS3Uploader{timeout: timeout}
}

// PutObject streams one object to the Hilbert-issued target and returns the object ETag.
// Objects at least 128 MiB are uploaded sequentially in multipart chunks; smaller
// objects use one streaming PUT. Neither path spools the complete payload to disk.
func (u *TOSS3Uploader) PutObject(ctx context.Context, target TOSS3UploadTarget, reader io.Reader, size int64, payloadHash string, progress UploadProgressFunc) (string, error) {
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
	payloadHash, err = normalizeTOSSHA256(payloadHash)
	if err != nil {
		return "", err
	}

	if size >= u.effectiveMultipartThreshold() {
		return u.uploadMultipart(ctx, endpoint, secure, target, reader, size, payloadHash, progress)
	}
	return u.putObject(ctx, endpoint, secure, target, reader, size, payloadHash, progress)
}

func (u *TOSS3Uploader) putObject(ctx context.Context, endpoint string, secure bool, target TOSS3UploadTarget, reader io.Reader, size int64, payloadHash string, progress UploadProgressFunc) (string, error) {
	req, err := newTOSPutObjectRequest(ctx, endpoint, secure, target, payloadHash, &progressReadCloser{reader: reader, total: size, progress: progress}, size)
	if err != nil {
		return "", err
	}
	if err := signTOSRequest(req, target, payloadHash, time.Now().UTC()); err != nil {
		return "", err
	}

	resp, err := u.httpClient().Do(req) // #nosec G704 -- target endpoint is Hilbert-issued and normalized before request creation.
	if err != nil {
		return "", fmt.Errorf("put tos object: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("put tos object: %w", tosUploadErrorFromResponse(resp))
	}
	if progress != nil {
		progress(size, size)
	}
	return cleanTOSETag(resp.Header.Get("ETag")), nil
}

func (u *TOSS3Uploader) uploadMultipart(ctx context.Context, endpoint string, secure bool, target TOSS3UploadTarget, reader io.Reader, size int64, payloadHash string, progress UploadProgressFunc) (etag string, retErr error) {
	uploadID, err := u.createMultipartUpload(ctx, endpoint, secure, target)
	if err != nil {
		return "", err
	}

	completed := false
	defer func() {
		if completed {
			return
		}
		abortCtx, cancel := context.WithTimeout(context.Background(), tosMultipartAbortTimeout)
		defer cancel()
		if abortErr := u.abortMultipartUpload(abortCtx, endpoint, secure, target, uploadID); abortErr != nil && retErr != nil {
			retErr = fmt.Errorf("%w; abort tos multipart upload %s: %v", retErr, uploadID, abortErr)
		}
	}()

	partSize := multipartPartSize(size, u.effectiveMultipartPartSize())
	maxInt := int64(^uint(0) >> 1)
	if partSize > maxInt {
		return "", fmt.Errorf("tos multipart part size %d exceeds platform buffer limit", partSize)
	}
	partBuffer := make([]byte, int(partSize))
	wholeHash := sha256.New()
	completedBytes := int64(0)
	remaining := size
	parts := make([]tosCompletedPart, 0, multipartPartCount(size, partSize))

	for partNumber := 1; remaining > 0; partNumber++ {
		currentSize := min(remaining, partSize)
		part := partBuffer[:int(currentSize)]
		if _, err := io.ReadFull(io.TeeReader(reader, wholeHash), part); err != nil {
			return "", fmt.Errorf("read tos multipart part %d size=%d: %w", partNumber, currentSize, err)
		}
		partETag, err := u.uploadMultipartPart(ctx, endpoint, secure, target, uploadID, partNumber, part)
		if err != nil {
			return "", err
		}
		parts = append(parts, tosCompletedPart{PartNumber: partNumber, ETag: partETag})
		remaining -= currentSize
		completedBytes += currentSize
		if progress != nil {
			progress(completedBytes, size)
		}
	}

	var extra [1]byte
	if n, err := io.ReadFull(reader, extra[:]); n > 0 {
		return "", fmt.Errorf("tos upload payload exceeds declared size %d", size)
	} else if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("check tos upload payload size: %w", err)
	}

	actualHash := hex.EncodeToString(wholeHash.Sum(nil))
	if actualHash != payloadHash {
		return "", fmt.Errorf("%w: got %s want %s", ErrTOSPayloadChecksumMismatch, actualHash, payloadHash)
	}

	etag, err = u.completeMultipartUpload(ctx, endpoint, secure, target, uploadID, parts)
	if err != nil {
		return "", err
	}
	completed = true
	if progress != nil {
		progress(size, size)
	}
	return etag, nil
}

func (u *TOSS3Uploader) createMultipartUpload(ctx context.Context, endpoint string, secure bool, target TOSS3UploadTarget) (string, error) {
	query := url.Values{"uploads": {""}}
	payloadHash := sha256Hex(nil)
	req, err := newTOSObjectRequest(ctx, http.MethodPost, endpoint, secure, target, query, bytes.NewReader(nil), 0, payloadHash, "")
	if err != nil {
		return "", fmt.Errorf("create tos multipart request: %w", err)
	}
	if err := signTOSRequest(req, target, payloadHash, time.Now().UTC()); err != nil {
		return "", err
	}
	resp, err := u.httpClient().Do(req) // #nosec G704 -- target endpoint is Hilbert-issued and normalized before request creation.
	if err != nil {
		return "", fmt.Errorf("create tos multipart upload: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("create tos multipart upload: %w", tosUploadErrorFromResponse(resp))
	}

	result := struct {
		UploadID string `json:"UploadId"`
	}{}
	if err := decodeTOSJSONResponse(resp, &result); err != nil {
		return "", fmt.Errorf("decode create tos multipart response: %w", err)
	}
	if strings.TrimSpace(result.UploadID) == "" {
		return "", fmt.Errorf("create tos multipart response missing upload id (%s)", tosResponseDetails(resp, -1))
	}
	return strings.TrimSpace(result.UploadID), nil
}

func (u *TOSS3Uploader) uploadMultipartPart(ctx context.Context, endpoint string, secure bool, target TOSS3UploadTarget, uploadID string, partNumber int, part []byte) (string, error) {
	payloadHash := sha256Hex(part)
	var lastErr error
	for attempt := 1; attempt <= u.effectivePartAttempts(); attempt++ {
		query := url.Values{
			"partNumber": {strconv.Itoa(partNumber)},
			"uploadId":   {uploadID},
		}
		req, err := newTOSObjectRequest(ctx, http.MethodPut, endpoint, secure, target, query, bytes.NewReader(part), int64(len(part)), payloadHash, "application/octet-stream")
		if err != nil {
			return "", fmt.Errorf("create tos upload part %d request: %w", partNumber, err)
		}
		if err := signTOSRequest(req, target, payloadHash, time.Now().UTC()); err != nil {
			return "", err
		}

		resp, err := u.httpClient().Do(req) // #nosec G704 -- target endpoint is Hilbert-issued and normalized before request creation.
		if err != nil {
			lastErr = fmt.Errorf("upload tos multipart part %d attempt=%d: %w", partNumber, attempt, err)
		} else {
			if resp.StatusCode == http.StatusOK {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
				_ = resp.Body.Close()
				etag := strings.TrimSpace(resp.Header.Get("ETag"))
				if etag == "" {
					return "", fmt.Errorf("upload tos multipart part %d response missing ETag", partNumber)
				}
				return etag, nil
			}
			lastErr = fmt.Errorf("upload tos multipart part %d attempt=%d: %w", partNumber, attempt, tosUploadErrorFromResponse(resp))
			_ = resp.Body.Close()
		}

		if attempt == u.effectivePartAttempts() || !isRetryableTOSPartError(ctx, lastErr) {
			return "", lastErr
		}
		if err := waitTOSPartRetry(ctx, u.effectiveRetryBaseDelay(), attempt); err != nil {
			return "", fmt.Errorf("upload tos multipart part %d retry wait: %w", partNumber, err)
		}
	}
	return "", lastErr
}

func (u *TOSS3Uploader) completeMultipartUpload(ctx context.Context, endpoint string, secure bool, target TOSS3UploadTarget, uploadID string, parts []tosCompletedPart) (string, error) {
	payload, err := json.Marshal(tosCompleteMultipartUpload{Parts: parts})
	if err != nil {
		return "", fmt.Errorf("encode complete tos multipart request: %w", err)
	}
	payloadHash := sha256Hex(payload)
	query := url.Values{"uploadId": {uploadID}}
	req, err := newTOSObjectRequest(ctx, http.MethodPost, endpoint, secure, target, query, bytes.NewReader(payload), int64(len(payload)), payloadHash, "application/json")
	if err != nil {
		return "", fmt.Errorf("create complete tos multipart request: %w", err)
	}
	if err := signTOSRequest(req, target, payloadHash, time.Now().UTC()); err != nil {
		return "", err
	}
	resp, err := u.httpClient().Do(req) // #nosec G704 -- target endpoint is Hilbert-issued and normalized before request creation.
	if err != nil {
		return "", fmt.Errorf("complete tos multipart upload: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("complete tos multipart upload: %w", tosUploadErrorFromResponse(resp))
	}

	result := struct {
		ETag string `json:"ETag"`
	}{}
	if err := decodeTOSJSONResponse(resp, &result); err != nil {
		return "", fmt.Errorf("decode complete tos multipart response: %w", err)
	}
	if etag := cleanTOSETag(resp.Header.Get("ETag")); etag != "" {
		return etag, nil
	}
	if etag := cleanTOSETag(result.ETag); etag != "" {
		return etag, nil
	}
	return "", fmt.Errorf("complete tos multipart response missing ETag (%s)", tosResponseDetails(resp, -1))
}

func (u *TOSS3Uploader) abortMultipartUpload(ctx context.Context, endpoint string, secure bool, target TOSS3UploadTarget, uploadID string) error {
	payloadHash := sha256Hex(nil)
	query := url.Values{"uploadId": {uploadID}}
	req, err := newTOSObjectRequest(ctx, http.MethodDelete, endpoint, secure, target, query, nil, 0, payloadHash, "")
	if err != nil {
		return fmt.Errorf("create abort tos multipart request: %w", err)
	}
	if err := signTOSRequest(req, target, payloadHash, time.Now().UTC()); err != nil {
		return err
	}
	resp, err := u.httpClient().Do(req) // #nosec G704 -- target endpoint is Hilbert-issued and normalized before request creation.
	if err != nil {
		return fmt.Errorf("abort tos multipart upload: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("abort tos multipart upload: %w", tosUploadErrorFromResponse(resp))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return nil
}

func (u *TOSS3Uploader) httpClient() *http.Client {
	if u.client != nil {
		return u.client
	}
	timeout := u.timeout
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	return &http.Client{Timeout: timeout}
}

func (u *TOSS3Uploader) effectiveMultipartThreshold() int64 {
	if u.multipartThreshold > 0 {
		return u.multipartThreshold
	}
	return tosMultipartThreshold
}

func (u *TOSS3Uploader) effectiveMultipartPartSize() int64 {
	if u.multipartPartSize > 0 {
		return u.multipartPartSize
	}
	return tosMultipartBasePartSize
}

func (u *TOSS3Uploader) effectivePartAttempts() int {
	if u.partAttempts > 0 {
		return u.partAttempts
	}
	return tosMultipartPartAttempts
}

func (u *TOSS3Uploader) effectiveRetryBaseDelay() time.Duration {
	if u.retryBaseDelay < 0 {
		return 0
	}
	if u.retryBaseDelay > 0 {
		return u.retryBaseDelay
	}
	return tosMultipartRetryBaseDelay
}

func multipartPartSize(size, base int64) int64 {
	minimum := ceilDiv(size, tosMultipartMaxParts)
	if minimum <= base {
		return base
	}
	return ceilDiv(minimum, base) * base
}

func multipartPartCount(size, partSize int64) int {
	return int(ceilDiv(size, partSize))
}

func ceilDiv(value, divisor int64) int64 {
	if value <= 0 {
		return 0
	}
	return 1 + (value-1)/divisor
}

func waitTOSPartRetry(ctx context.Context, base time.Duration, attempt int) error {
	if base <= 0 {
		return nil
	}
	delay := base * time.Duration(1<<min(attempt-1, 10))
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func isRetryableTOSPartError(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	var uploadErr *tosUploadError
	if !errors.As(err, &uploadErr) {
		return true
	}
	return uploadErr.StatusCode == http.StatusRequestTimeout ||
		uploadErr.StatusCode == http.StatusConflict ||
		uploadErr.StatusCode == http.StatusTooManyRequests ||
		uploadErr.StatusCode >= http.StatusInternalServerError
}

func normalizeTOSSHA256(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != sha256.Size*2 {
		return "", fmt.Errorf("tos upload payload SHA-256 must contain 64 hexadecimal characters")
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", fmt.Errorf("tos upload payload SHA-256 is invalid: %w", err)
	}
	return value, nil
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

func newTOSPutObjectRequest(ctx context.Context, endpoint string, secure bool, target TOSS3UploadTarget, payloadHash string, body io.Reader, size int64) (*http.Request, error) {
	return newTOSObjectRequest(ctx, http.MethodPut, endpoint, secure, target, nil, body, size, payloadHash, "application/octet-stream")
}

func newTOSObjectRequest(ctx context.Context, method, endpoint string, secure bool, target TOSS3UploadTarget, query url.Values, body io.Reader, size int64, payloadHash, contentType string) (*http.Request, error) {
	scheme := "http"
	if secure {
		scheme = "https"
	}
	objectKey := strings.Trim(strings.TrimSpace(target.Key), "/")
	u := url.URL{
		Scheme:   scheme,
		Host:     strings.TrimSpace(target.Bucket) + "." + endpoint,
		Path:     "/" + objectKey,
		RawQuery: query.Encode(),
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, fmt.Errorf("create tos %s request: %w", method, err)
	}
	req.ContentLength = size
	req.Host = req.URL.Host
	req.Header.Set("Host", req.URL.Host)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
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
	if strings.TrimSpace(target.TemporaryToken) != "" {
		req.Header.Set("x-tos-security-token", strings.TrimSpace(target.TemporaryToken))
	}

	signedHeaderNames := []string{"host", "x-tos-content-sha256", "x-tos-date"}
	if strings.TrimSpace(target.TemporaryToken) != "" {
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

type tosUploadError struct {
	StatusCode  int
	Code        string
	Message     string
	RequestID   string
	EC          string
	ContentType string
	BodyLength  int
	Body        string
}

func (e *tosUploadError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("tos status=%d code=%s message=%s request_id=%s ec=%s content_type=%q body_length=%d body=%q", e.StatusCode, e.Code, e.Message, e.RequestID, e.EC, e.ContentType, e.BodyLength, e.Body)
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
	if parsed.RequestID == "" {
		parsed.RequestID = resp.Header.Get("x-tos-request-id")
	}
	if parsed.EC == "" {
		parsed.EC = resp.Header.Get("x-tos-ec")
	}
	return &tosUploadError{
		StatusCode:  resp.StatusCode,
		Code:        parsed.Code,
		Message:     parsed.Message,
		RequestID:   parsed.RequestID,
		EC:          parsed.EC,
		ContentType: resp.Header.Get("Content-Type"),
		BodyLength:  len(data),
		Body:        strings.TrimSpace(string(data)),
	}
}

type tosCompletedPart struct {
	PartNumber int    `json:"PartNumber"`
	ETag       string `json:"ETag"`
}

type tosCompleteMultipartUpload struct {
	Parts []tosCompletedPart `json:"Parts"`
}

func decodeTOSJSONResponse(resp *http.Response, destination any) error {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read response body (%s): %w", tosResponseDetails(resp, -1), err)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return fmt.Errorf("empty response body (%s)", tosResponseDetails(resp, len(body)))
	}
	if err := json.Unmarshal(body, destination); err != nil {
		return fmt.Errorf("invalid JSON (%s): %w", tosResponseDetails(resp, len(body)), err)
	}
	return nil
}

func tosResponseDetails(resp *http.Response, bodyLength int) string {
	if resp == nil {
		return "status=unknown request_id= content_type=\"\" body_length=unknown"
	}
	length := "unknown"
	if bodyLength >= 0 {
		length = strconv.Itoa(bodyLength)
	}
	return fmt.Sprintf("status=%d request_id=%s content_type=%q body_length=%s", resp.StatusCode, resp.Header.Get("x-tos-request-id"), resp.Header.Get("Content-Type"), length)
}

func cleanTOSETag(value string) string {
	return strings.Trim(strings.TrimSpace(value), `"`)
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
