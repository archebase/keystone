// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

func TestDeviceRegistrationHandlerRotateDeviceAuthToken_SuccessRevokesOldToken(t *testing.T) {
	db := newTestDeviceRegistrationDB(t)
	defer db.Close()
	seedDeviceRegistrationFixtures(t, db)

	router := newTestDeviceRegistrationRouter(t, db)
	const robotID int64 = 9
	oldToken := seedActiveDeviceAuthToken(t, db, robotID)
	oldHashBytes := sha256.Sum256([]byte(oldToken))
	oldHash := hex.EncodeToString(oldHashBytes[:])

	req := httptest.NewRequest(http.MethodPost, "/api/v1/robots/9/device-auth-token/rotate", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		DeviceID        string `json:"device_id"`
		RobotID         string `json:"robot_id"`
		DeviceAuthToken string `json:"device_auth_token"`
		RotatedAt       string `json:"rotated_at"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v body=%s", err, w.Body.String())
	}
	if resp.DeviceID != "456" || resp.RobotID != "9" {
		t.Fatalf("unexpected rotate response identity: %#v", resp)
	}
	if !strings.HasPrefix(resp.DeviceAuthToken, "kda_v1_") {
		t.Fatalf("device_auth_token=%q want kda_v1_ prefix", resp.DeviceAuthToken)
	}
	if resp.DeviceAuthToken == oldToken {
		t.Fatalf("rotated token should differ from old token")
	}
	if strings.TrimSpace(resp.RotatedAt) == "" {
		t.Fatalf("rotated_at is empty")
	}

	var revokedOldCount int
	if err := db.Get(&revokedOldCount, `
		SELECT COUNT(*)
		FROM ws_client_auth_tokens
		WHERE robot_id = ? AND token_hash = ? AND revoked_at IS NOT NULL AND last_rotated_at IS NOT NULL
	`, robotID, oldHash); err != nil {
		t.Fatalf("count revoked old token: %v", err)
	}
	if revokedOldCount != 1 {
		t.Fatalf("revoked old token count=%d want=1", revokedOldCount)
	}

	newHashBytes := sha256.Sum256([]byte(resp.DeviceAuthToken))
	newHash := hex.EncodeToString(newHashBytes[:])
	var activeTokenHash string
	if err := db.Get(&activeTokenHash, `
		SELECT token_hash
		FROM ws_client_auth_tokens
		WHERE robot_id = ? AND revoked_at IS NULL
	`, robotID); err != nil {
		t.Fatalf("query active token hash: %v", err)
	}
	if activeTokenHash != newHash {
		t.Fatalf("active token hash=%q does not match rotated token", activeTokenHash)
	}
	if strings.Contains(activeTokenHash, resp.DeviceAuthToken) {
		t.Fatalf("stored token hash appears to contain plaintext token")
	}
}

func TestDeviceRegistrationHandlerRotateDeviceAuthToken_SucceedsWithoutActiveToken(t *testing.T) {
	db := newTestDeviceRegistrationDB(t)
	defer db.Close()
	seedDeviceRegistrationFixtures(t, db)

	router := newTestDeviceRegistrationRouter(t, db)
	seedActiveDeviceAuthToken(t, db, 9)
	if _, err := db.Exec(`
		UPDATE ws_client_auth_tokens
		SET revoked_at = ?, last_rotated_at = ?
		WHERE robot_id = ?
	`, "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z", 9); err != nil {
		t.Fatalf("revoke seeded token: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/robots/9/device-auth-token/rotate", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp RotateDeviceAuthTokenResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v body=%s", err, w.Body.String())
	}
	if !strings.HasPrefix(resp.DeviceAuthToken, "kda_v1_") {
		t.Fatalf("device_auth_token=%q want kda_v1_ prefix", resp.DeviceAuthToken)
	}

	var activeTokenCount int
	if err := db.Get(&activeTokenCount, `
		SELECT COUNT(*)
		FROM ws_client_auth_tokens
		WHERE robot_id = ? AND revoked_at IS NULL
	`, 9); err != nil {
		t.Fatalf("count active tokens: %v", err)
	}
	if activeTokenCount != 1 {
		t.Fatalf("active token count=%d want=1", activeTokenCount)
	}
}

func TestDeviceRegistrationHandlerRotateDeviceAuthToken_EnablesRecovery(t *testing.T) {
	db := newTestDeviceRegistrationDB(t)
	defer db.Close()
	seedDeviceRegistrationFixtures(t, db)

	router := newTestDeviceRegistrationRouter(t, db)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/robots/9/device-auth-token/rotate", strings.NewReader(`{"enable_recovery":true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	var resp RotateDeviceAuthTokenResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if !resp.RecoveryEnabled {
		t.Fatal("recovery_enabled=false want=true")
	}

	var stored struct {
		RequestedAt string `db:"recovery_requested_at"`
		Stage       string `db:"recovery_stage"`
	}
	if err := db.Get(&stored, `
		SELECT recovery_requested_at, recovery_stage
		FROM ws_client_auth_tokens
		WHERE robot_id = 9 AND revoked_at IS NULL
	`); err != nil {
		t.Fatalf("query recovery state: %v", err)
	}
	if stored.RequestedAt == "" || stored.Stage != "authorized" {
		t.Fatalf("unexpected recovery state: %#v", stored)
	}
}

func TestDeviceRegistrationHandlerRotateDeviceAuthToken_RobotNotFound(t *testing.T) {
	db := newTestDeviceRegistrationDB(t)
	defer db.Close()
	seedDeviceRegistrationFixtures(t, db)
	if _, err := db.Exec(`
		INSERT INTO robots (id, device_id, status, deleted_at)
		VALUES (99, 'deleted-device', 'active', '2026-01-01T00:00:00Z')
	`); err != nil {
		t.Fatalf("seed deleted robot: %v", err)
	}

	router := newTestDeviceRegistrationRouter(t, db)
	for _, path := range []string{
		"/api/v1/robots/42/device-auth-token/rotate",
		"/api/v1/robots/99/device-auth-token/rotate",
	} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Fatalf("%s status=%d want=%d body=%s", path, w.Code, http.StatusNotFound, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "robot not found") {
			t.Fatalf("%s unexpected error response: %s", path, w.Body.String())
		}
	}
}

func TestDeviceRegistrationHandlerRotateDeviceAuthToken_RobotNotActive(t *testing.T) {
	db := newTestDeviceRegistrationDB(t)
	defer db.Close()
	seedDeviceRegistrationFixtures(t, db)
	if _, err := db.Exec(`
		INSERT INTO robots (id, device_id, status)
		VALUES (88, 'maintenance-device', 'maintenance')
	`); err != nil {
		t.Fatalf("seed maintenance robot: %v", err)
	}

	router := newTestDeviceRegistrationRouter(t, db)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/robots/88/device-auth-token/rotate", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "robot is not active") {
		t.Fatalf("unexpected error response: %s", w.Body.String())
	}

	var tokenCount int
	if err := db.Get(&tokenCount, "SELECT COUNT(*) FROM ws_client_auth_tokens WHERE robot_id = 88"); err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	if tokenCount != 0 {
		t.Fatalf("token count=%d want=0", tokenCount)
	}
}

