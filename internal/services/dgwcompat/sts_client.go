// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package dgwcompat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/volcengine/volcengine-go-sdk/service/sts"
	"github.com/volcengine/volcengine-go-sdk/volcengine"
	"github.com/volcengine/volcengine-go-sdk/volcengine/credentials"
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
	sdkConfig := volcengine.NewConfig().
		WithRegion(cfg.TOSRegion).
		WithCredentials(credentials.NewStaticCredentials(cfg.AccessKeyID, cfg.AccessKeySecret, ""))
	if cfg.STSEndpoint != "" {
		sdkConfig = sdkConfig.WithEndpoint(cfg.STSEndpoint)
	}
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
	output, err := p.client.AssumeRoleWithContext(ctx, (&sts.AssumeRoleInput{}).
		SetDurationSeconds(durationSeconds).
		SetPolicy(policy).
		SetRoleSessionName(sessionName).
		SetRoleTrn(p.roleTRN))
	if err != nil {
		var sdkErr volcengineerr.Error
		if errors.As(err, &sdkErr) {
			return stsCredentials{}, fmt.Errorf("volcengine STS AssumeRole failed: %s", sdkErr.Code())
		}
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
	return result, nil
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
