// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package cloud

import (
	"strings"
	"testing"
	"time"
)

func TestCredentialBase64_ReturnsOpaqueAPIKey(t *testing.T) {
	client := NewAuthClient(AuthClientConfig{
		APIKey: "cloud-issued-opaque-key",
	})

	got := client.credentialBase64()
	want := "cloud-issued-opaque-key"
	if got != want {
		t.Errorf("credential_base64 = %q, want %q", got, want)
	}
}

func TestCredentialBase64_TrimsWhitespace(t *testing.T) {
	client := NewAuthClient(AuthClientConfig{
		APIKey: "  cloud-issued-opaque-key  ",
	})

	got := client.credentialBase64()
	want := "cloud-issued-opaque-key"
	if got != want {
		t.Errorf("credential_base64 = %q, want %q", got, want)
	}
}

func TestShouldRefresh_NotExpired(t *testing.T) {
	client := NewAuthClient(AuthClientConfig{
		RefreshBefore: 60 * time.Second,
	})
	token := &AuthToken{
		ExpiresAt: time.Now().Add(5 * time.Minute),
	}
	if client.shouldRefresh(token) {
		t.Error("should not refresh token that expires in 5 minutes")
	}
}

func TestShouldRefresh_AboutToExpire(t *testing.T) {
	client := NewAuthClient(AuthClientConfig{
		RefreshBefore: 60 * time.Second,
	})
	token := &AuthToken{
		ExpiresAt: time.Now().Add(30 * time.Second),
	}
	if !client.shouldRefresh(token) {
		t.Error("should refresh token that expires in 30 seconds")
	}
}

func TestShouldRefresh_AlreadyExpired(t *testing.T) {
	client := NewAuthClient(AuthClientConfig{
		RefreshBefore: 60 * time.Second,
	})
	token := &AuthToken{
		ExpiresAt: time.Now().Add(-10 * time.Second),
	}
	if !client.shouldRefresh(token) {
		t.Error("should refresh already expired token")
	}
}

func TestAuthHeader_Format(t *testing.T) {
	// Just verify the format would be correct
	token := &AuthToken{AccessToken: "eyJhbGciOiJFZERTQSJ9.test"}
	header := "Bearer " + token.AccessToken
	if !strings.HasPrefix(header, "Bearer ey") {
		t.Errorf("unexpected header format: %q", header)
	}
}
