// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package cloud

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"  //#nosec G501 -- MD5 required by OSS multipart ETag protocol
	"crypto/sha1" //#nosec G505 -- SHA1 required by OSS V1 signature
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"archebase.com/keystone-edge/internal/logger"
)

// OSSUploader provides Aliyun OSS multipart upload using V1 signature.
// This is a direct Go translation of data-platform/aliyun/ram/src/oss.rs.
type OSSUploader struct {
	httpClient *http.Client
}

// UploadedPart tracks a successfully uploaded OSS multipart part.
type UploadedPart struct {
	PartNumber int
	ETag       string
}

// MultipartUploadState tracks the current multipart upload progress.
type MultipartUploadState struct {
	UploadID          string
	MultipartUploadID string
	ObjectKey         string
	PartSizeBytes     int64
	UploadedParts     []UploadedPart
	LocalObjectETag   string
}

// NewOSSUploader creates a new OSS uploader.
func NewOSSUploader(timeout time.Duration) *OSSUploader {
	return &OSSUploader{
		httpClient: &http.Client{Timeout: timeout},
	}
}

// InitiateMultipartUpload starts a multipart upload and returns the OSS multipart upload ID.
func (u *OSSUploader) InitiateMultipartUpload(ctx context.Context, session *UploadSession) (string, error) {
	query := []queryParam{{Key: "uploads"}}
	resp, err := u.sendRequest(ctx, session, http.MethodPost, session.ObjectKey, query, nil, "")
	if err != nil {
		return "", fmt.Errorf("initiate multipart: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read initiate response: %w", err)
	}

	var envelope initiateMultipartUploadResult
	if err := xml.Unmarshal(body, &envelope); err != nil {
		return "", fmt.Errorf("parse initiate response: %w", err)
	}
	if strings.TrimSpace(envelope.UploadID) == "" {
		return "", fmt.Errorf("missing UploadId in initiate response")
	}
	return envelope.UploadID, nil
}

// UploadPart uploads a single part and returns its ETag.
func (u *OSSUploader) UploadPart(ctx context.Context, session *UploadSession, multipartUploadID string, partNumber int, body []byte) (string, error) {
	query := []queryParam{
		{Key: "partNumber", Value: fmt.Sprintf("%d", partNumber)},
		{Key: "uploadId", Value: multipartUploadID},
	}
	resp, err := u.sendRequest(ctx, session, http.MethodPut, session.ObjectKey, query, body, "application/octet-stream")
	if err != nil {
		return "", fmt.Errorf("upload part %d: %w", partNumber, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	etag := resp.Header.Get("ETag")
	if etag == "" {
		return "", fmt.Errorf("missing ETag header for part %d", partNumber)
	}
	return etag, nil
}

// CompleteMultipartUpload completes the multipart upload and returns the final object ETag.
func (u *OSSUploader) CompleteMultipartUpload(ctx context.Context, session *UploadSession, multipartUploadID string, parts []UploadedPart) (string, error) {
	xmlBody := buildCompleteMultipartUploadXML(parts)
	query := []queryParam{{Key: "uploadId", Value: multipartUploadID}}
	resp, err := u.sendRequest(ctx, session, http.MethodPost, session.ObjectKey, query, []byte(xmlBody), "application/xml")
	if err != nil {
		return "", fmt.Errorf("complete multipart: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read complete response: %w", err)
	}

	var envelope completeMultipartUploadResult
	if err := xml.Unmarshal(body, &envelope); err != nil {
		return "", fmt.Errorf("parse complete response: %w", err)
	}
	return envelope.ETag, nil
}

// AbortMultipartUpload aborts a multipart upload (best-effort cleanup).
func (u *OSSUploader) AbortMultipartUpload(ctx context.Context, session *UploadSession, multipartUploadID string) {
	query := []queryParam{{Key: "uploadId", Value: multipartUploadID}}
	resp, err := u.sendRequest(ctx, session, http.MethodDelete, session.ObjectKey, query, nil, "")
	if err != nil {
		logger.Printf("[OSS] Abort multipart upload failed: %v", err)
		return
	}
	_ = resp.Body.Close()
}

func (u *OSSUploader) sendRequest(ctx context.Context, session *UploadSession, method, objectKey string, query []queryParam, body []byte, contentType string) (*http.Response, error) {
	date := formatHTTPDate()

	extraHeaders := map[string]string{}
	if session.STSSecurityToken != "" {
		extraHeaders["x-oss-security-token"] = session.STSSecurityToken
	}

	authorization := buildAuthorizationHeader(
		session.STSAccessKeyID,
		session.STSAccessKeySecret,
		method,
		contentType,
		date,
		extraHeaders,
		session.Bucket,
		objectKey,
		query,
	)

	requestURL := buildRequestURL(session.Endpoint, session.Bucket, objectKey, query)

	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, requestURL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}

	req.Header.Set("Date", date)
	req.Header.Set("Authorization", authorization)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}

	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("oss object not found")
	}
	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("oss returned status %d: %s", resp.StatusCode, string(respBody))
	}
	return resp, nil
}

