// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package auth

import (
	"strings"
	"testing"
	"time"

	"archebase.com/keystone-edge/internal/config"
)

func TestSignAndParseStorageDownloadToken(t *testing.T) {
	cfg := &config.AuthConfig{
		JWTSecret: "test-secret-at-least-32-bytes-long-ok",
		Issuer:    "keystone-test",
	}

	bucket := "edge-factory-archebase"
	object := "factory-archebase/robot_a/task/file.mcap"

	tok, err := SignStorageDownloadToken(bucket, object, time.Hour, cfg)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if tok == "" || !strings.Contains(tok, ".") {
		t.Fatalf("unexpected token: %q", tok)
	}

	if err := ParseStorageDownloadToken(tok, cfg, bucket, object); err != nil {
		t.Fatalf("parse valid: %v", err)
	}

	if err := ParseStorageDownloadToken(tok, cfg, bucket, object+"x"); err == nil {
		t.Fatal("expected error for object mismatch")
	}

	if err := ParseStorageDownloadToken(tok, cfg, bucket+"x", object); err == nil {
		t.Fatal("expected error for bucket mismatch")
	}

	cfg2 := &config.AuthConfig{JWTSecret: "other-secret-also-32-bytes-minimum", Issuer: "x"}
	if err := ParseStorageDownloadToken(tok, cfg2, bucket, object); err == nil {
		t.Fatal("expected error for wrong secret")
	}
}

func TestStorageDownloadTokenExpired(t *testing.T) {
	cfg := &config.AuthConfig{
		JWTSecret: "test-secret-at-least-32-bytes-long-ok",
		Issuer:    "keystone-test",
	}
	bucket := "b"
	object := "o"

	tok, err := SignStorageDownloadToken(bucket, object, time.Millisecond, cfg)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	err = ParseStorageDownloadToken(tok, cfg, bucket, object)
	if err != ErrExpiredToken {
		t.Fatalf("want ErrExpiredToken, got %v", err)
	}
}
