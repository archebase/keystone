// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

func TestDeviceRegistrationHandlerRegisterDevice_MissingFactory(t *testing.T) {
	db := newTestDeviceRegistrationDB(t)
	defer db.Close()
	seedDeviceRegistrationFixtures(t, db)

	router := newTestDeviceRegistrationRouter(t, db)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/devices/register", bytes.NewBufferString(`{"robot_type":"搬运机器人"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "factory is required") {
		t.Fatalf("unexpected error response: %s", w.Body.String())
	}
}

func TestDeviceRegistrationHandlerRegisterDevice_UnknownFactory(t *testing.T) {
	db := newTestDeviceRegistrationDB(t)
	defer db.Close()
	seedDeviceRegistrationFixtures(t, db)

	router := newTestDeviceRegistrationRouter(t, db)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/devices/register", bytes.NewBufferString(`{"factory":"未知工厂","robot_type":"搬运机器人"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusNotFound, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "factory not found") {
		t.Fatalf("unexpected error response: %s", w.Body.String())
	}
}

func TestDeviceRegistrationHandlerRegisterDevice_UnknownRobotType(t *testing.T) {
	db := newTestDeviceRegistrationDB(t)
	defer db.Close()
	seedDeviceRegistrationFixtures(t, db)

	router := newTestDeviceRegistrationRouter(t, db)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/devices/register", bytes.NewBufferString(`{"factory":"上海一厂","robot_type":"未知类型"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusNotFound, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "robot_type not found") {
		t.Fatalf("unexpected error response: %s", w.Body.String())
	}
}

func TestDeviceRegistrationHandlerRegisterDevice_Success(t *testing.T) {
	db := newTestDeviceRegistrationDB(t)
	defer db.Close()
	seedDeviceRegistrationFixtures(t, db)

	router := newTestDeviceRegistrationRouter(t, db)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/devices/register", bytes.NewBufferString(`{"factory":"上海一厂","robot_type":"搬运机器人"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusCreated, w.Body.String())
	}

	var resp DeviceRegistrationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v body=%s", err, w.Body.String())
	}
	if resp.DeviceID != "AB-F0003-T0012-000001" || resp.FactoryID != "3" || resp.RobotTypeID != "12" {
		t.Fatalf("unexpected response: %#v", resp)
	}
	if resp.Factory != "上海一厂" || resp.RobotType != "搬运机器人" || resp.RobotID == "" {
		t.Fatalf("unexpected response fields: %#v", resp)
	}
	if !strings.HasPrefix(resp.WSClientAuthToken, "kws_v1_") {
		t.Fatalf("ws_client_auth_token=%q want kws_v1_ prefix", resp.WSClientAuthToken)
	}
	if !isASCII(resp.DeviceID) {
		t.Fatalf("device_id is not ASCII: %q", resp.DeviceID)
	}
	if resp.CallbackAllowlist.AllowedHost != "192.168.1.20:9999" {
		t.Fatalf("allowed_host=%q want 192.168.1.20:9999", resp.CallbackAllowlist.AllowedHost)
	}
	if resp.CallbackAllowlist.AllowedPathPrefix != "/api/v1/callbacks/" {
		t.Fatalf("allowed_path_prefix=%q want /api/v1/callbacks/", resp.CallbackAllowlist.AllowedPathPrefix)
	}

	var robotCount int
	if err := db.Get(&robotCount, "SELECT COUNT(*) FROM robots WHERE device_id = ?", resp.DeviceID); err != nil {
		t.Fatalf("count inserted robot: %v", err)
	}
	if robotCount != 1 {
		t.Fatalf("robot count=%d want=1", robotCount)
	}

	robotID, err := strconv.ParseInt(resp.RobotID, 10, 64)
	if err != nil {
		t.Fatalf("parse robot_id: %v", err)
	}
	tokenHash := sha256.Sum256([]byte(resp.WSClientAuthToken))
	var storedToken struct {
		RobotID      int64  `db:"robot_id"`
		TokenHash    string `db:"token_hash"`
		TokenVersion string `db:"token_version"`
	}
	if err := db.Get(&storedToken, `
		SELECT robot_id, token_hash, token_version
		FROM ws_client_auth_tokens
		WHERE robot_id = ?
	`, robotID); err != nil {
		t.Fatalf("query ws client token: %v", err)
	}
	if storedToken.RobotID != robotID || storedToken.TokenVersion != "kws_v1" {
		t.Fatalf("unexpected stored token metadata: %#v", storedToken)
	}
	if storedToken.TokenHash != hex.EncodeToString(tokenHash[:]) {
		t.Fatalf("stored token_hash=%q does not match response token", storedToken.TokenHash)
	}
	if strings.Contains(storedToken.TokenHash, resp.WSClientAuthToken) {
		t.Fatalf("stored token hash appears to contain plaintext token")
	}

	var nextSequence int64
	if err := db.Get(&nextSequence, "SELECT next_sequence FROM device_id_sequences WHERE factory_id = 3 AND robot_type_id = 12"); err != nil {
		t.Fatalf("query next sequence: %v", err)
	}
	if nextSequence != 2 {
		t.Fatalf("next_sequence=%d want=2", nextSequence)
	}
}