// --- OSS V1 Signature (mirrors data-platform/aliyun/ram/src/signer.rs) ---

func buildAuthorizationHeader(accessKeyID, accessKeySecret, method, contentType, date string, ossHeaders map[string]string, bucket, objectKey string, query []queryParam) string {
	canonHeaders := canonicalizedOSSHeaders(ossHeaders)
	canonResource := canonicalizedResource(bucket, objectKey, query)
	stringToSign := method + "\n\n" + contentType + "\n" + date + "\n" + canonHeaders + canonResource
	signature := signOSSV1(stringToSign, accessKeySecret)
	return "OSS " + accessKeyID + ":" + signature
}

// signOSSV1 signs a string using HMAC-SHA1 (OSS V1 signature).
func signOSSV1(stringToSign, secret string) string {
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func canonicalizedOSSHeaders(headers map[string]string) string {
	if len(headers) == 0 {
		return ""
	}
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, strings.ToLower(k))
	}
	sort.Strings(keys)

	var result string
	for _, k := range keys {
		result += k + ":" + strings.TrimSpace(headers[k]) + "\n"
	}
	return result
}

func canonicalizedResource(bucket, objectKey string, query []queryParam) string {
	resource := "/" + bucket + "/" + objectKey
	if len(query) == 0 {
		return resource
	}

	sorted := make([]queryParam, len(query))
	copy(sorted, query)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Key == sorted[j].Key {
			return sorted[i].Value < sorted[j].Value
		}
		return sorted[i].Key < sorted[j].Key
	})

	var subresources []string
	for _, q := range sorted {
		if q.Value != "" {
			subresources = append(subresources, q.Key+"="+q.Value)
		} else {
			subresources = append(subresources, q.Key)
		}
	}
	return resource + "?" + strings.Join(subresources, "&")
}

func formatHTTPDate() string {
	return time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT")
}

func buildRequestURL(endpoint, bucket, objectKey string, query []queryParam) string {
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		endpoint = "https://" + endpoint
	}

	parsed, err := url.Parse(endpoint)
	if err != nil {
		return endpoint + "/" + objectKey
	}

	host := parsed.Hostname()
	port := parsed.Port()
	encodedKey := encodeObjectKeyForPath(objectKey)

	// Use path-style for localhost / 127.0.0.1, virtual-hosted style otherwise.
	if host == "127.0.0.1" || host == "localhost" {
		parsed.Path = "/" + bucket + "/" + encodedKey
	} else {
		if port != "" {
			parsed.Host = bucket + "." + host + ":" + port
		} else {
			parsed.Host = bucket + "." + host
		}
		parsed.Path = "/" + encodedKey
	}

	if len(query) > 0 {
		parsed.RawQuery = encodeQueryString(query)
	}

	return parsed.String()
}

func encodeObjectKeyForPath(objectKey string) string {
	segments := strings.Split(objectKey, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return strings.Join(segments, "/")
}

func encodeQueryString(query []queryParam) string {
	var parts []string
	for _, q := range query {
		if q.Value != "" {
			parts = append(parts, url.QueryEscape(q.Key)+"="+url.QueryEscape(q.Value))
		} else {
			parts = append(parts, url.QueryEscape(q.Key))
		}
	}
	return strings.Join(parts, "&")
}

// --- Multipart ETag (mirrors Rust build_local_multipart_etag) ---

// MD5DigestBytes returns the MD5 digest of a byte slice.
func MD5DigestBytes(data []byte) [16]byte {
	return md5.Sum(data) //#nosec G401 -- MD5 required by OSS multipart ETag protocol
}

// BuildMultipartETag computes the local multipart ETag from per-part MD5 digests.
// Format: "\"<hex(md5(concat(hex(md5(part1)), hex(md5(part2)), ...)))>-<part_count>\""
func BuildMultipartETag(partMD5s [][16]byte) string {
	var hexParts string
	for _, digest := range partMD5s {
		hexParts += strings.ToUpper(hex.EncodeToString(digest[:]))
	}
	//#nosec G401 -- MD5 required by OSS multipart ETag protocol
	final := md5.Sum([]byte(hexParts))
	return fmt.Sprintf("\"%s-%d\"", strings.ToUpper(hex.EncodeToString(final[:])), len(partMD5s))
}

// --- XML types ---

type queryParam struct {
	Key   string
	Value string
}

type initiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	UploadID string   `xml:"UploadId"`
}

type completeMultipartUploadResult struct {
	XMLName xml.Name `xml:"CompleteMultipartUploadResult"`
	ETag    string   `xml:"ETag"`
}

func buildCompleteMultipartUploadXML(parts []UploadedPart) string {
	sorted := make([]UploadedPart, len(parts))
	copy(sorted, parts)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].PartNumber < sorted[j].PartNumber
	})

	var partsXML string
	for _, p := range sorted {
		partsXML += fmt.Sprintf("<Part><PartNumber>%d</PartNumber><ETag>%s</ETag></Part>", p.PartNumber, p.ETag)
	}
	return "<CompleteMultipartUpload>" + partsXML + "</CompleteMultipartUpload>"
}
