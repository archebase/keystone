// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package dgwcompat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/volcengineauth"
	"github.com/volcengine/volcengine-go-sdk/service/sts"
	"github.com/volcengine/volcengine-go-sdk/volcengine"
	"github.com/volcengine/volcengine-go-sdk/volcengine/request"
	"github.com/volcengine/volcengine-go-sdk/volcengine/session"
	"github.com/volcengine/volcengine-go-sdk/volcengine/volcengineerr"
)

type stsCredentials struct {
	AccessKeyID     string
	AccessKeySecret string
	SecurityToken   string
	Expiration      time.Time
}

type stsScope struct {
	Bucket    string
	ObjectKey string
}

type stsProvider interface {
	AssumeRole(ctx context.Context, scope stsScope) (stsCredentials, error)
}

type mockSTSProvider struct {
	ttl time.Duration
}

func (p mockSTSProvider) AssumeRole(context.Context, stsScope) (stsCredentials, error) {
	expiration := time.Now().UTC().Add(p.ttl)
	return stsCredentials{
		AccessKeyID:     "mock-ak",
		AccessKeySecret: "mock-sk",
		SecurityToken:   "mock-token",
		Expiration:      expiration,
	}, nil
}

type volcengineSTSClient interface {
	AssumeRoleWithContext(ctx volcengine.Context, input *sts.AssumeRoleInput, opts ...request.Option) (*sts.AssumeRoleOutput, error)
}

type volcengineSTSProvider struct {
	client     volcengineSTSClient
	roleTRN    string
	sessionTTL time.Duration
}

func newSTSProvider(cfg Config) (stsProvider, error) {
	if cfg.MockSTS {
		return mockSTSProvider{ttl: cfg.STSSessionTTL}, nil
	}
	sdkConfig := volcengineauth.NewConfig(cfg.TOSRegion, cfg.STSEndpoint, cfg.AccessKeyID, cfg.AccessKeySecret)
	sess, err := session.NewSession(sdkConfig)
	if err != nil {
		return nil, fmt.Errorf("create Volcengine STS session: %w", err)
	}
	return &volcengineSTSProvider{
		client:     sts.New(sess),
		roleTRN:    cfg.STSRoleTRN,
		sessionTTL: cfg.STSSessionTTL,
	}, nil
}

func (p *volcengineSTSProvider) AssumeRole(ctx context.Context, scope stsScope) (stsCredentials, error) {
	policy, err := tosUploadPolicy(scope)
	if err != nil {
		return stsCredentials{}, err
	}
	durationSeconds := int32(p.sessionTTL.Seconds()) // #nosec G115 -- validated deployment TTL is intentionally bounded below.
	if durationSeconds <= 0 {
		durationSeconds = 900
	}
	sessionName := fmt.Sprintf("keystone-device-upload-%d", time.Now().UTC().Unix())
	logger.Printf("[DGW_COMPAT] STS AssumeRole start role_trn=%s session_name=%s bucket=%s object_key=%s duration_seconds=%d policy_sha256=%s policy_bytes=%d",
		p.roleTRN, sessionName, scope.Bucket, scope.ObjectKey, durationSeconds, sha256Hex(policy), len(policy))
	output, err := p.client.AssumeRoleWithContext(ctx, (&sts.AssumeRoleInput{}).
		SetDurationSeconds(durationSeconds).
		SetPolicy(policy).
		SetRoleSessionName(sessionName).
		SetRoleTrn(p.roleTRN))
	if err != nil {
		var sdkErr volcengineerr.Error
		if errors.As(err, &sdkErr) {
			logger.Printf("[DGW_COMPAT] STS AssumeRole failed role_trn=%s bucket=%s object_key=%s error_code=%s",
				p.roleTRN, scope.Bucket, scope.ObjectKey, sdkErr.Code())
			return stsCredentials{}, fmt.Errorf("volcengine STS AssumeRole failed: %s", sdkErr.Code())
		}
		logger.Printf("[DGW_COMPAT] STS AssumeRole failed role_trn=%s bucket=%s object_key=%s",
			p.roleTRN, scope.Bucket, scope.ObjectKey)
		return stsCredentials{}, fmt.Errorf("volcengine STS AssumeRole failed")
	}
	if output == nil || output.Credentials == nil {
		return stsCredentials{}, fmt.Errorf("volcengine STS response missing credentials")
	}
	creds := output.Credentials
	if creds.AccessKeyId == nil || creds.SecretAccessKey == nil || creds.SessionToken == nil || creds.ExpiredTime == nil {
		return stsCredentials{}, fmt.Errorf("volcengine STS response contains incomplete credentials")
	}
	expiration, err := time.Parse(time.RFC3339, strings.TrimSpace(*creds.ExpiredTime))
	if err != nil {
		return stsCredentials{}, fmt.Errorf("parse Volcengine STS expiration: %w", err)
	}
	result := stsCredentials{
		AccessKeyID:     strings.TrimSpace(*creds.AccessKeyId),
		AccessKeySecret: strings.TrimSpace(*creds.SecretAccessKey),
		SecurityToken:   strings.TrimSpace(*creds.SessionToken),
		Expiration:      expiration.UTC(),
	}
	if result.AccessKeyID == "" || result.AccessKeySecret == "" || result.SecurityToken == "" {
		return stsCredentials{}, fmt.Errorf("volcengine STS response contains empty credentials")
	}
	logger.Printf("[DGW_COMPAT] STS AssumeRole success role_trn=%s bucket=%s object_key=%s expires_at=%s ttl_seconds=%d access_key_suffix=%s",
		p.roleTRN,
		scope.Bucket,
		scope.ObjectKey,
		result.Expiration.Format(time.RFC3339),
		int(time.Until(result.Expiration).Seconds()),
		suffix(result.AccessKeyID, 6),
	)
	return result, nil
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func suffix(value string, n int) string {
	value = strings.TrimSpace(value)
	if n <= 0 || value == "" {
		return ""
	}
	if len(value) <= n {
		return value
	}
	return value[len(value)-n:]
}

func tosUploadPolicy(scope stsScope) (string, error) {
	bucket := strings.TrimSpace(scope.Bucket)
	objectKey := strings.TrimSpace(scope.ObjectKey)
	if bucket == "" || objectKey == "" {
		return "", fmt.Errorf("TOS STS scope requires bucket and object key")
	}
	policy := map[string]any{
		"Statement": []map[string]any{{
			"Effect": "Allow",
			"Action": []string{
				"tos:PutObject",
				"tos:CreateMultipartUpload",
				"tos:UploadPart",
				"tos:CompleteMultipartUpload",
				"tos:AbortMultipartUpload",
				"tos:ListParts",
				"tos:HeadObject",
			},
			"Resource": []string{"trn:tos:::" + bucket + "/" + objectKey},
		}},
	}
	encoded, err := json.Marshal(policy)
	if err != nil {
		return "", fmt.Errorf("encode TOS STS policy: %w", err)
	}
	return string(encoded), nil
}
