// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"bytes"
	"database/sql"
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

func TestAuthHandlerLoginWithHilbertSuccessIssuesIdentityJWT(t *testing.T) {
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
	w := performAuthLogin(router, `{"operator_id":"dc01","password":"secret"}`)

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
	if claims.Role != "data_collector" || claims.CollectorID != 7 || claims.OperatorID != "dc01" || claims.WorkstationID != 0 {
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

func TestAuthHandlerActivateWorkstationForWebSelection(t *testing.T) {
	db := newTestAuthDB(t)
	defer db.Close()

	if _, err := db.Exec(`
		INSERT INTO data_collectors (id, name, operator_id, status, deleted_at)
		VALUES (7, 'Collector', 'dc01', 'active', NULL);
		INSERT INTO robots (id, device_id, workspace_id, status, deleted_at)
		VALUES (101, '101', 10, 'active', NULL), (102, '102', 10, 'active', NULL);
		INSERT INTO workstations (
			id, data_collector_id, workspace_id, robot_id, collector_name, status, is_current, deleted_at
		) VALUES
			(11, 7, 10, 101, 'Collector', 'active', TRUE, NULL),
			(12, 7, 10, 102, 'Collector', 'offline', FALSE, NULL);
		INSERT INTO dc_plan (id, workspace_id, operator, dc_device_id, deleted_at)
		VALUES (1001, 10, 'dc01', 102, NULL)
	`); err != nil {
		t.Fatalf("seed device workstations: %v", err)
	}

	hilbert := newTestHilbertServer(t, testHilbertBehavior{
		statusCode: http.StatusOK,
		body:       `{"code":0,"data":{"account":{"id":9,"code":"dc01","displayName":"Collector","role":"external_user","externalUserType":"data_supplier","status":"enabled"},"sessionKey":"hilbert-session"}}`,
	})
	defer hilbert.Close()

	router := newTestAuthRouter(db, hilbert.URL)
	login := performAuthLogin(router, `{"operator_id":"dc01","password":"secret"}`)
	if login.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", login.Code, login.Body.String())
	}
	var loginResp LoginResponse
	if err := json.Unmarshal(login.Body.Bytes(), &loginResp); err != nil {
		t.Fatalf("unmarshal login response: %v", err)
	}
	w := performAuthActivation(router, loginResp.AccessToken, `{"workstation_id":12}`, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp WorkstationActivationResponse
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

func TestAuthHandlerMeListsPlanEligibleNonCurrentWorkstation(t *testing.T) {
	db := newTestAuthDB(t)
	defer db.Close()
	if _, err := db.Exec(`
		INSERT INTO data_collectors (id, name, operator_id, status) VALUES (7, 'Collector', 'dc01', 'active');
		INSERT INTO robots (id, device_id, device_name, workspace_id, status) VALUES (101, '101', 'Ego iPhone 01', 10, 'active');
		INSERT INTO workstations (id, data_collector_id, workspace_id, robot_id, status, is_current)
		VALUES (11, 7, 10, 101, 'offline', FALSE);
		INSERT INTO dc_plan (id, workspace_id, operator, dc_device_id) VALUES (1001, 10, 'dc01', 101)
	`); err != nil {
		t.Fatalf("seed eligible workstation: %v", err)
	}
	token, err := auth.GenerateToken(auth.NewCollectorClaims(7, "dc01"), testAuthConfig())
	if err != nil {
		t.Fatalf("generate identity token: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	newTestAuthRouter(db, "").ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp AuthMeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal me response: %v", err)
	}
	if len(resp.Workstations) != 0 || len(resp.AvailableWorkstations) != 1 {
		t.Fatalf("current=%#v available=%#v", resp.Workstations, resp.AvailableWorkstations)
	}
	available := resp.AvailableWorkstations[0]
	if available.ID != "11" || available.WorkspaceName != "Test Workspace" || available.DeviceID != "101" || available.DeviceName != "Ego iPhone 01" || available.IsCurrent {
		t.Fatalf("unexpected available workstation: %#v", available)
	}
}

func TestAuthHandlerActivateWorkstationForEgoPortalDeviceCredential(t *testing.T) {
	db := newTestAuthDB(t)
	defer db.Close()
	deviceToken := "kda_v1_test-device-token"
	if _, err := db.Exec(`
		INSERT INTO data_collectors (id, name, operator_id, status) VALUES (7, 'Collector', 'dc01', 'active');
		INSERT INTO robots (id, device_id, device_name, workspace_id, status) VALUES (101, '101', 'Ego iPhone 01', 10, 'active');
		INSERT INTO workstations (id, data_collector_id, workspace_id, robot_id, status, is_current)
		VALUES (11, 7, 10, 101, 'offline', FALSE);
		INSERT INTO dc_plan (id, workspace_id, operator, dc_device_id) VALUES (1001, 10, 'dc01', 101);
		INSERT INTO ws_client_auth_tokens (robot_id, token_hash) VALUES (101, ?)
	`, hashWSClientAuthToken(deviceToken)); err != nil {
		t.Fatalf("seed ego activation: %v", err)
	}
	identityToken, err := auth.GenerateToken(auth.NewCollectorClaims(7, "dc01"), testAuthConfig())
	if err != nil {
		t.Fatalf("generate identity token: %v", err)
	}
	w := performAuthActivation(newTestAuthRouter(db, ""), identityToken, `{}`, deviceToken)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp WorkstationActivationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal activation response: %v", err)
	}
	claims, err := auth.ParseToken(resp.AccessToken, testAuthConfig())
	if err != nil || claims.WorkstationID != 11 || claims.RobotID != 101 {
		t.Fatalf("claims=%#v err=%v", claims, err)
	}
	var lastUsed sql.NullString
	if err := db.Get(&lastUsed, `SELECT last_used_at FROM ws_client_auth_tokens WHERE robot_id = 101`); err != nil {
		t.Fatalf("query device credential usage: %v", err)
	}
	if !lastUsed.Valid || lastUsed.String == "" {
		t.Fatalf("device credential last_used_at was not updated")
	}
}

func TestAuthHandlerActivateWorkstationRejectsOccupiedDevice(t *testing.T) {
	db := newTestAuthDB(t)
	defer db.Close()
	if _, err := db.Exec(`
		INSERT INTO data_collectors (id, name, operator_id, status) VALUES
			(7, 'Collector', 'dc01', 'active'), (8, 'Other', 'dc02', 'active');
		INSERT INTO robots (id, device_id, workspace_id, status) VALUES (101, '101', 10, 'active');
		INSERT INTO workstations (id, data_collector_id, workspace_id, robot_id, status, is_current) VALUES
			(11, 7, 10, 101, 'offline', FALSE), (12, 8, 10, 101, 'active', TRUE);
		INSERT INTO dc_plan (id, workspace_id, operator, dc_device_id) VALUES (1001, 10, 'dc01', 101)
	`); err != nil {
		t.Fatalf("seed occupied device: %v", err)
	}
	identityToken, err := auth.GenerateToken(auth.NewCollectorClaims(7, "dc01"), testAuthConfig())
	if err != nil {
		t.Fatalf("generate identity token: %v", err)
	}
	w := performAuthActivation(newTestAuthRouter(db, ""), identityToken, `{"workstation_id":11}`, "")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "workstation_occupied") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestAuthHandlerActivateWorkstationAllowsEgoPortalDeviceTakeover(t *testing.T) {
	db := newTestAuthDB(t)
	defer db.Close()
	deviceToken := "kda_v1_takeover-device-token"
	if _, err := db.Exec(`
		INSERT INTO data_collectors (id, name, operator_id, status) VALUES
			(7, 'Collector B', 'dc01', 'active'), (8, 'Collector A', 'dc02', 'active');
		INSERT INTO robots (id, device_id, workspace_id, status) VALUES (101, '101', 10, 'active');
		INSERT INTO workstations (
			id, data_collector_id, workspace_id, robot_id, status, is_current
		) VALUES
			(11, 7, 10, 101, 'offline', FALSE),
			(12, 8, 10, 101, 'active', TRUE);
		INSERT INTO dc_plan (id, workspace_id, operator, dc_device_id) VALUES (1001, 10, 'dc01', 101);
		INSERT INTO ws_client_auth_tokens (robot_id, token_hash) VALUES (101, ?)
	`, hashWSClientAuthToken(deviceToken)); err != nil {
		t.Fatalf("seed Ego Portal takeover: %v", err)
	}

	identityToken, err := auth.GenerateToken(auth.NewCollectorClaims(7, "dc01"), testAuthConfig())
	if err != nil {
		t.Fatalf("generate identity token: %v", err)
	}
	router := newTestAuthRouter(db, "")
	w := performAuthActivation(router, identityToken, `{}`, deviceToken)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var currentID int64
	if err := db.Get(&currentID, `
		SELECT id FROM workstations
		WHERE robot_id = 101 AND is_current = TRUE AND deleted_at IS NULL
	`); err != nil {
		t.Fatalf("query current workstation: %v", err)
	}
	if currentID != 11 {
		t.Fatalf("current workstation=%d want 11", currentID)
	}

	var replaced struct {
		Status       string        `db:"status"`
		IsCurrent    bool          `db:"is_current"`
		SupersededBy sql.NullInt64 `db:"superseded_by"`
	}
	if err := db.Get(&replaced, `
		SELECT status, is_current, superseded_by
		FROM workstations WHERE id = 12
	`); err != nil {
		t.Fatalf("query replaced workstation: %v", err)
	}
	if replaced.Status != "offline" || replaced.IsCurrent || !replaced.SupersededBy.Valid || replaced.SupersededBy.Int64 != 11 {
		t.Fatalf("replaced workstation=%#v", replaced)
	}

	oldToken, err := auth.GenerateToken(
		auth.NewCollectorWorkstationClaims(8, "dc02", 12, 101, 10),
		testAuthConfig(),
	)
	if err != nil {
		t.Fatalf("generate previous workstation token: %v", err)
	}
	oldRequest := httptest.NewRequest(http.MethodPost, "/api/v1/auth/me/station/break", nil)
	oldRequest.Header.Set("Authorization", "Bearer "+oldToken)
	oldResponse := httptest.NewRecorder()
	router.ServeHTTP(oldResponse, oldRequest)
	if oldResponse.Code != http.StatusUnauthorized || !strings.Contains(oldResponse.Body.String(), "workstation_session_invalid") {
		t.Fatalf("old session status=%d body=%s", oldResponse.Code, oldResponse.Body.String())
	}
}

func TestAuthHandlerActivateWorkstationRejectsEgoPortalTakeoverDuringActiveRecording(t *testing.T) {
	db := newTestAuthDB(t)
	defer db.Close()
	deviceToken := "kda_v1_recording-device-token"
	if _, err := db.Exec(`
		INSERT INTO data_collectors (id, name, operator_id, status) VALUES
			(7, 'Collector B', 'dc01', 'active'), (8, 'Collector A', 'dc02', 'active');
		INSERT INTO robots (id, device_id, workspace_id, status) VALUES (101, '101', 10, 'active');
		INSERT INTO workstations (
			id, data_collector_id, workspace_id, robot_id, status, is_current
		) VALUES
			(11, 7, 10, 101, 'offline', FALSE),
			(12, 8, 10, 101, 'active', TRUE);
		INSERT INTO tasks (id, workstation_id, status) VALUES (5001, 12, 'in_progress');
		INSERT INTO dc_plan (id, workspace_id, operator, dc_device_id) VALUES (1001, 10, 'dc01', 101);
		INSERT INTO ws_client_auth_tokens (robot_id, token_hash) VALUES (101, ?)
	`, hashWSClientAuthToken(deviceToken)); err != nil {
		t.Fatalf("seed active recording takeover: %v", err)
	}

	identityToken, err := auth.GenerateToken(auth.NewCollectorClaims(7, "dc01"), testAuthConfig())
	if err != nil {
		t.Fatalf("generate identity token: %v", err)
	}
	w := performAuthActivation(newTestAuthRouter(db, ""), identityToken, `{}`, deviceToken)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "recording_active") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	var currentID int64
	if err := db.Get(&currentID, `SELECT id FROM workstations WHERE robot_id = 101 AND is_current = TRUE`); err != nil {
		t.Fatalf("query current workstation: %v", err)
	}
	if currentID != 12 {
		t.Fatalf("current workstation=%d want 12", currentID)
	}
}

func TestAuthHandlerActivateWorkstationRejectsPreviousActiveRecording(t *testing.T) {
	db := newTestAuthDB(t)
	defer db.Close()
	if _, err := db.Exec(`
		INSERT INTO data_collectors (id, name, operator_id, status) VALUES
			(7, 'Collector', 'dc01', 'active'), (8, 'Other', 'dc02', 'active');
		INSERT INTO robots (id, device_id, workspace_id, status) VALUES (101, '101', 10, 'active');
		INSERT INTO workstations (id, data_collector_id, workspace_id, robot_id, status, is_current) VALUES
			(11, 7, 10, 101, 'offline', FALSE), (12, 8, 10, 101, 'offline', FALSE);
		INSERT INTO tasks (id, workstation_id, status) VALUES (5001, 12, 'in_progress');
		INSERT INTO dc_plan (id, workspace_id, operator, dc_device_id) VALUES (1001, 10, 'dc01', 101)
	`); err != nil {
		t.Fatalf("seed active recording: %v", err)
	}
	identityToken, err := auth.GenerateToken(auth.NewCollectorClaims(7, "dc01"), testAuthConfig())
	if err != nil {
		t.Fatalf("generate identity token: %v", err)
	}
	w := performAuthActivation(newTestAuthRouter(db, ""), identityToken, `{"workstation_id":11}`, "")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "recording_active") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestAuthHandlerActivateWorkstationReleasesCollectorOtherWorkspace(t *testing.T) {
	db := newTestAuthDB(t)
	defer db.Close()
	if _, err := db.Exec(`
		INSERT INTO workspaces (id, admins, members) VALUES (20, '[]', '["dc01"]');
		INSERT INTO data_collectors (id, name, operator_id, status) VALUES (7, 'Collector', 'dc01', 'active');
		INSERT INTO robots (id, device_id, workspace_id, status) VALUES
			(101, '101', 20, 'active'), (102, '102', 10, 'active');
		INSERT INTO workstations (id, data_collector_id, workspace_id, robot_id, status, is_current) VALUES
			(11, 7, 20, 101, 'active', TRUE), (12, 7, 10, 102, 'offline', FALSE);
		INSERT INTO dc_plan (id, workspace_id, operator, dc_device_id) VALUES (1001, 10, 'dc01', 102)
	`); err != nil {
		t.Fatalf("seed cross-workspace activation: %v", err)
	}
	identityToken, err := auth.GenerateToken(auth.NewCollectorClaims(7, "dc01"), testAuthConfig())
	if err != nil {
		t.Fatalf("generate identity token: %v", err)
	}
	w := performAuthActivation(newTestAuthRouter(db, ""), identityToken, `{"workstation_id":12}`, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var currentIDs []int64
	if err := db.Select(&currentIDs, `SELECT id FROM workstations WHERE data_collector_id = 7 AND is_current = TRUE ORDER BY id`); err != nil {
		t.Fatalf("query current workstations: %v", err)
	}
	if len(currentIDs) != 1 || currentIDs[0] != 12 {
		t.Fatalf("current workstations=%v want [12]", currentIDs)
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

func TestAuthHandlerCollectorLoginDoesNotRequireDeviceID(t *testing.T) {
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
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp LoginResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	claims, err := auth.ParseToken(resp.AccessToken, testAuthConfig())
	if err != nil || claims.WorkstationID != 0 {
		t.Fatalf("identity claims=%#v err=%v", claims, err)
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
		INSERT INTO robots (id, device_id, workspace_id, status) VALUES
			(101, '101', 10, 'active'), (102, '102', 20, 'active');
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
	handler := NewAuthHandler(db, testAuthConfig(), testHilbertConfig(hilbertBaseURL))
	api := router.Group("/api/v1")
	handler.RegisterRoutes(api)
	identityMw := middleware.IdentityJWTAuth(testAuthConfig())
	workstationMw := middleware.JWTAuth(testAuthConfig(), db)
	handler.RegisterAuthenticatedRoutes(
		api.Group("/auth/me", identityMw),
		api.Group("/auth/me/station", workstationMw, middleware.RequireRole("data_collector")),
		api.Group("/auth/workstation", identityMw, middleware.RequireRole("data_collector")),
	)
	return router
}

func performAuthLogin(router *gin.Engine, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func performAuthActivation(router *gin.Engine, token string, body string, deviceToken string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/workstation/activate", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	if deviceToken != "" {
		req.Header.Set("Device-Authorization", deviceToken)
	}
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
			name TEXT NOT NULL DEFAULT 'Workspace',
			admins TEXT NOT NULL,
			members TEXT NOT NULL,
			deleted_at TEXT
		)`,
		`INSERT INTO workspaces (id, name, admins, members) VALUES (10, 'Test Workspace', '[]', '["dc01"]')`,
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
			device_name TEXT,
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
		`CREATE TABLE dc_plan (
			id INTEGER PRIMARY KEY,
			workspace_id INTEGER NOT NULL,
			operator TEXT NOT NULL,
			dc_device_id INTEGER NOT NULL,
			deleted_at TEXT
		)`,
		`CREATE TABLE ws_client_auth_tokens (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			robot_id INTEGER NOT NULL,
			token_hash TEXT NOT NULL,
			last_used_at TEXT,
			revoked_at TEXT
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
