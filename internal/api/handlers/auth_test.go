// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"archebase.com/keystone-edge/internal/auth"
	"archebase.com/keystone-edge/internal/config"
	"archebase.com/keystone-edge/internal/middleware"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

func TestAuthHandlerLoginWithHilbertSuccessIssuesKeystoneJWT(t *testing.T) {
	db := newTestAuthDB(t)
	defer db.Close()

	if _, err := db.Exec(`INSERT INTO data_collectors (id, name, operator_id, status, deleted_at) VALUES (7, 'Old Name', 'dc01', 'active', NULL)`); err != nil {
		t.Fatalf("seed collector: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO robots (id, device_id, workspace_id, status, deleted_at) VALUES (101, 'device-a', 10, 'active', NULL);
		INSERT INTO workstations (id, data_collector_id, workspace_id, robot_id, collector_name, status, is_current, deleted_at)
		VALUES (11, 7, 10, 101, 'Old Name', 'offline', TRUE, NULL)
	`); err != nil {
		t.Fatalf("seed workstation: %v", err)
	}

	hilbert := newTestHilbertServer(t, testHilbertBehavior{
		statusCode: http.StatusOK,
		body:       `{"code":0,"data":{"account":{"id":9,"code":"dc01","displayName":"一号采集员","role":"external_user","externalUserType":"data_supplier","status":"enabled"},"sessionKey":"hilbert-session"}}`,
	})
	defer hilbert.Close()

	router := newTestAuthRouter(db, hilbert.URL)
	w := performAuthLogin(router, `{"operator_id":"dc01","password":"secret","device_id":"device-a"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp LoginResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Role != "data_collector" || resp.Collector == nil || resp.Collector.ID != "7" || resp.Collector.Name != "一号采集员" {
		t.Fatalf("unexpected login response: %#v", resp)
	}

	claims, err := auth.ParseToken(resp.AccessToken, testAuthConfig())
	if err != nil {
		t.Fatalf("parse token: %v", err)
	}
	if claims.Role != "data_collector" || claims.CollectorID != 7 || claims.OperatorID != "dc01" || claims.WorkstationID != 11 {
		t.Fatalf("unexpected claims: %#v", claims)
	}

	var collectorName string
	if err := db.Get(&collectorName, `SELECT name FROM data_collectors WHERE id = 7`); err != nil {
		t.Fatalf("query collector name: %v", err)
	}
	if collectorName != "一号采集员" {
		t.Fatalf("collector name=%q want Hilbert display name", collectorName)
	}

	var workstationName string
	if err := db.Get(&workstationName, `SELECT collector_name FROM workstations WHERE id = 11`); err != nil {
		t.Fatalf("query workstation name: %v", err)
	}
	if workstationName != "一号采集员" {
		t.Fatalf("workstation collector_name=%q want Hilbert display name", workstationName)
	}
}

func TestAuthHandlerLoginActivatesWorkstationForRequestedDevice(t *testing.T) {
	db := newTestAuthDB(t)
	defer db.Close()

	if _, err := db.Exec(`
		INSERT INTO data_collectors (id, name, operator_id, status, deleted_at)
		VALUES (7, 'Collector', 'dc01', 'active', NULL);
		INSERT INTO robots (id, device_id, workspace_id, status, deleted_at)
		VALUES (101, 'device-a', 10, 'active', NULL), (102, 'device-b', 10, 'active', NULL);
		INSERT INTO workstations (
			id, data_collector_id, workspace_id, robot_id, collector_name, status, is_current, deleted_at
		) VALUES
			(11, 7, 10, 101, 'Collector', 'active', TRUE, NULL),
			(12, 7, 10, 102, 'Collector', 'offline', FALSE, NULL)
	`); err != nil {
		t.Fatalf("seed device workstations: %v", err)
	}

	hilbert := newTestHilbertServer(t, testHilbertBehavior{
		statusCode: http.StatusOK,
		body:       `{"code":0,"data":{"account":{"id":9,"code":"dc01","displayName":"Collector","role":"external_user","externalUserType":"data_supplier","status":"enabled"},"sessionKey":"hilbert-session"}}`,
	})
	defer hilbert.Close()

	router := newTestAuthRouter(db, hilbert.URL)
	w := performAuthLogin(router, `{"operator_id":"dc01","password":"secret","device_id":"device-b"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp LoginResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	claims, err := auth.ParseToken(resp.AccessToken, testAuthConfig())
	if err != nil {
		t.Fatalf("parse token: %v", err)
	}
	if claims.WorkstationID != 12 {
		t.Fatalf("workstation_id=%d want 12", claims.WorkstationID)
	}

	var currentIDs []int64
	if err := db.Select(&currentIDs, `
		SELECT id FROM workstations
		WHERE data_collector_id = 7 AND is_current = TRUE AND deleted_at IS NULL
		ORDER BY id
	`); err != nil {
		t.Fatalf("query current workstations: %v", err)
	}
	if len(currentIDs) != 1 || currentIDs[0] != 12 {
		t.Fatalf("current workstation ids=%v want [12]", currentIDs)
	}
}

func TestAuthHandlerLoginWithHilbertSuccessRequiresLocalCollectorBinding(t *testing.T) {
	db := newTestAuthDB(t)
	defer db.Close()

	hilbert := newTestHilbertServer(t, testHilbertBehavior{
		statusCode: http.StatusOK,
		body:       `{"code":0,"data":{"account":{"id":9,"code":"dc01","displayName":"一号采集员","role":"external_user","externalUserType":"data_supplier","status":"enabled"},"sessionKey":"hilbert-session"}}`,
	})
	defer hilbert.Close()

	router := newTestAuthRouter(db, hilbert.URL)
	w := performAuthLogin(router, `{"operator_id":"dc01","password":"secret"}`)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusForbidden, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "collector is not registered in keystone") {
		t.Fatalf("body=%s want local collector binding error", w.Body.String())
	}
}

func TestAuthHandlerLoginAllowsSyncedWorkspaceMemberRegardlessHilbertType(t *testing.T) {
	db := newTestAuthDB(t)
	defer db.Close()

	if _, err := db.Exec(`INSERT INTO data_collectors (id, name, operator_id, status, deleted_at) VALUES (7, 'Workspace Member', 'dc01', 'active', NULL)`); err != nil {
		t.Fatalf("seed collector: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO robots (id, device_id, workspace_id, status, deleted_at) VALUES (101, 'device-a', 10, 'active', NULL);
		INSERT INTO workstations (id, data_collector_id, workspace_id, robot_id, collector_name, status, is_current, deleted_at)
		VALUES (11, 7, 10, 101, 'Workspace Member', 'offline', FALSE, NULL)
	`); err != nil {
		t.Fatalf("seed workstation: %v", err)
	}

	hilbert := newTestHilbertServer(t, testHilbertBehavior{
		statusCode: http.StatusOK,
		body:       `{"code":0,"data":{"account":{"id":9,"code":"dc01","displayName":"Workspace Member","role":"internal_user","externalUserType":"customer","status":"disabled"},"sessionKey":"hilbert-session"}}`,
	})
	defer hilbert.Close()

	router := newTestAuthRouter(db, hilbert.URL)
	w := performAuthLogin(router, `{"operator_id":"dc01","password":"secret","device_id":"device-a"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp LoginResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Role != "data_collector" || resp.Collector == nil || resp.Collector.ID != "7" {
		t.Fatalf("unexpected login response: %#v", resp)
	}
}

func TestAuthHandlerCollectorLoginRequiresDeviceID(t *testing.T) {
	db := newTestAuthDB(t)
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO data_collectors (id, name, operator_id, status, deleted_at) VALUES (7, 'Collector', 'dc01', 'active', NULL)`); err != nil {
		t.Fatalf("seed collector: %v", err)
	}
	hilbert := newTestHilbertServer(t, testHilbertBehavior{
		statusCode: http.StatusOK,
		body:       `{"code":0,"data":{"account":{"id":9,"code":"dc01","displayName":"Collector"},"sessionKey":"hilbert-session"}}`,
	})
	defer hilbert.Close()

	w := performAuthLogin(newTestAuthRouter(db, hilbert.URL), `{"operator_id":"dc01","password":"secret"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "device_id is required") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestAuthHandlerLogoutDeactivatesBoundWorkstation(t *testing.T) {
	db := newTestAuthDB(t)
	defer db.Close()
	if _, err := db.Exec(`
		INSERT INTO data_collectors (id, name, operator_id, status, deleted_at)
		VALUES (7, 'Collector', 'dc01', 'active', NULL);
		INSERT INTO workstations (
			id, data_collector_id, workspace_id, robot_id, collector_name, status, is_current, deleted_at
		) VALUES (11, 7, 10, 101, 'Collector', 'active', TRUE, NULL)
	`); err != nil {
		t.Fatalf("seed workstation: %v", err)
	}
	token, err := auth.GenerateToken(auth.NewCollectorWorkstationClaims(7, "dc01", 11, 101, 10), testAuthConfig())
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	router := newTestAuthRouter(db, "")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var row struct {
		Status    string `db:"status"`
		IsCurrent bool   `db:"is_current"`
	}
	if err := db.Get(&row, `SELECT status, is_current FROM workstations WHERE id = 11`); err != nil {
		t.Fatalf("query workstation: %v", err)
	}
	if row.Status != "offline" || row.IsCurrent {
		t.Fatalf("workstation after logout: %#v", row)
	}
}

func TestAuthHandlerLoginWithHilbertBusinessFailureReturnsUnauthorized(t *testing.T) {
	db := newTestAuthDB(t)
	defer db.Close()

	hilbert := newTestHilbertServer(t, testHilbertBehavior{
		statusCode: http.StatusOK,
		body:       `{"code":401,"data":null}`,
	})
	defer hilbert.Close()

	router := newTestAuthRouter(db, hilbert.URL)
	w := performAuthLogin(router, `{"operator_id":"dc01","password":"bad"}`)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusUnauthorized, w.Body.String())
	}
}

func TestAuthHandlerLoginWithHilbertUnavailableReturnsServiceUnavailable(t *testing.T) {
	db := newTestAuthDB(t)
	defer db.Close()

	hilbert := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "hilbert failed", http.StatusInternalServerError)
	}))
	defer hilbert.Close()

	router := newTestAuthRouter(db, hilbert.URL)
	w := performAuthLogin(router, `{"operator_id":"dc01","password":"secret"}`)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusServiceUnavailable, w.Body.String())
	}
}

func TestAuthHandlerLoginWithoutHilbertConfigReturnsServiceUnavailable(t *testing.T) {
	db := newTestAuthDB(t)
	defer db.Close()

	router := newTestAuthRouter(db, "")
	w := performAuthLogin(router, `{"operator_id":"dc01","password":"secret"}`)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusServiceUnavailable, w.Body.String())
	}
}

func TestAuthHandlerBreakOnlyUpdatesAccessibleWorkspaceWorkstations(t *testing.T) {
	db := newTestAuthDB(t)
	defer db.Close()
	if _, err := db.Exec(`
		INSERT INTO workspaces (id, admins, members) VALUES (20, '[]', '[]');
		INSERT INTO workstations (id, data_collector_id, workspace_id, robot_id, status, is_current)
		VALUES (11, 7, 10, 101, 'inactive', TRUE), (12, 7, 20, 102, 'inactive', TRUE)
	`); err != nil {
		t.Fatalf("seed workstations: %v", err)
	}
	handler := NewAuthHandler(db, testAuthConfig(), nil)
	router := gin.New()
	router.POST("/auth/me/station/break", func(c *gin.Context) {
		c.Set(middleware.ClaimsKey, auth.NewCollectorClaims(7, "dc01"))
		handler.MeStationBreak(c)
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/auth/me/station/break", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var accessibleStatus string
	if err := db.Get(&accessibleStatus, `SELECT status FROM workstations WHERE id = 11`); err != nil {
		t.Fatalf("query accessible workstation: %v", err)
	}
	var revokedStatus string
	if err := db.Get(&revokedStatus, `SELECT status FROM workstations WHERE id = 12`); err != nil {
		t.Fatalf("query revoked workstation: %v", err)
	}
	if accessibleStatus != "break" || revokedStatus != "inactive" {
		t.Fatalf("statuses accessible=%s revoked=%s", accessibleStatus, revokedStatus)
	}
}

type testHilbertBehavior struct {
	statusCode int
	body       string
}

func newTestHilbertServer(t *testing.T, behavior testHilbertBehavior) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/console/nonce/generate":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"data":{"id":67,"randomKey":"nZZP19BmFxg2pbakq1eWKxLB1gd5qJe4IbTEjOGa+A91XPvGJsmsEkV5NK0="}}`))
		case "/v1/console/account/login":
			var body struct {
				Code         string `json:"code"`
				NonceID      int64  `json:"nonceId"`
				CipherDigest string `json:"cipherDigest"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode Hilbert login body: %v", err)
			}
			if body.Code != "dc01" || body.NonceID != 67 || body.CipherDigest == "" {
				t.Fatalf("unexpected Hilbert login body: %#v", body)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(behavior.statusCode)
			_, _ = w.Write([]byte(behavior.body))
		default:
			http.NotFound(w, r)
		}
	}))
}

func newTestAuthRouter(db *sqlx.DB, hilbertBaseURL string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	NewAuthHandler(db, testAuthConfig(), testHilbertConfig(hilbertBaseURL)).RegisterRoutes(router.Group("/api/v1"))
	return router
}

func performAuthLogin(router *gin.Engine, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func testAuthConfig() *config.AuthConfig {
	return &config.AuthConfig{
		JWTSecret:      "test-jwt-secret-at-least-32-bytes-long",
		Issuer:         "keystone-test",
		JWTExpiryHours: 24,
	}
}

func testHilbertConfig(hilbertBaseURL string) *config.HilbertConfig {
	return &config.HilbertConfig{
		BaseURL:        hilbertBaseURL,
		TimeoutSeconds: 2,
	}
}

func newTestAuthDB(t *testing.T) *sqlx.DB {
	t.Helper()

	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	stmts := []string{
		`CREATE TABLE workspaces (
			id INTEGER PRIMARY KEY,
			admins TEXT NOT NULL,
			members TEXT NOT NULL,
			deleted_at TEXT
		)`,
		`INSERT INTO workspaces (id, admins, members) VALUES (10, '[]', '["dc01"]')`,
		`CREATE TABLE data_collectors (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			operator_id TEXT NOT NULL,
			status TEXT NOT NULL,
			last_login_at TEXT,
			deleted_at TEXT
		)`,
		`CREATE TABLE robots (
			id INTEGER PRIMARY KEY,
			device_id TEXT NOT NULL,
			workspace_id INTEGER NOT NULL,
			status TEXT NOT NULL,
			deleted_at TEXT
		)`,
		`CREATE TABLE workstations (
			id INTEGER PRIMARY KEY,
			data_collector_id INTEGER,
			workspace_id INTEGER NOT NULL DEFAULT 0,
			robot_id INTEGER NOT NULL DEFAULT 0,
			collector_name TEXT,
			status TEXT,
			is_current BOOLEAN,
			superseded_at TEXT,
			superseded_by INTEGER,
			updated_at TEXT,
			deleted_at TEXT
		)`,
		`CREATE TABLE tasks (
			id INTEGER PRIMARY KEY,
			workstation_id INTEGER,
			status TEXT,
			deleted_at TEXT
		)`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			_ = db.Close()
			t.Fatalf("create auth test schema: %v", err)
		}
	}
	return db
}
