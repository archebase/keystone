// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package volcengineauth centralizes Volcengine SDK credential selection.
package volcengineauth

import (
	"net/url"
	"os"
	"strings"

	"github.com/volcengine/volcengine-go-sdk/volcengine"
	"github.com/volcengine/volcengine-go-sdk/volcengine/credentials"
)

const oidcSessionDurationSeconds = 1800

// NewConfig returns a Volcengine SDK config. Static AK/SK are used only when
// both values are provided. When VKE injects IRSA/OIDC environment variables,
// OIDC is selected explicitly so the SDK does not fall through to ECS/node role
// credentials. Without static AK/SK or OIDC, the SDK default chain is preserved
// for local/offline use.
func NewConfig(region, endpoint, accessKeyID, accessKeySecret string) *volcengine.Config {
	cfg := volcengine.NewConfig().
		WithRegion(strings.TrimSpace(region)).
		WithCredentialsChainVerboseErrors(true)

	accessKeyID = strings.TrimSpace(accessKeyID)
	accessKeySecret = strings.TrimSpace(accessKeySecret)
	if accessKeyID != "" && accessKeySecret != "" {
		cfg = cfg.WithCredentials(credentials.NewStaticCredentials(accessKeyID, accessKeySecret, ""))
	} else if oidcTokenFile, oidcRoleTRN, ok := oidcEnv(); ok {
		scheme, oidcEndpoint := normalizeOIDCEndpoint(firstNonEmpty(os.Getenv("VOLCENGINE_OIDC_STS_ENDPOINT"), endpoint))
		cfg = cfg.WithCredentials(credentials.NewCredentials(credentials.NewOIDCCredentialsProviderWithOptions(
			oidcTokenFile,
			oidcRoleTRN,
			func(options *credentials.OIDCProviderOptions) {
				options.DurationSeconds = oidcSessionDurationSeconds
				options.Schema = scheme
				options.Endpoint = oidcEndpoint
			},
		)))
	}

	if endpoint = strings.TrimSpace(endpoint); endpoint != "" {
		cfg = cfg.WithEndpoint(endpoint)
	}
	return cfg
}

func oidcEnv() (string, string, bool) {
	tokenFile := strings.TrimSpace(os.Getenv("VOLCENGINE_OIDC_TOKEN_FILE"))
	roleTRN := strings.TrimSpace(os.Getenv("VOLCENGINE_OIDC_ROLE_TRN"))
	return tokenFile, roleTRN, tokenFile != "" || roleTRN != ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func normalizeOIDCEndpoint(raw string) (string, string) {
	value := strings.TrimRight(strings.TrimSpace(raw), "/")
	if value == "" {
		return "https", ""
	}
	parsed, err := url.Parse(value)
	if err == nil && parsed.Scheme != "" && parsed.Host != "" {
		return strings.ToLower(parsed.Scheme), parsed.Host
	}
	return "https", value
}
