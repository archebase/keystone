// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package deviceauth authenticates Keystone device credentials and owns the
// common device principal used by HTTP and gRPC transports.
package deviceauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

const (
	// PersistentTokenVersion identifies administrator-issued device credentials.
	PersistentTokenVersion = "kda_v1"

	deviceTokenIssuer = "keystone-device"
	deviceTokenType   = "device_access"
)

// ErrInvalidCredential indicates that a device credential is malformed,
// expired, revoked, unknown, or belongs to an inactive device.
var ErrInvalidCredential = errors.New("invalid device credential")

// Principal is the active robot identity resolved from either supported
// device credential type.
type Principal struct {
	RobotID     int64  `db:"robot_id"`
	DeviceID    string `db:"device_id"`
	WorkspaceID int64  `db:"workspace_id"`
	AuthEpoch   int64  `db:"auth_epoch"`
}

// Config controls device JWT signing and verification.
type Config struct {
	JWTSecret string
	JWTTTL    time.Duration
}

// Authenticator resolves persistent device credentials and temporary Device JWTs.
type Authenticator struct {
	db  *sqlx.DB
	cfg Config
}

type deviceClaims struct {
	DeviceID    string `json:"device_id"`
	WorkspaceID int64  `json:"workspace_id"`
	AuthEpoch   int64  `json:"auth_epoch"`
	TokenType   string `json:"token_type"`
	jwt.RegisteredClaims
}

type principalContextKey struct{}

// New constructs a device credential authenticator.
func New(db *sqlx.DB, cfg Config) *Authenticator {
	return &Authenticator{db: db, cfg: cfg}
}

// GeneratePersistentToken returns a plaintext token exposed only at issuance time.
func GeneratePersistentToken() (string, error) {
	randomBytes := make([]byte, 32)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return PersistentTokenVersion + "_" + base64.RawURLEncoding.EncodeToString(randomBytes), nil
}

// HashPersistentToken returns the database representation of a plaintext token.
func HashPersistentToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// AuthenticatePersistent resolves one active device from a persistent token.
func (a *Authenticator) AuthenticatePersistent(ctx context.Context, token string) (Principal, error) {
	if a == nil || a.db == nil {
		return Principal{}, fmt.Errorf("authenticate persistent device token: database is not configured")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return Principal{}, ErrInvalidCredential
	}

	var principal Principal
	if err := a.db.GetContext(ctx, &principal, `
		SELECT r.id AS robot_id, r.device_id, r.workspace_id, r.auth_epoch
		FROM ws_client_auth_tokens t
		INNER JOIN robots r ON r.id = t.robot_id
		WHERE t.token_hash = ?
			AND t.revoked_at IS NULL
			AND r.status = 'active'
			AND r.deleted_at IS NULL
		LIMIT 1
	`, HashPersistentToken(token)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Principal{}, ErrInvalidCredential
		}
		return Principal{}, fmt.Errorf("authenticate persistent device token: %w", err)
	}
	return principal, nil
}

// IssueJWT signs a temporary Device JWT bound to the principal's auth epoch.
func (a *Authenticator) IssueJWT(principal Principal, now time.Time) (string, time.Time, error) {
	if a == nil || strings.TrimSpace(a.cfg.JWTSecret) == "" || a.cfg.JWTTTL <= 0 {
		return "", time.Time{}, fmt.Errorf("issue device JWT: invalid configuration")
	}
	expiresAt := now.Add(a.cfg.JWTTTL)
	claims := deviceClaims{
		DeviceID:    principal.DeviceID,
		WorkspaceID: principal.WorkspaceID,
		AuthEpoch:   principal.AuthEpoch,
		TokenType:   deviceTokenType,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    deviceTokenIssuer,
			Subject:   principal.DeviceID,
			ID:        uuid.NewString(),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(a.cfg.JWTSecret))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign device JWT: %w", err)
	}
	return signed, expiresAt, nil
}

// AuthenticateJWT verifies a Device JWT and resolves its current active principal.
func (a *Authenticator) AuthenticateJWT(ctx context.Context, raw string) (Principal, error) {
	if a == nil || a.db == nil {
		return Principal{}, fmt.Errorf("authenticate device JWT: database is not configured")
	}
	claims, err := a.parseJWT(strings.TrimSpace(raw))
	if err != nil {
		return Principal{}, err
	}
	var principal Principal
	if err := a.db.GetContext(ctx, &principal, `
		SELECT id AS robot_id, device_id, workspace_id, auth_epoch
		FROM robots
		WHERE device_id = ? AND status = 'active' AND deleted_at IS NULL
		LIMIT 1
	`, claims.DeviceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Principal{}, ErrInvalidCredential
		}
		return Principal{}, fmt.Errorf("authenticate device JWT: %w", err)
	}
	if principal.WorkspaceID != claims.WorkspaceID || principal.AuthEpoch != claims.AuthEpoch {
		return Principal{}, ErrInvalidCredential
	}
	return principal, nil
}

func (a *Authenticator) parseJWT(raw string) (*deviceClaims, error) {
	if strings.TrimSpace(a.cfg.JWTSecret) == "" {
		return nil, fmt.Errorf("authenticate device JWT: JWT secret is not configured")
	}
	if raw == "" {
		return nil, ErrInvalidCredential
	}
	token, err := jwt.ParseWithClaims(raw, &deviceClaims{}, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodHS256 {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return []byte(a.cfg.JWTSecret), nil
	}, jwt.WithIssuer(deviceTokenIssuer), jwt.WithExpirationRequired())
	if err != nil {
		return nil, ErrInvalidCredential
	}
	claims, ok := token.Claims.(*deviceClaims)
	if !ok || !token.Valid || claims.TokenType != deviceTokenType ||
		claims.DeviceID == "" || claims.Subject != claims.DeviceID ||
		claims.WorkspaceID <= 0 || claims.AuthEpoch <= 0 {
		return nil, ErrInvalidCredential
	}
	return claims, nil
}

// WithPrincipal attaches an authenticated device principal to a request context.
func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, principal)
}

// PrincipalFromContext returns the device principal attached by a transport adapter.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(Principal)
	return principal, ok
}
