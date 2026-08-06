// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package auth

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"archebase.com/keystone-edge/internal/config"
	"github.com/golang-jwt/jwt/v5"
)

const (
	// StorageDownloadTokenKind identifies JWTs minted for GET /storage/object via dl_token.
	StorageDownloadTokenKind = "storage_object_v1"
	// DefaultStorageDownloadTTL is the default validity window for a download token.
	DefaultStorageDownloadTTL = 15 * time.Minute
)

// StorageDownloadClaims binds a short-lived JWT to one bucket/object pair.
type StorageDownloadClaims struct {
	Kind   string `json:"kind"`
	Bucket string `json:"bucket"`
	Object string `json:"object"`
	jwt.RegisteredClaims
}

// SignStorageDownloadToken returns a JWT suitable for the dl_token query parameter.
func SignStorageDownloadToken(bucket, object string, ttl time.Duration, cfg *config.AuthConfig) (string, error) {
	if cfg == nil || strings.TrimSpace(cfg.JWTSecret) == "" {
		return "", fmt.Errorf("auth not configured")
	}
	if ttl <= 0 {
		ttl = DefaultStorageDownloadTTL
	}
	now := time.Now()
	claims := StorageDownloadClaims{
		Kind:   StorageDownloadTokenKind,
		Bucket: bucket,
		Object: object,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			Issuer:    cfg.Issuer,
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, &claims)
	return tok.SignedString([]byte(cfg.JWTSecret))
}

// ParseStorageDownloadTokenClaims validates a token and returns its bucket/object-bound claims.
func ParseStorageDownloadTokenClaims(tokenString string, cfg *config.AuthConfig, wantBucket, wantObject string) (*StorageDownloadClaims, error) {
	if cfg == nil || strings.TrimSpace(cfg.JWTSecret) == "" {
		return nil, ErrInvalidToken
	}
	tokenString = strings.TrimSpace(tokenString)
	if tokenString == "" {
		return nil, ErrInvalidToken
	}

	var claims StorageDownloadClaims
	token, err := jwt.ParseWithClaims(tokenString, &claims, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, ErrInvalidToken
		}
		return []byte(cfg.JWTSecret), nil
	})
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrExpiredToken
		}
		return nil, ErrInvalidToken
	}
	if !token.Valid {
		return nil, ErrInvalidToken
	}
	if claims.Kind != StorageDownloadTokenKind || claims.ExpiresAt == nil {
		return nil, ErrInvalidToken
	}
	if claims.Bucket != wantBucket || claims.Object != wantObject {
		return nil, ErrInvalidToken
	}
	return &claims, nil
}

// ParseStorageDownloadToken validates token signature, expiry, kind, and bucket/object binding.
func ParseStorageDownloadToken(tokenString string, cfg *config.AuthConfig, wantBucket, wantObject string) error {
	_, err := ParseStorageDownloadTokenClaims(tokenString, cfg, wantBucket, wantObject)
	return err
}