func TestDeviceRegistrationHandlerRotateDeviceAuthToken_InvalidRobotID(t *testing.T) {
	db := newTestDeviceRegistrationDB(t)
	defer db.Close()

	router := newTestDeviceRegistrationRouter(t, db)
	for _, path := range []string{
		"/api/v1/robots/not-a-number/device-auth-token/rotate",
		"/api/v1/robots/0/device-auth-token/rotate",
	} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s status=%d want=%d body=%s", path, w.Code, http.StatusBadRequest, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "invalid robot id") {
			t.Fatalf("%s unexpected error response: %s", path, w.Body.String())
		}
	}
}

func TestDeviceRegistrationRoutes_DoNotConflictWithRobotDeviceRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	v1 := router.Group("/api/v1")

	NewRobotHandler(nil, nil, nil).RegisterRoutes(v1)
	handler := NewDeviceRegistrationHandler(nil, "http://192.168.1.20:9999")
	handler.RegisterRoutes(v1)
	handler.RegisterAdminRoutes(v1)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/devices/register", strings.NewReader(`{"device_id":"456"}`))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("public device registration status=%d want=%d", w.Code, http.StatusNotFound)
	}
}

func seedActiveDeviceAuthToken(t *testing.T, db *sqlx.DB, robotID int64) string {
	t.Helper()
	token, err := generateWSClientAuthToken()
	if err != nil {
		t.Fatalf("generate device auth token: %v", err)
	}
	tx, err := db.Beginx()
	if err != nil {
		t.Fatalf("begin token transaction: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck // Test cleanup after commit.
	if err := insertWSClientAuthToken(tx, robotID, token, time.Now().UTC(), false); err != nil {
		t.Fatalf("insert device auth token: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit device auth token: %v", err)
	}
	return token
}

func newTestDeviceRegistrationRouter(t *testing.T, db *sqlx.DB) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()

	handler := NewDeviceRegistrationHandler(db, "http://192.168.1.20:9999")
	v1 := router.Group("/api/v1")
	handler.RegisterAdminRoutes(v1)

	return router
}

func newTestDeviceRegistrationDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}

	schema := []string{
		`CREATE TABLE robots (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			device_id TEXT NOT NULL UNIQUE,
			workspace_id INTEGER NOT NULL DEFAULT 0,
			asset_id TEXT,
			status TEXT,
			metadata TEXT,
			created_at TIMESTAMP,
			updated_at TIMESTAMP,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE ws_client_auth_tokens (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			robot_id INTEGER NOT NULL,
			token_hash TEXT NOT NULL UNIQUE,
			token_version TEXT NOT NULL DEFAULT 'kda_v1',
			created_at TIMESTAMP,
			last_rotated_at TIMESTAMP NULL,
			last_used_at TIMESTAMP NULL,
			sdk_initialized_at TIMESTAMP NULL,
			recovery_requested_at TIMESTAMP NULL,
			recovery_stage TEXT NOT NULL DEFAULT 'none',
			recovery_completed_at TIMESTAMP NULL,
			revoked_at TIMESTAMP NULL
		)`,
	}

	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("create schema failed: %v", err)
		}
	}

	return db
}

func seedDeviceRegistrationFixtures(t *testing.T, db *sqlx.DB) {
	t.Helper()
	stmts := []string{
		`INSERT INTO robots (id, device_id, workspace_id, status, metadata) VALUES (9, '456', 123, 'active', '{"source":"hilbert","hilbert_dc_device_id":456}')`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed fixture failed: %v", err)
		}
	}
}
