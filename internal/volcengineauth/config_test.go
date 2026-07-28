// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package volcengineauth

import (
	"testing"

	"github.com/volcengine/volcengine-go-sdk/volcengine/credentials"
)

func TestNewConfigKeepsDefaultChainWhenNoStaticOrOIDC(t *testing.T) {
	t.Setenv("VOLCENGINE_OIDC_TOKEN_FILE", "")
	t.Setenv("VOLCENGINE_OIDC_ROLE_TRN", "")
	t.Setenv("VOLCENGINE_OIDC_STS_ENDPOINT", "")

	cfg := NewConfig("cn-beijing", "https://sts.volcengineapi.com", "", "")

	if cfg.Credentials != nil {
		t.Fatalf("Credentials = %#v, want nil so SDK default chain remains available", cfg.Credentials.GetProvider())
	}
}

func TestNewConfigUsesStaticCredentialsBeforeOIDC(t *testing.T) {
	t.Setenv("VOLCENGINE_OIDC_TOKEN_FILE", "/var/run/secrets/vke.volcengine.com/irsa-tokens/token")
	t.Setenv("VOLCENGINE_OIDC_ROLE_TRN", "trn:iam::123:role/irsa")
	t.Setenv("VOLCENGINE_OIDC_STS_ENDPOINT", "")

	cfg := NewConfig("cn-beijing", "https://sts.volcengineapi.com", "ak", "sk")

	if cfg.Credentials == nil {
		t.Fatal("Credentials = nil, want static credentials")
	}
	if _, ok := cfg.Credentials.GetProvider().(*credentials.StaticProvider); !ok {
		t.Fatalf("provider = %T, want *credentials.StaticProvider", cfg.Credentials.GetProvider())
	}
}

func TestNewConfigUsesExplicitOIDCWithoutDefaultFallback(t *testing.T) {
	t.Setenv("VOLCENGINE_OIDC_TOKEN_FILE", "/var/run/secrets/vke.volcengine.com/irsa-tokens/token")
	t.Setenv("VOLCENGINE_OIDC_ROLE_TRN", "trn:iam::123:role/keystone-irsa")
	t.Setenv("VOLCENGINE_OIDC_STS_ENDPOINT", "")

	cfg := NewConfig("cn-beijing", "https://sts.volcengineapi.com", "", "")

	if cfg.Credentials == nil {
		t.Fatal("Credentials = nil, want explicit OIDC credentials")
	}
	provider, ok := cfg.Credentials.GetProvider().(*credentials.OIDCCredentialsProvider)
	if !ok {
		t.Fatalf("provider = %T, want *credentials.OIDCCredentialsProvider", cfg.Credentials.GetProvider())
	}
	if provider.OIDCTokenFilePath != "/var/run/secrets/vke.volcengine.com/irsa-tokens/token" {
		t.Fatalf("OIDCTokenFilePath = %q", provider.OIDCTokenFilePath)
	}
	if provider.RoleTrn != "trn:iam::123:role/keystone-irsa" {
		t.Fatalf("RoleTrn = %q", provider.RoleTrn)
	}
	if provider.DurationSeconds != oidcSessionDurationSeconds {
		t.Fatalf("DurationSeconds = %d, want %d", provider.DurationSeconds, oidcSessionDurationSeconds)
	}
	if provider.Schema != "https" || provider.Endpoint != "sts.volcengineapi.com" {
		t.Fatalf("OIDC endpoint = %s://%s, want https://sts.volcengineapi.com", provider.Schema, provider.Endpoint)
	}
}

func TestNewConfigUsesOIDCEnvEndpointBeforeServiceEndpoint(t *testing.T) {
	t.Setenv("VOLCENGINE_OIDC_TOKEN_FILE", "/token")
	t.Setenv("VOLCENGINE_OIDC_ROLE_TRN", "trn:iam::123:role/keystone-irsa")
	t.Setenv("VOLCENGINE_OIDC_STS_ENDPOINT", "http://oidc-sts.internal")

	cfg := NewConfig("cn-beijing", "https://sts.volcengineapi.com", "", "")

	provider, ok := cfg.Credentials.GetProvider().(*credentials.OIDCCredentialsProvider)
	if !ok {
		t.Fatalf("provider = %T, want *credentials.OIDCCredentialsProvider", cfg.Credentials.GetProvider())
	}
	if provider.Schema != "http" || provider.Endpoint != "oidc-sts.internal" {
		t.Fatalf("OIDC endpoint = %s://%s, want http://oidc-sts.internal", provider.Schema, provider.Endpoint)
	}
}

func TestNewConfigUsesExplicitOIDCWhenOIDCEnvIsPartial(t *testing.T) {
	t.Setenv("VOLCENGINE_OIDC_TOKEN_FILE", "/token")
	t.Setenv("VOLCENGINE_OIDC_ROLE_TRN", "")
	t.Setenv("VOLCENGINE_OIDC_STS_ENDPOINT", "")

	cfg := NewConfig("cn-beijing", "https://sts.volcengineapi.com", "", "")

	provider, ok := cfg.Credentials.GetProvider().(*credentials.OIDCCredentialsProvider)
	if !ok {
		t.Fatalf("provider = %T, want *credentials.OIDCCredentialsProvider", cfg.Credentials.GetProvider())
	}
	if provider.RoleTrn != "" {
		t.Fatalf("RoleTrn = %q, want empty so OIDC fails closed instead of falling through", provider.RoleTrn)
	}
}

func TestNormalizeOIDCEndpoint(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		wantScheme   string
		wantEndpoint string
	}{
		{
			name:         "empty",
			input:        "",
			wantScheme:   "https",
			wantEndpoint: "",
		},
		{
			name:         "host only",
			input:        "sts.volcengineapi.com",
			wantScheme:   "https",
			wantEndpoint: "sts.volcengineapi.com",
		},
		{
			name:         "https url",
			input:        "https://sts.volcengineapi.com",
			wantScheme:   "https",
			wantEndpoint: "sts.volcengineapi.com",
		},
		{
			name:         "http url",
			input:        "http://sts.internal/",
			wantScheme:   "http",
			wantEndpoint: "sts.internal",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotScheme, gotEndpoint := normalizeOIDCEndpoint(tt.input)
			if gotScheme != tt.wantScheme || gotEndpoint != tt.wantEndpoint {
				t.Fatalf("normalizeOIDCEndpoint(%q) = %s, %s; want %s, %s", tt.input, gotScheme, gotEndpoint, tt.wantScheme, tt.wantEndpoint)
			}
		})
	}
}
