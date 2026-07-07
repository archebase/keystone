// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package dgwcompat

import (
	"context"
	"crypto/hmac"
	"crypto/sha1" // #nosec G505 -- Alibaba Cloud RPC API requires HMAC-SHA1 signing.
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

type stsCredentials struct {
	AccessKeyID     string
	AccessKeySecret string
	SecurityToken   string
	Expiration      time.Time
}

type stsProvider interface {
	AssumeRole(ctx context.Context) (stsCredentials, error)
}

type mockSTSProvider struct {
	ttl time.Duration
}

func (p mockSTSProvider) AssumeRole(context.Context) (stsCredentials, error) {
	expiration := time.Now().UTC().Add(p.ttl)
	return stsCredentials{
		AccessKeyID:     "mock-ak",
		AccessKeySecret: "mock-sk",
		SecurityToken:   "mock-token",
		Expiration:      expiration,
	}, nil
}

type aliyunSTSProvider struct {
	httpClient      *http.Client
	endpoint        string
	region          string
	roleARN         string
	sessionTTL      time.Duration
	accessKeyID     string
	accessKeySecret string
}

func newSTSProvider(cfg Config) stsProvider {
	if cfg.MockSTS {
		return mockSTSProvider{ttl: cfg.STSSessionTTL}
	}
	return &aliyunSTSProvider{
		httpClient:      &http.Client{Timeout: 10 * time.Second},
		endpoint:        cfg.STSEndpoint,
		region:          cfg.STSRegion,
		roleARN:         cfg.STSRoleARN,
		sessionTTL:      cfg.STSSessionTTL,
		accessKeyID:     cfg.AccessKeyID,
		accessKeySecret: cfg.AccessKeySecret,
	}
}

func (p *aliyunSTSProvider) AssumeRole(ctx context.Context) (stsCredentials, error) {
	durationSeconds := int64(p.sessionTTL.Seconds())
	if durationSeconds <= 0 {
		durationSeconds = 3600
	}
	params := []queryParam{
		{Key: "Action", Value: "AssumeRole"},
		{Key: "Format", Value: "XML"},
		{Key: "Version", Value: "2015-04-01"},
		{Key: "AccessKeyId", Value: p.accessKeyID},
		{Key: "SignatureMethod", Value: "HMAC-SHA1"},
		{Key: "SignatureVersion", Value: "1.0"},
		{Key: "SignatureNonce", Value: uuid.NewString()},
		{Key: "Timestamp", Value: time.Now().UTC().Format("2006-01-02T15:04:05Z")},
		{Key: "RegionId", Value: p.region},
		{Key: "RoleArn", Value: p.roleARN},
		{Key: "RoleSessionName", Value: "keystone-ego-upload"},
		{Key: "DurationSeconds", Value: fmt.Sprintf("%d", durationSeconds)},
	}
	canonicalized := canonicalizeQuery(params)
	toSign := "GET&" + percentEncode("/") + "&" + percentEncode(canonicalized)
	params = append(params, queryParam{Key: "Signature", Value: signQuery(toSign, p.accessKeySecret)})

	requestURL := p.endpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return stsCredentials{}, fmt.Errorf("create sts request: %w", err)
	}
	req.URL.RawQuery = encodeRPCQuery(params)

	resp, err := p.httpClient.Do(req) //nolint:gosec // G107: STS endpoint is operator-provided deployment config.
	if err != nil {
		return stsCredentials{}, fmt.Errorf("sts assume role request: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return stsCredentials{}, fmt.Errorf("read sts response: %w", err)
	}
	if resp.StatusCode >= 300 || strings.Contains(string(body), "<Error>") {
		return stsCredentials{}, fmt.Errorf("sts assume role failed status=%d body=%s", resp.StatusCode, string(body))
	}

	var envelope assumeRoleEnvelope
	if err := xml.Unmarshal(body, &envelope); err != nil {
		return stsCredentials{}, fmt.Errorf("parse sts response: %w", err)
	}
	expiration, err := time.Parse(time.RFC3339, strings.TrimSpace(envelope.Credentials.Expiration))
	if err != nil {
		return stsCredentials{}, fmt.Errorf("parse sts expiration: %w", err)
	}
	credentials := envelope.Credentials
	if credentials.AccessKeyID == "" || credentials.AccessKeySecret == "" || credentials.SecurityToken == "" {
		return stsCredentials{}, fmt.Errorf("sts response missing credentials")
	}
	return stsCredentials{
		AccessKeyID:     credentials.AccessKeyID,
		AccessKeySecret: credentials.AccessKeySecret,
		SecurityToken:   credentials.SecurityToken,
		Expiration:      expiration.UTC(),
	}, nil
}

type assumeRoleEnvelope struct {
	XMLName     xml.Name                `xml:"AssumeRoleResponse"`
	Credentials assumeRoleCredentialXML `xml:"Credentials"`
}

type assumeRoleCredentialXML struct {
	AccessKeyID     string `xml:"AccessKeyId"`
	AccessKeySecret string `xml:"AccessKeySecret"`
	SecurityToken   string `xml:"SecurityToken"`
	Expiration      string `xml:"Expiration"`
}

type queryParam struct {
	Key   string
	Value string
}

func canonicalizeQuery(params []queryParam) string {
	sorted := make([]queryParam, len(params))
	copy(sorted, params)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Key == sorted[j].Key {
			return sorted[i].Value < sorted[j].Value
		}
		return sorted[i].Key < sorted[j].Key
	})
	parts := make([]string, 0, len(sorted))
	for _, p := range sorted {
		parts = append(parts, percentEncode(p.Key)+"="+percentEncode(p.Value))
	}
	return strings.Join(parts, "&")
}

func encodeRPCQuery(params []queryParam) string {
	parts := make([]string, 0, len(params))
	for _, p := range params {
		parts = append(parts, url.QueryEscape(p.Key)+"="+url.QueryEscape(p.Value))
	}
	return strings.Join(parts, "&")
}

func percentEncode(value string) string {
	encoded := url.QueryEscape(value)
	encoded = strings.ReplaceAll(encoded, "+", "%20")
	encoded = strings.ReplaceAll(encoded, "*", "%2A")
	encoded = strings.ReplaceAll(encoded, "%7E", "~")
	return encoded
}

func signQuery(stringToSign, secret string) string {
	mac := hmac.New(sha1.New, []byte(secret+"&"))
	mac.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
