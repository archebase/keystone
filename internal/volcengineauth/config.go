// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package volcengineauth centralizes Volcengine SDK credential selection.
package volcengineauth

import (
	"strings"

	"github.com/volcengine/volcengine-go-sdk/volcengine"
	"github.com/volcengine/volcengine-go-sdk/volcengine/credentials"
)

// NewConfig returns a Volcengine SDK config. Static AK/SK are used only when
// both values are provided; otherwise the SDK default chain handles IRSA/OIDC,
// environment credentials, CLI credentials, or ECS role credentials.
func NewConfig(region, endpoint, accessKeyID, accessKeySecret string) *volcengine.Config {
	cfg := volcengine.NewConfig().
		WithRegion(strings.TrimSpace(region)).
		WithCredentialsChainVerboseErrors(true)

	accessKeyID = strings.TrimSpace(accessKeyID)
	accessKeySecret = strings.TrimSpace(accessKeySecret)
	if accessKeyID != "" && accessKeySecret != "" {
		cfg = cfg.WithCredentials(credentials.NewStaticCredentials(accessKeyID, accessKeySecret, ""))
	}

	if endpoint = strings.TrimSpace(endpoint); endpoint != "" {
		cfg = cfg.WithEndpoint(endpoint)
	}
	return cfg
}
