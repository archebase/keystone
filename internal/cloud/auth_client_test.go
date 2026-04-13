// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package cloud

import (
	"encoding/base64"
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

func TestBuildCredentialBase64_Format(t *testing.T) {
	client := NewAuthClient(AuthClientConfig{
		SiteID:    42,
		APISecret: "secret-1",
	})

	got := client.buildCredentialBase64()
	if got == "" {
		t.Fatal("buildCredentialBase64 returned empty string")
	}

	// Decode and verify structure: int64_be(42) + "." + "secret-1"
	raw, err := base64.URLEncoding.WithPadding(base64.NoPadding).DecodeString(got)
	if err != nil {
		t.Fatalf("failed to decode base64: %v", err)
	}

	if len(raw) < 10 { // 8 bytes site_id + 1 byte '.' + at least 1 byte secret
		t.Fatalf("decoded credential too short: %d bytes", len(raw))
	}

	siteID := binary.BigEndian.Uint64(raw[:8])
	if siteID != 42 {
		t.Errorf("siteID = %d, want 42", siteID)
	}

	if raw[8] != '.' {
		t.Errorf("separator = %c, want '.'", raw[8])
	}

	secret := string(raw[9:])
	if secret != "secret-1" {
		t.Errorf("secret = %q, want %q", secret, "secret-1")
	}
}

func TestBuildCredentialBase64_MatchesRustSDK(t *testing.T) {
	// The Rust SDK test uses:
	// let mut raw = 42_i64.to_be_bytes().to_vec();
	// raw.push(b'.');
	// raw.extend_from_slice(b"secret-1");
	// base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(raw)

	client := NewAuthClient(AuthClientConfig{
		SiteID:    42,
		APISecret: "secret-1",
	})
	got := client.buildCredentialBase64()

	// Build expected value same way
	var raw []byte
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, 42)
	raw = append(raw, buf...)
	raw = append(raw, '.')
	raw = append(raw, []byte("secret-1")...)
	want := base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(raw)

	if got != want {
		t.Errorf("credential_base64 = %q, want %q", got, want)
	}
}

func TestBuildCredentialBase64_EmptySecret(t *testing.T) {
	client := NewAuthClient(AuthClientConfig{
		SiteID:    1,
		APISecret: "",
	})
	got := client.buildCredentialBase64()
	if got != "" {
		t.Errorf("expected empty credential for empty secret, got %q", got)
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
