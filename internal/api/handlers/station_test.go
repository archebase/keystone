// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"database/sql"
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

func TestStationHandlerListStations_FilterByWorkstationFields(t *testing.T) {
	db := newTestStationHandlerDB(t)
	defer db.Close()

	now := time.Now().UTC()
	stmts := []struct {
		sql  string
		args []any
	}{
		{sql: `INSERT INTO workspaces (id, name, deleted_at) VALUES (60, 'Org A', NULL)`},
		{sql: `INSERT INTO workspaces (id, name, deleted_at) VALUES (61, 'Org B', NULL)`},
		{sql: `INSERT INTO robots (id, device_id, workspace_id, deleted_at) VALUES (1, 'device-a', 60, NULL)`},
		{sql: `INSERT INTO robots (id, device_id, workspace_id, deleted_at) VALUES (2, 'device-b', 61, NULL)`},
		{sql: `INSERT INTO robots (id, device_id, workspace_id, metadata, deleted_at) VALUES (3, 'device-c', 60, '{"hilbert_dc_device_name":"Device C"}', NULL)`},
		{
			sql: `INSERT INTO workstations (
				id, robot_id, robot_name, robot_serial, data_collector_id,
				collector_name, collector_operator_id, workspace_id,
				name, status, metadata, created_at, updated_at, deleted_at
			) VALUES (1, 1, 'device-a', 'device-a', 100, 'Alice', 'C001', 60, 'ws-a', 'active', '{}', ?, ?, NULL)`,
			args: []any{now, now},
		},
		{
			sql: `INSERT INTO workstations (
				id, robot_id, robot_name, robot_serial, data_collector_id,
				collector_name, collector_operator_id, workspace_id,
				name, status, metadata, created_at, updated_at, deleted_at
			) VALUES (2, 2, 'device-b', 'device-b', 101, 'Bob', 'C002', 61, 'ws-b', 'inactive', '{}', ?, ?, NULL)`,
			args: []any{now, now},
		},
		{
			sql: `INSERT INTO workstations (
				id, robot_id, robot_name, robot_serial, data_collector_id,
				collector_name, collector_operator_id, workspace_id,
				name, status, metadata, created_at, updated_at, deleted_at
			) VALUES (3, 3, 'device-c', 'device-c', 102, 'Alice', 'C003', 60, 'ws-c', 'offline', '{}', ?, ?, NULL)`,
			args: []any{now, now},
		},
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed station fixture failed: %v", err)
		}
	}

	r := newTestStationRouter(t, db)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stations?device_id=device-a,device-c&collector_name=Alice&collector_operator_id=C003&workspace_id=60&status=offline", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		Items []StationResponse `json:"items"`
		Total int               `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v body=%s", err, w.Body.String())
	}
	if resp.Total != 1 || len(resp.Items) != 1 {
		t.Fatalf("unexpected filtered response: %#v", resp)
	}
	got := resp.Items[0]
	if got.ID != "3" || got.RobotSerial != "device-c" || got.RobotDeviceName != "Device C" || got.CollectorName != "Alice" || got.CollectorOperatorID != "C003" || got.WorkspaceID != "60" {
		t.Fatalf("unexpected station item: %#v", got)
	}
}

func TestStationHandlerListStations_SearchesRobotDeviceName(t *testing.T) {
	db := newTestStationHandlerDB(t)
	defer db.Close()

	now := time.Now().UTC()
	stmts := []struct {
		sql  string
		args []any
	}{
		{sql: `INSERT INTO workspaces (id, name, deleted_at) VALUES (60, 'Org A', NULL)`},
		{sql: `INSERT INTO robots (id, device_id, workspace_id, metadata, deleted_at) VALUES (1, 'device-a', 60, '{"hilbert_dc_device_name":"Inspection Cart A"}', NULL)`},
		{
			sql: `INSERT INTO workstations (
				id, robot_id, robot_name, robot_serial, data_collector_id,
				collector_name, collector_operator_id, workspace_id,
				name, status, metadata, created_at, updated_at, deleted_at
			) VALUES (1, 1, 'device-a', 'device-a', 100, 'Alice', 'C001', 60, 'ws-a', 'active', '{}', ?, ?, NULL)`,
			args: []any{now, now},
		},
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed station fixture failed: %v", err)
		}
	}

	r := newTestStationRouter(t, db)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stations?search=Inspection", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		Items []StationResponse `json:"items"`
		Total int               `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v body=%s", err, w.Body.String())
	}
	if resp.Total != 1 || len(resp.Items) != 1 {
		t.Fatalf("unexpected search response: %#v", resp)
	}
	if got := resp.Items[0].RobotDeviceName; got != "Inspection Cart A" {
		t.Fatalf("RobotDeviceName=%q want=%q", got, "Inspection Cart A")
	}
}