func TestDeviceRegistrationHandlerRegisterDevice_RepeatedRequestAllocatesNewDeviceID(t *testing.T) {
	db := newTestDeviceRegistrationDB(t)
	defer db.Close()
	seedDeviceRegistrationFixtures(t, db)

	router := newTestDeviceRegistrationRouter(t, db)
	first := registerTestDevice(t, router)
	second := registerTestDevice(t, router)

	if first.DeviceID != "AB-F0003-T0012-000001" {
		t.Fatalf("first device_id=%q", first.DeviceID)
	}
	if second.DeviceID != "AB-F0003-T0012-000002" {
		t.Fatalf("second device_id=%q", second.DeviceID)
	}
	if first.RobotID == second.RobotID {
		t.Fatalf("expected distinct robot ids, got %q", first.RobotID)
	}
	if first.WSClientAuthToken == "" || second.WSClientAuthToken == "" {
		t.Fatalf("expected non-empty ws client tokens: first=%q second=%q", first.WSClientAuthToken, second.WSClientAuthToken)
	}
	if first.WSClientAuthToken == second.WSClientAuthToken {
		t.Fatalf("expected distinct ws client tokens, got %q", first.WSClientAuthToken)
	}

	var robotCount int
	if err := db.Get(&robotCount, "SELECT COUNT(*) FROM robots"); err != nil {
		t.Fatalf("count robots: %v", err)
	}
	if robotCount != 2 {
		t.Fatalf("robot count=%d want=2", robotCount)
	}

	var tokenCount int
	if err := db.Get(&tokenCount, "SELECT COUNT(*) FROM ws_client_auth_tokens"); err != nil {
		t.Fatalf("count ws client tokens: %v", err)
	}
	if tokenCount != 2 {
		t.Fatalf("ws client token count=%d want=2", tokenCount)
	}
}

func TestDeviceRegistrationHandlerRegisterDevice_TokenInsertFailureRollsBackRobot(t *testing.T) {
	db := newTestDeviceRegistrationDB(t)
	defer db.Close()
	seedDeviceRegistrationFixtures(t, db)
	if _, err := db.Exec(`DROP TABLE ws_client_auth_tokens`); err != nil {
		t.Fatalf("drop ws client token table: %v", err)
	}

	router := newTestDeviceRegistrationRouter(t, db)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/devices/register", bytes.NewBufferString(`{"factory":"上海一厂","robot_type":"搬运机器人"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusInternalServerError, w.Body.String())
	}
	var robotCount int
	if err := db.Get(&robotCount, "SELECT COUNT(*) FROM robots"); err != nil {
		t.Fatalf("count robots: %v", err)
	}
	if robotCount != 0 {
		t.Fatalf("robot count=%d want=0", robotCount)
	}
}

func TestFormatRegisteredDeviceID_DoesNotTruncateLargeValues(t *testing.T) {
	got := formatRegisteredDeviceID(12345, 98765, 1234567)
	want := "AB-F12345-T98765-1234567"
	if got != want {
		t.Fatalf("formatRegisteredDeviceID()=%q want=%q", got, want)
	}
}

func TestDeviceRegistrationRoutes_DoNotConflictWithRobotDeviceRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	v1 := router.Group("/api/v1")

	NewRobotHandler(nil, nil, nil).RegisterRoutes(v1)
	NewDeviceRegistrationHandler(nil, "http://192.168.1.20:9999").RegisterRoutes(v1)
}

func registerTestDevice(t *testing.T, router *gin.Engine) DeviceRegistrationResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/devices/register", bytes.NewBufferString(`{"factory":"上海一厂","robot_type":"搬运机器人"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusCreated, w.Body.String())
	}

	var resp DeviceRegistrationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v body=%s", err, w.Body.String())
	}
	return resp
}

func newTestDeviceRegistrationRouter(t *testing.T, db *sqlx.DB) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()

	handler := NewDeviceRegistrationHandler(db, "http://192.168.1.20:9999")
	v1 := router.Group("/api/v1")
	handler.RegisterRoutes(v1)

	return router
}

func newTestDeviceRegistrationDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}

	schema := []string{
		`CREATE TABLE factories (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE robot_types (
			id INTEGER PRIMARY KEY,
			model TEXT NOT NULL,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE device_id_sequences (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			factory_id INTEGER NOT NULL,
			robot_type_id INTEGER NOT NULL,
			next_sequence INTEGER NOT NULL DEFAULT 1,
			created_at TIMESTAMP,
			updated_at TIMESTAMP,
			UNIQUE(factory_id, robot_type_id)
		)`,
		`CREATE TABLE robots (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			robot_type_id INTEGER NOT NULL,
			device_id TEXT NOT NULL UNIQUE,
			factory_id INTEGER NOT NULL,
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
			token_version TEXT NOT NULL DEFAULT 'kws_v1',
			created_at TIMESTAMP,
			last_rotated_at TIMESTAMP NULL,
			last_used_at TIMESTAMP NULL,
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
		`INSERT INTO factories (id, name) VALUES (3, '上海一厂')`,
		`INSERT INTO factories (id, name, deleted_at) VALUES (4, '已删除工厂', '2026-01-01T00:00:00Z')`,
		`INSERT INTO robot_types (id, model) VALUES (12, '搬运机器人')`,
		`INSERT INTO robot_types (id, model, deleted_at) VALUES (13, '已删除类型', '2026-01-01T00:00:00Z')`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed fixture failed: %v", err)
		}
	}
}

func isASCII(s string) bool {
	for _, r := range s {
		if r > 127 {
			return false
		}
	}
	return true
}
