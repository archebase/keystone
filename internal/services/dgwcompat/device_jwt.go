// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package dgwcompat

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const deviceTokenType = "device_access"

type deviceClaims struct {
	DeviceID    string `json:"device_id"`
	WorkspaceID int64  `json:"workspace_id"`
	AuthEpoch   int64  `json:"auth_epoch"`
	TokenType   string `json:"token_type"`
	jwt.RegisteredClaims
}

type devicePrincipal struct {
	RobotID     int64  `db:"robot_id"`
	DeviceID    string `db:"device_id"`
	WorkspaceID int64  `db:"workspace_id"`
	AuthEpoch   int64  `db:"auth_epoch"`
}

type devicePrincipalContextKey struct{}

func issueDeviceJWT(cfg Config, principal devicePrincipal, now time.Time) (string, time.Time, error) {
	expiresAt := now.Add(cfg.DeviceJWTTTL)
	claims := deviceClaims{
		DeviceID:    principal.DeviceID,
		WorkspaceID: principal.WorkspaceID,
		AuthEpoch:   principal.AuthEpoch,
		TokenType:   deviceTokenType,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "keystone-device",
			Subject:   principal.DeviceID,
			ID:        uuid.NewString(),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(cfg.DeviceJWTSecret))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign device JWT: %w", err)
	}
	return signed, expiresAt, nil
}

func parseDeviceJWT(cfg Config, raw string) (*deviceClaims, error) {
	token, err := jwt.ParseWithClaims(raw, &deviceClaims{}, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodHS256 {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return []byte(cfg.DeviceJWTSecret), nil
	}, jwt.WithIssuer("keystone-device"))
	if err != nil {
		return nil, err
	}
	claims, ok := token.Claims.(*deviceClaims)
	if !ok || !token.Valid || claims.TokenType != deviceTokenType || claims.DeviceID == "" || claims.WorkspaceID <= 0 || claims.AuthEpoch <= 0 {
		return nil, fmt.Errorf("invalid device claims")
	}
	return claims, nil
}

func deviceUnaryAuthInterceptor(db *sqlx.DB, cfg Config) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		principal, err := authenticateDeviceContext(ctx, db, cfg)
		if err != nil {
			return nil, err
		}
		return handler(context.WithValue(ctx, devicePrincipalContextKey{}, principal), req)
	}
}

func authenticateDeviceContext(ctx context.Context, db *sqlx.DB, cfg Config) (devicePrincipal, error) {
	values := metadata.ValueFromIncomingContext(ctx, "authorization")
	if len(values) != 1 {
		return devicePrincipal{}, status.Error(codes.Unauthenticated, "device bearer token required")
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return devicePrincipal{}, status.Error(codes.Unauthenticated, "invalid bearer token")
	}
	claims, err := parseDeviceJWT(cfg, parts[1])
	if err != nil {
		return devicePrincipal{}, status.Error(codes.Unauthenticated, "invalid device token")
	}
	var principal devicePrincipal
	if err := db.GetContext(ctx, &principal, `
		SELECT id AS robot_id, device_id, workspace_id, auth_epoch
		FROM robots
		WHERE device_id = ? AND status = 'active' AND deleted_at IS NULL
		LIMIT 1
	`, claims.DeviceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return devicePrincipal{}, status.Error(codes.Unauthenticated, "device is not active")
		}
		return devicePrincipal{}, status.Error(codes.Unavailable, "device authentication unavailable")
	}
	if principal.WorkspaceID != claims.WorkspaceID || principal.AuthEpoch != claims.AuthEpoch {
		return devicePrincipal{}, status.Error(codes.Unauthenticated, "device token has been revoked")
	}
	return principal, nil
}

func principalFromContext(ctx context.Context) (devicePrincipal, error) {
	principal, ok := ctx.Value(devicePrincipalContextKey{}).(devicePrincipal)
	if !ok {
		return devicePrincipal{}, status.Error(codes.Unauthenticated, "device principal missing")
	}
	return principal, nil
}