func TestStationHandlerListStations_IncludesAvailableNonCurrentBindings(t *testing.T) {
	db := newTestStationHandlerDB(t)
	defer db.Close()

	now := time.Now().UTC()
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{sql: `INSERT INTO workspaces (id, name, deleted_at) VALUES (60, 'Org A', NULL)`},
		{sql: `INSERT INTO robots (id, device_id, workspace_id, deleted_at) VALUES (1, 'device-a', 60, NULL), (2, 'device-b', 60, NULL), (3, 'device-c', 60, NULL)`},
		{
			sql: `INSERT INTO workstations (
				id, robot_id, robot_name, robot_serial, data_collector_id,
				collector_name, collector_operator_id, workspace_id,
				name, status, is_current, superseded_at, metadata, created_at, updated_at, deleted_at
			) VALUES
				(1, 1, 'device-a', 'device-a', 100, 'Alice', 'C001', 60, 'ws-current', 'active', TRUE, NULL, '{}', ?, ?, NULL),
				(2, 2, 'device-b', 'device-b', 100, 'Alice', 'C001', 60, 'ws-planned', 'offline', FALSE, NULL, '{}', ?, ?, NULL),
				(3, 3, 'device-c', 'device-c', 100, 'Alice', 'C001', 60, 'ws-old', 'offline', FALSE, ?, '{}', ?, ?, NULL)`,
			args: []any{now, now, now, now, now, now, now},
		},
	} {
		if _, err := db.Exec(stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed station fixture failed: %v", err)
		}
	}

	r := newTestStationRouter(t, db)

	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/stations?workspace_id=60", nil)
	listW := httptest.NewRecorder()
	r.ServeHTTP(listW, listReq)
	if listW.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", listW.Code, listW.Body.String())
	}
	var listResp struct {
		Items []StationResponse `json:"items"`
		Total int               `json:"total"`
	}
	if err := json.Unmarshal(listW.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("unmarshal list response: %v body=%s", err, listW.Body.String())
	}
	if listResp.Total != 2 || len(listResp.Items) != 2 {
		t.Fatalf("available current and planned stations should appear: %#v", listResp)
	}
	if listResp.Items[0].ID != "2" || listResp.Items[0].IsCurrent || listResp.Items[1].ID != "1" || !listResp.Items[1].IsCurrent {
		t.Fatalf("unexpected available station ordering or state: %#v", listResp.Items)
	}

	currentReq := httptest.NewRequest(http.MethodGet, "/api/v1/stations?workspace_id=60&is_current=true", nil)
	currentW := httptest.NewRecorder()
	r.ServeHTTP(currentW, currentReq)
	if currentW.Code != http.StatusOK {
		t.Fatalf("current list status=%d body=%s", currentW.Code, currentW.Body.String())
	}
	var currentResp struct {
		Items []StationResponse `json:"items"`
		Total int               `json:"total"`
	}
	if err := json.Unmarshal(currentW.Body.Bytes(), &currentResp); err != nil {
		t.Fatalf("unmarshal current response: %v body=%s", err, currentW.Body.String())
	}
	if currentResp.Total != 1 || len(currentResp.Items) != 1 || currentResp.Items[0].ID != "1" {
		t.Fatalf("current filter should only return current station: %#v", currentResp)
	}
}

