// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"archebase.com/keystone-edge/internal/logger"
	"github.com/jmoiron/sqlx"
)

const wsClientTokenVersion = "kws_v1"

func generateWSClientAuthToken() (string, error) {
	randomBytes := make([]byte, 32)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return wsClientTokenVersion + "_" + base64.RawURLEncoding.EncodeToString(randomBytes), nil
}

func hashWSClientAuthToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func insertWSClientAuthToken(tx *sqlx.Tx, robotID int64, token string, now time.Time) error {
	if _, err := tx.Exec(`
		INSERT INTO ws_client_auth_tokens (
			robot_id,
			token_hash,
			token_version,
			created_at
		) VALUES (?, ?, ?, ?)
	`, robotID, hashWSClientAuthToken(token), wsClientTokenVersion, now); err != nil {
		return fmt.Errorf("insert ws client auth token: %w", err)
	}
	return nil
}

func (h *RecorderHandler) authorizeRecorderWebSocket(w http.ResponseWriter, r *http.Request, deviceID string) bool {
	if h.db == nil {
		writeRecorderWebSocketAuthError(w, http.StatusServiceUnavailable, "service unavailable", false)
		return false
	}

	token, ok := parseBearerToken(r.Header.Get("Authorization"))
	if !ok {
		writeRecorderWebSocketAuthError(w, http.StatusUnauthorized, "unauthorized", true)
		return false
	}

	queryTimeout := 5 * time.Second
	queryCtx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()

	var tokenID int64
	if err := h.db.GetContext(queryCtx, &tokenID, `
		SELECT t.id
		FROM ws_client_auth_tokens t
		JOIN robots r ON r.id = t.robot_id
		WHERE r.device_id = ?
			AND t.token_hash = ?
			AND t.revoked_at IS NULL
			AND r.status = 'active'
			AND r.deleted_at IS NULL
		LIMIT 1
	`, deviceID, hashWSClientAuthToken(token)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeRecorderWebSocketAuthError(w, http.StatusUnauthorized, "unauthorized", true)
			return false
		}
		logger.Printf("%s ws client auth query error: %v", recorderLogPrefix(deviceID), err)
		writeRecorderWebSocketAuthError(w, http.StatusServiceUnavailable, "service unavailable", false)
		return false
	}

	if _, err := h.db.ExecContext(queryCtx, `
		UPDATE ws_client_auth_tokens
		SET last_used_at = ?
		WHERE id = ?
	`, time.Now().UTC(), tokenID); err != nil {
		logger.Printf("%s ws client auth last_used_at update failed: %v", recorderLogPrefix(deviceID), err)
	}

	return true
}

func parseBearerToken(header string) (string, bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 || parts[0] != "Bearer" || strings.TrimSpace(parts[1]) == "" {
		return "", false
	}
	return parts[1], true
}

func writeRecorderWebSocketAuthError(w http.ResponseWriter, status int, message string, challenge bool) {
	w.Header().Set("Content-Type", "application/json")
	if challenge {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	w.WriteHeader(status)
	if _, err := fmt.Fprintf(w, `{"error":%q}`, message); err != nil {
		logger.Printf("[RECORDER] write websocket auth error failed: %v", err)
	}
}
