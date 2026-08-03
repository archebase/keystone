// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

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

	"github.com/jmoiron/sqlx"
)

// DeviceAuthTokenVersion identifies administrator-issued device credentials.
const DeviceAuthTokenVersion = "kda_v1"

// ErrInvalidDeviceAuthToken indicates that a device credential is unknown,
// revoked, or belongs to an inactive device.
var ErrInvalidDeviceAuthToken = errors.New("invalid device auth token")

// DevicePrincipal is the active device identity resolved from a persistent
// device authentication token.
type DevicePrincipal struct {
	RobotID     int64  `db:"robot_id"`
	DeviceID    string `db:"device_id"`
	WorkspaceID int64  `db:"workspace_id"`
}

// GenerateDeviceAuthToken returns a plaintext token that is only exposed at issuance time.
func GenerateDeviceAuthToken() (string, error) {
	randomBytes := make([]byte, 32)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return DeviceAuthTokenVersion + "_" + base64.RawURLEncoding.EncodeToString(randomBytes), nil
}

// HashDeviceAuthToken returns the database representation of a plaintext device token.
func HashDeviceAuthToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// AuthenticateDeviceAuthToken resolves one active device from its persistent
// administrator-issued credential.
func AuthenticateDeviceAuthToken(
	ctx context.Context,
	db *sqlx.DB,
	token string,
) (DevicePrincipal, error) {
	if db == nil {
		return DevicePrincipal{}, fmt.Errorf("authenticate device token: database is not configured")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return DevicePrincipal{}, ErrInvalidDeviceAuthToken
	}

	var principal DevicePrincipal
	if err := db.GetContext(ctx, &principal, `
		SELECT r.id AS robot_id, r.device_id, r.workspace_id
		FROM ws_client_auth_tokens t
		INNER JOIN robots r ON r.id = t.robot_id
		WHERE t.token_hash = ?
			AND t.revoked_at IS NULL
			AND r.status = 'active'
			AND r.deleted_at IS NULL
		LIMIT 1
	`, HashDeviceAuthToken(token)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DevicePrincipal{}, ErrInvalidDeviceAuthToken
		}
		return DevicePrincipal{}, fmt.Errorf("authenticate device token: %w", err)
	}
	return principal, nil
}