func TestStationHandlerCreateRejectsCrossWorkspaceBinding(t *testing.T) {
	db := newTestStationHandlerDB(t)
	defer db.Close()
	for _, stmt := range []string{
		`INSERT INTO workspaces (id, name, members, deleted_at) VALUES (60, 'Workspace A', '["C001"]', NULL), (61, 'Workspace B', '[]', NULL)`,
		`INSERT INTO robots (id, device_id, workspace_id, status, deleted_at) VALUES (1, 'device-a', 61, 'active', NULL)`,
		`INSERT INTO data_collectors (id, name, operator_id, status, deleted_at) VALUES (100, 'Alice', 'C001', 'active', NULL)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed cross-workspace fixture: %v", err)
		}
	}

	r := newTestStationRouter(t, db)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/stations", strings.NewReader(`{"robot_id":"1","data_collector_id":"100"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "workspace") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestStationHandlerUpdateUsesCurrentWorkspaceFields(t *testing.T) {
	db := newTestStationHandlerDB(t)
	defer db.Close()

	now := time.Now().UTC()
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{sql: `INSERT INTO workspaces (id, name, members, deleted_at) VALUES (60, 'Workspace A', '["C001"]', NULL)`},
		{sql: `INSERT INTO robots (id, device_id, workspace_id, status, deleted_at) VALUES (1, 'device-a', 60, 'active', NULL)`},
		{sql: `INSERT INTO data_collectors (id, name, operator_id, status, deleted_at) VALUES (100, 'Alice', 'C001', 'active', NULL)`},
		{
			sql: `INSERT INTO workstations (
				id, robot_id, robot_name, robot_serial, data_collector_id,
				collector_name, collector_operator_id, workspace_id,
				name, status, is_current, metadata, created_at, updated_at, deleted_at
			) VALUES (1, 1, 'device-a', 'device-a', 100, 'Alice', 'C001', 60, 'ws-a', 'offline', TRUE, '{}', ?, ?, NULL)`,
			args: []any{now, now},
		},
	} {
		if _, err := db.Exec(stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed update fixture: %v", err)
		}
	}

	r := newTestStationRouter(t, db)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/stations/1", strings.NewReader(`{
		"robot_id":"1",
		"data_collector_id":"100",
		"status":"active",
		"metadata":{"mode":"current"}
	}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	var updated StationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &updated); err != nil {
		t.Fatalf("unmarshal update response: %v", err)
	}
	if updated.WorkspaceID != "60" || updated.Status != "active" || updated.RobotSerial != "device-a" {
		t.Fatalf("unexpected update response: %#v", updated)
	}
	if got, ok := updated.Metadata.(map[string]interface{})["mode"].(string); !ok || got != "current" {
		t.Fatalf("metadata not updated: %#v", updated.Metadata)
	}
}

func TestStationHandlerUpdateRejectsRebindingWithPendingTasks(t *testing.T) {
	db := newTestStationHandlerDB(t)
	defer db.Close()

	now := time.Now().UTC()
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{sql: `INSERT INTO workspaces (id, name, members, deleted_at) VALUES (60, 'Workspace A', '["C001"]', NULL)`},
		{sql: `INSERT INTO robots (id, device_id, workspace_id, status, deleted_at) VALUES
			(1, 'device-a', 60, 'active', NULL),
			(2, 'device-b', 60, 'active', NULL)`},
		{sql: `INSERT INTO data_collectors (id, name, operator_id, status, deleted_at) VALUES (100, 'Alice', 'C001', 'active', NULL)`},
		{
			sql: `INSERT INTO workstations (
				id, robot_id, robot_name, robot_serial, data_collector_id,
				collector_name, collector_operator_id, workspace_id,
				name, status, is_current, metadata, created_at, updated_at, deleted_at
			) VALUES (1, 1, 'device-a', 'device-a', 100, 'Alice', 'C001', 60, 'ws-a', 'active', TRUE, '{}', ?, ?, NULL)`,
			args: []any{now, now},
		},
		{sql: `INSERT INTO tasks (id, workstation_id, status, deleted_at) VALUES (1, 1, 'pending', NULL)`},
	} {
		if _, err := db.Exec(stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed rebinding fixture: %v", err)
		}
	}

	router := newTestStationRouter(t, db)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/stations/1", strings.NewReader(`{"robot_id":"2"}`))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status=%d want=%d body=%s", recorder.Code, http.StatusConflict, recorder.Body.String())
	}
	var robotID int64
	if err := db.Get(&robotID, `SELECT robot_id FROM workstations WHERE id = 1`); err != nil {
		t.Fatalf("query station robot: %v", err)
	}
	if robotID != 1 {
		t.Fatalf("robot_id=%d want original robot 1", robotID)
	}
}

