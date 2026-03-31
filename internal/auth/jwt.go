// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package auth

import (
	"errors"
	"time"

	"archebase.com/keystone-edge/internal/config"
	"github.com/golang-jwt/jwt/v5"
)

var (
	// ErrInvalidToken indicates the token is malformed, signed with an unexpected
	// algorithm, or otherwise fails validation.
	ErrInvalidToken = errors.New("invalid token")
	// ErrExpiredToken indicates the token is expired.
	ErrExpiredToken = errors.New("token has expired")
)

// GenerateToken signs the given claims into a JWT string using the auth config.
func GenerateToken(claims *Claims, cfg *config.AuthConfig) (string, error) {
	claims.Issuer = cfg.Issuer
	claims.ExpiresAt = jwt.NewNumericDate(time.Now().Add(time.Duration(cfg.JWTExpiryHours) * time.Hour))
	claims.IssuedAt = jwt.NewNumericDate(time.Now())
	claims.NotBefore = jwt.NewNumericDate(time.Now())

	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString([]byte(cfg.JWTSecret))
}

// ParseToken parses and validates a JWT string and returns its Claims on success.
func ParseToken(tokenString string, cfg *config.AuthConfig) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(token *jwt.Token) (interface{}, error) {
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

	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, ErrInvalidToken
	}
	return claims, nil
}
