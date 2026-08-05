// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package deviceauth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

const testJWTSecret = "test-device-secret-at-least-32-bytes"

func TestAuthenticatePersistentAndJWTWithValidCredentials(t *testing.T) {
	db := newAuthenticatorTestDB(t)
	authenticator := New(db, Config{JWTSecret: testJWTSecret, JWTTTL: 15 * time.Minute})
	persistentToken := "kda_v1_active-device-token"
	if _, err := db.Exec(`
		INSERT INTO ws_client_auth_tokens (robot_id, token_hash, revoked_at)
		VALUES (101, ?, NULL)
	`, HashPersistentToken(persistentToken)); err != nil {
		t.Fatalf("seed persistent credential: %v", err)
	}

	persistentPrincipal, err := authenticator.AuthenticatePersistent(context.Background(), persistentToken)
	if err != nil {
		t.Fatalf("AuthenticatePersistent() error = %v", err)
	}
	deviceJWT, _, err := authenticator.IssueJWT(persistentPrincipal, time.Now().UTC())
	if err != nil {
		t.Fatalf("IssueJWT() error = %v", err)
	}
	jwtPrincipal, err := authenticator.AuthenticateJWT(context.Background(), deviceJWT)
	if err != nil {
		t.Fatalf("AuthenticateJWT() error = %v", err)
	}
	if jwtPrincipal != persistentPrincipal {
		t.Fatalf("JWT principal = %#v, want %#v", jwtPrincipal, persistentPrincipal)
	}
}

func TestAuthenticateJWTRejectsInvalidClaims(t *testing.T) {
	db := newAuthenticatorTestDB(t)
	authenticator := New(db, Config{JWTSecret: testJWTSecret, JWTTTL: 15 * time.Minute})
	now := time.Now().UTC()
	tests := []struct {
		name   string
		secret string
		claims deviceClaims
	}{
		{
			name:   "expired",
			secret: testJWTSecret,
			claims: validTestClaims(now.Add(-time.Hour), now.Add(-time.Minute)),
		},
		{
			name:   "bad signature",
			secret: "different-device-secret-at-least-32-bytes",
			claims: validTestClaims(now, now.Add(time.Minute)),
		},
		{
			name:   "wrong issuer",
			secret: testJWTSecret,
			claims: func() deviceClaims {
				claims := validTestClaims(now, now.Add(time.Minute))
				claims.Issuer = "another-issuer"
				return claims
			}(),
		},
		{
			name:   "wrong token type",
			secret: testJWTSecret,
			claims: func() deviceClaims {
				claims := validTestClaims(now, now.Add(time.Minute))
				claims.TokenType = "user_access"
				return claims
			}(),
		},
		{
			name:   "missing expiry",
			secret: testJWTSecret,
			claims: func() deviceClaims {
				claims := validTestClaims(now, now.Add(time.Minute))
				claims.ExpiresAt = nil
				return claims
			}(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := signedTestJWT(t, tt.secret, tt.claims)
			_, err := authenticator.AuthenticateJWT(context.Background(), raw)
			if !errors.Is(err, ErrInvalidCredential) {
				t.Fatalf("AuthenticateJWT() error = %v, want ErrInvalidCredential", err)
			}
		})
	}
}

func TestAuthenticateJWTRejectsRevokedAuthEpoch(t *testing.T) {
	db := newAuthenticatorTestDB(t)
	authenticator := New(db, Config{JWTSecret: testJWTSecret, JWTTTL: 15 * time.Minute})
	principal := Principal{RobotID: 101, DeviceID: "device-101", WorkspaceID: 10, AuthEpoch: 1}
	raw, _, err := authenticator.IssueJWT(principal, time.Now().UTC())
	if err != nil {
		t.Fatalf("IssueJWT() error = %v", err)
	}
	if _, err := db.Exec(`UPDATE robots SET auth_epoch = 2 WHERE id = 101`); err != nil {
		t.Fatalf("increment auth epoch: %v", err)
	}

	_, err = authenticator.AuthenticateJWT(context.Background(), raw)
	if !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("AuthenticateJWT() error = %v, want ErrInvalidCredential", err)
	}
}

func validTestClaims(notBefore, expiresAt time.Time) deviceClaims {
	return deviceClaims{
		DeviceID:    "device-101",
		WorkspaceID: 10,
		AuthEpoch:   1,
		TokenType:   deviceTokenType,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    deviceTokenIssuer,
			Subject:   "device-101",
			ID:        "test-jti",
			IssuedAt:  jwt.NewNumericDate(notBefore),
			NotBefore: jwt.NewNumericDate(notBefore),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
	}
}

func signedTestJWT(t *testing.T, secret string, claims deviceClaims) string {
	t.Helper()
	raw, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign test JWT: %v", err)
	}
	return raw
}

func newAuthenticatorTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close sqlite: %v", err)
		}
	})
	for _, statement := range []string{
		`CREATE TABLE robots (
			id INTEGER PRIMARY KEY,
			device_id TEXT NOT NULL,
			workspace_id INTEGER NOT NULL,
			status TEXT NOT NULL,
			auth_epoch INTEGER NOT NULL,
			deleted_at TIMESTAMP
		)`,
		`CREATE TABLE ws_client_auth_tokens (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			robot_id INTEGER NOT NULL,
			token_hash TEXT NOT NULL UNIQUE,
			revoked_at TIMESTAMP
		)`,
		`INSERT INTO robots (id, device_id, workspace_id, status, auth_epoch)
		 VALUES (101, 'device-101', 10, 'active', 1)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("create device auth schema: %v", err)
		}
	}
	return db
}