func TestStationHandlerDeleteUnbindsAndCreateReusesHistoricalBinding(t *testing.T) {
	db := newTestStationHandlerDB(t)
	defer db.Close()

	now := time.Now().UTC()
	stmts := []struct {
		sql  string
		args []any
	}{
		{sql: `INSERT INTO workspaces (id, name, members, deleted_at) VALUES (60, 'Org A', '["C001"]', NULL)`},
		{sql: `INSERT INTO data_collectors (id, name, operator_id, status, deleted_at) VALUES (100, 'Alice', 'C001', 'active', NULL)`},
		{sql: `INSERT INTO robots (id, device_id, workspace_id, status, deleted_at) VALUES (1, 'device-a', 60, 'active', NULL)`},
		{
			sql: `INSERT INTO workstations (
				id, robot_id, robot_name, robot_serial, data_collector_id,
				collector_name, collector_operator_id, workspace_id,
				name, status, is_current, metadata, created_at, updated_at, deleted_at
			) VALUES (1, 1, 'device-a', 'device-a', 100, 'Alice', 'C001', 60, 'ws-a', 'active', TRUE, '{"old":true}', ?, ?, NULL)`,
			args: []any{now, now},
		},
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed station fixture failed: %v", err)
		}
	}

	r := newTestStationRouter(t, db)

	deleteReq := httptest.NewRequest(http.MethodDelete, "/api/v1/stations/1", nil)
	deleteW := httptest.NewRecorder()
	r.ServeHTTP(deleteW, deleteReq)
	if deleteW.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s", deleteW.Code, deleteW.Body.String())
	}

	var unbound struct {
		IsCurrent    bool         `db:"is_current"`
		SupersededAt sql.NullTime `db:"superseded_at"`
		DeletedAt    sql.NullTime `db:"deleted_at"`
	}
	if err := db.Get(&unbound, "SELECT is_current, superseded_at, deleted_at FROM workstations WHERE id = 1"); err != nil {
		t.Fatalf("query unbound workstation: %v", err)
	}
	if unbound.IsCurrent || !unbound.SupersededAt.Valid || unbound.DeletedAt.Valid {
		t.Fatalf("delete should unbind without soft-deleting: %#v", unbound)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/stations", nil)
	listW := httptest.NewRecorder()
	r.ServeHTTP(listW, listReq)
	if listW.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", listW.Code, listW.Body.String())
	}
	var listResp struct {
		Items []StationResponse `json:"items"`
		Total int               `json:"total"`
	}
	if err := json.Unmarshal(listW.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("unmarshal list response: %v body=%s", err, listW.Body.String())
	}
	if listResp.Total != 0 || len(listResp.Items) != 0 {
		t.Fatalf("unbound station should not appear in current list: %#v", listResp)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/stations", strings.NewReader(`{"robot_id":"1","data_collector_id":"100","metadata":{"new":true}}`))
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	r.ServeHTTP(createW, createReq)
	if createW.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", createW.Code, createW.Body.String())
	}

	var recreated StationResponse
	if err := json.Unmarshal(createW.Body.Bytes(), &recreated); err != nil {
		t.Fatalf("unmarshal create response: %v body=%s", err, createW.Body.String())
	}
	if recreated.ID != "1" || !recreated.IsCurrent || recreated.Status != "offline" {
		t.Fatalf("create should reuse unbound workstation: %#v", recreated)
	}
	if got, ok := recreated.Metadata.(map[string]interface{})["new"].(bool); !ok || !got {
		t.Fatalf("reused workstation metadata was not refreshed: %#v", recreated.Metadata)
	}
}

func TestStationHandlerDeleteRejectsPendingOrActiveTasks(t *testing.T) {
	db := newTestStationHandlerDB(t)
	defer db.Close()

	now := time.Now().UTC()
	stmts := []struct {
		sql  string
		args []any
	}{
		{sql: `INSERT INTO workspaces (id, name, members, deleted_at) VALUES (60, 'Org A', '["C001"]', NULL)`},
		{sql: `INSERT INTO data_collectors (id, name, operator_id, status, deleted_at) VALUES (100, 'Alice', 'C001', 'active', NULL)`},
		{sql: `INSERT INTO robots (id, device_id, workspace_id, status, deleted_at) VALUES (1, 'device-a', 60, 'active', NULL)`},
		{
			sql: `INSERT INTO workstations (
				id, robot_id, robot_name, robot_serial, data_collector_id,
				collector_name, collector_operator_id, workspace_id,
				name, status, is_current, metadata, created_at, updated_at, deleted_at
			) VALUES (1, 1, 'device-a', 'device-a', 100, 'Alice', 'C001', 60, 'ws-a', 'active', TRUE, '{}', ?, ?, NULL)`,
			args: []any{now, now},
		},
		{
			sql:  `INSERT INTO tasks (id, workstation_id, status, deleted_at) VALUES (1, 1, 'pending', NULL)`,
			args: nil,
		},
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed station fixture failed: %v", err)
		}
	}

	r := newTestStationRouter(t, db)
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/stations/1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusConflict, w.Body.String())
	}

	var current bool
	if err := db.Get(&current, "SELECT is_current FROM workstations WHERE id = 1"); err != nil {
		t.Fatalf("query workstation current flag: %v", err)
	}
	if !current {
		t.Fatalf("station should remain current when unbind is rejected")
	}
}

func newTestStationRouter(t *testing.T, db *sqlx.DB) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewStationHandler(db).RegisterRoutes(r.Group("/api/v1"))
	return r
}

func newTestStationHandlerDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}

	schema := []string{
		`CREATE TABLE workspaces (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			admins TEXT NOT NULL DEFAULT '[]',
			members TEXT NOT NULL DEFAULT '[]',
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE robots (
			id INTEGER PRIMARY KEY,
			device_id TEXT NOT NULL,
			workspace_id INTEGER NOT NULL DEFAULT 60,
			status TEXT NOT NULL DEFAULT 'active',
			metadata TEXT,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE data_collectors (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			operator_id TEXT NOT NULL,
			status TEXT NOT NULL,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE workstations (
			id INTEGER PRIMARY KEY,
			robot_id INTEGER NOT NULL,
			robot_name TEXT,
			robot_serial TEXT,
			data_collector_id INTEGER NOT NULL,
			collector_name TEXT,
			collector_operator_id TEXT,
			workspace_id INTEGER NOT NULL,
			name TEXT,
			status TEXT NOT NULL,
			is_current BOOLEAN NOT NULL DEFAULT TRUE,
			superseded_at TIMESTAMP NULL,
			superseded_by INTEGER NULL,
			metadata TEXT,
			created_at TIMESTAMP,
			updated_at TIMESTAMP,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE tasks (
			id INTEGER PRIMARY KEY,
			workstation_id INTEGER NOT NULL,
			status TEXT NOT NULL,
			deleted_at TIMESTAMP NULL
		)`,
	}
	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("create station schema failed: %v", err)
		}
	}

	return db
}
