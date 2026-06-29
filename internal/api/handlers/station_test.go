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
		{sql: `INSERT INTO robot_types (id, model, deleted_at) VALUES (10, 'arm-v1', NULL)`},
		{sql: `INSERT INTO robot_types (id, model, deleted_at) VALUES (11, 'arm-v2', NULL)`},
		{sql: `INSERT INTO factories (id, name, deleted_at) VALUES (30, 'F1', NULL)`},
		{sql: `INSERT INTO factories (id, name, deleted_at) VALUES (31, 'F2', NULL)`},
		{sql: `INSERT INTO organizations (id, name, deleted_at) VALUES (60, 'Org A', NULL)`},
		{sql: `INSERT INTO organizations (id, name, deleted_at) VALUES (61, 'Org B', NULL)`},
		{sql: `INSERT INTO robots (id, robot_type_id, device_id, factory_id, deleted_at) VALUES (1, 10, 'device-a', 30, NULL)`},
		{sql: `INSERT INTO robots (id, robot_type_id, device_id, factory_id, deleted_at) VALUES (2, 10, 'device-b', 30, NULL)`},
		{sql: `INSERT INTO robots (id, robot_type_id, device_id, factory_id, deleted_at) VALUES (3, 11, 'device-c', 31, NULL)`},
		{
			sql: `INSERT INTO workstations (
				id, robot_id, robot_name, robot_serial, data_collector_id,
				collector_name, collector_operator_id, factory_id, organization_id,
				name, status, metadata, created_at, updated_at, deleted_at
			) VALUES (1, 1, 'arm-v1', 'device-a', 100, 'Alice', 'C001', 30, 60, 'ws-a', 'active', '{}', ?, ?, NULL)`,
			args: []any{now, now},
		},
		{
			sql: `INSERT INTO workstations (
				id, robot_id, robot_name, robot_serial, data_collector_id,
				collector_name, collector_operator_id, factory_id, organization_id,
				name, status, metadata, created_at, updated_at, deleted_at
			) VALUES (2, 2, 'arm-v1', 'device-b', 101, 'Bob', 'C002', 30, 61, 'ws-b', 'inactive', '{}', ?, ?, NULL)`,
			args: []any{now, now},
		},
		{
			sql: `INSERT INTO workstations (
				id, robot_id, robot_name, robot_serial, data_collector_id,
				collector_name, collector_operator_id, factory_id, organization_id,
				name, status, metadata, created_at, updated_at, deleted_at
			) VALUES (3, 3, 'arm-v2', 'device-c', 102, 'Alice', 'C003', 31, 60, 'ws-c', 'offline', '{}', ?, ?, NULL)`,
			args: []any{now, now},
		},
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed station fixture failed: %v", err)
		}
	}

	r := newTestStationRouter(t, db)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stations?device_id=device-a,device-c&collector_name=Alice&collector_operator_id=C003&factory_id=31&organization_id=60&robot_type_id=11&status=offline", nil)
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
	if got.ID != "3" || got.RobotSerial != "device-c" || got.CollectorName != "Alice" || got.CollectorOperatorID != "C003" {
		t.Fatalf("unexpected station item: %#v", got)
	}
}

func TestStationHandlerUpdateStationCreatesNewBindingVersion(t *testing.T) {
	db := newTestStationHandlerDB(t)
	defer db.Close()

	now := time.Now().UTC()
	stmts := []struct {
		sql  string
		args []any
	}{
		{sql: `INSERT INTO robot_types (id, name, model, deleted_at) VALUES (10, 'arm-v1', 'arm-v1', NULL)`},
		{sql: `INSERT INTO robot_types (id, name, model, deleted_at) VALUES (11, 'arm-v2', 'arm-v2', NULL)`},
		{sql: `INSERT INTO factories (id, name, deleted_at) VALUES (30, 'F1', NULL)`},
		{sql: `INSERT INTO organizations (id, factory_id, name, deleted_at) VALUES (60, 30, 'Org A', NULL)`},
		{sql: `INSERT INTO data_collectors (id, organization_id, name, operator_id, status, deleted_at) VALUES (100, 60, 'Alice', 'C001', 'active', NULL)`},
		{sql: `INSERT INTO robots (id, robot_type_id, device_id, factory_id, status, deleted_at) VALUES (1, 10, 'device-a', 30, 'active', NULL)`},
		{sql: `INSERT INTO robots (id, robot_type_id, device_id, factory_id, status, deleted_at) VALUES (2, 11, 'device-b', 30, 'active', NULL)`},
		{
			sql: `INSERT INTO workstations (
				id, robot_id, robot_name, robot_serial, data_collector_id,
				collector_name, collector_operator_id, factory_id, organization_id,
				name, status, is_current, metadata, created_at, updated_at, deleted_at
			) VALUES (1, 1, 'arm-v1', 'device-a', 100, 'Alice', 'C001', 30, 60, 'ws-a', 'active', TRUE, '{}', ?, ?, NULL)`,
			args: []any{now, now},
		},
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed station fixture failed: %v", err)
		}
	}

	r := newTestStationRouter(t, db)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/stations/1", strings.NewReader(`{"robot_id":"2"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var updated StationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &updated); err != nil {
		t.Fatalf("unmarshal response: %v body=%s", err, w.Body.String())
	}
	if updated.ID == "1" || updated.RobotID != "2" || updated.RobotSerial != "device-b" || !updated.IsCurrent {
		t.Fatalf("unexpected update response: %#v", updated)
	}

	var old struct {
		IsCurrent    bool          `db:"is_current"`
		SupersededBy sql.NullInt64 `db:"superseded_by"`
	}
	if err := db.Get(&old, "SELECT is_current, superseded_by FROM workstations WHERE id = 1"); err != nil {
		t.Fatalf("query old workstation: %v", err)
	}
	if old.IsCurrent || !old.SupersededBy.Valid {
		t.Fatalf("old workstation was not superseded: %#v", old)
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
	if listResp.Total != 1 || len(listResp.Items) != 1 || listResp.Items[0].ID != updated.ID {
		t.Fatalf("list should include only current binding: %#v", listResp)
	}
}

func TestStationHandlerUpdateStationReusesHistoricalBindingVersion(t *testing.T) {
	db := newTestStationHandlerDB(t)
	defer db.Close()

	now := time.Now().UTC()
	stmts := []struct {
		sql  string
		args []any
	}{
		{sql: `INSERT INTO robot_types (id, name, model, deleted_at) VALUES (10, 'arm-v1', 'arm-v1', NULL)`},
		{sql: `INSERT INTO robot_types (id, name, model, deleted_at) VALUES (11, 'arm-v2', 'arm-v2', NULL)`},
		{sql: `INSERT INTO factories (id, name, deleted_at) VALUES (30, 'F1', NULL)`},
		{sql: `INSERT INTO organizations (id, factory_id, name, deleted_at) VALUES (60, 30, 'Org A', NULL)`},
		{sql: `INSERT INTO data_collectors (id, organization_id, name, operator_id, status, deleted_at) VALUES (100, 60, 'Alice', 'C001', 'active', NULL)`},
		{sql: `INSERT INTO robots (id, robot_type_id, device_id, factory_id, status, deleted_at) VALUES (1, 10, 'device-a', 30, 'active', NULL)`},
		{sql: `INSERT INTO robots (id, robot_type_id, device_id, factory_id, status, deleted_at) VALUES (2, 11, 'device-b', 30, 'active', NULL)`},
		{
			sql: `INSERT INTO workstations (
				id, robot_id, robot_name, robot_serial, data_collector_id,
				collector_name, collector_operator_id, factory_id, organization_id,
				name, status, is_current, metadata, created_at, updated_at, deleted_at
			) VALUES (1, 1, 'arm-v1', 'device-a', 100, 'Alice', 'C001', 30, 60, 'ws-a', 'active', TRUE, '{}', ?, ?, NULL)`,
			args: []any{now, now},
		},
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed station fixture failed: %v", err)
		}
	}

	r := newTestStationRouter(t, db)

	toRobotBReq := httptest.NewRequest(http.MethodPut, "/api/v1/stations/1", strings.NewReader(`{"robot_id":"2"}`))
	toRobotBReq.Header.Set("Content-Type", "application/json")
	toRobotBW := httptest.NewRecorder()
	r.ServeHTTP(toRobotBW, toRobotBReq)
	if toRobotBW.Code != http.StatusOK {
		t.Fatalf("switch to robot B status=%d body=%s", toRobotBW.Code, toRobotBW.Body.String())
	}

	var robotBStation StationResponse
	if err := json.Unmarshal(toRobotBW.Body.Bytes(), &robotBStation); err != nil {
		t.Fatalf("unmarshal robot B response: %v body=%s", err, toRobotBW.Body.String())
	}
	if robotBStation.ID == "1" || robotBStation.RobotID != "2" || !robotBStation.IsCurrent {
		t.Fatalf("unexpected robot B response: %#v", robotBStation)
	}

	toRobotAReq := httptest.NewRequest(http.MethodPut, "/api/v1/stations/"+robotBStation.ID, strings.NewReader(`{"robot_id":"1","status":"inactive","metadata":{"phase":"back"}}`))
	toRobotAReq.Header.Set("Content-Type", "application/json")
	toRobotAW := httptest.NewRecorder()
	r.ServeHTTP(toRobotAW, toRobotAReq)
	if toRobotAW.Code != http.StatusOK {
		t.Fatalf("switch back to robot A status=%d body=%s", toRobotAW.Code, toRobotAW.Body.String())
	}

	var robotAStation StationResponse
	if err := json.Unmarshal(toRobotAW.Body.Bytes(), &robotAStation); err != nil {
		t.Fatalf("unmarshal robot A response: %v body=%s", err, toRobotAW.Body.String())
	}
	if robotAStation.ID != "1" || robotAStation.RobotID != "1" || robotAStation.RobotSerial != "device-a" || !robotAStation.IsCurrent {
		t.Fatalf("historical robot A binding was not reused: %#v", robotAStation)
	}
	if robotAStation.Status != "inactive" {
		t.Fatalf("reused station status was not refreshed: %#v", robotAStation)
	}

	var robotBRow struct {
		IsCurrent    bool          `db:"is_current"`
		SupersededBy sql.NullInt64 `db:"superseded_by"`
	}
	if err := db.Get(&robotBRow, "SELECT is_current, superseded_by FROM workstations WHERE id = ?", robotBStation.ID); err != nil {
		t.Fatalf("query robot B workstation: %v", err)
	}
	if robotBRow.IsCurrent || !robotBRow.SupersededBy.Valid || robotBRow.SupersededBy.Int64 != 1 {
		t.Fatalf("robot B workstation was not superseded by reused robot A binding: %#v", robotBRow)
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
	if listResp.Total != 1 || len(listResp.Items) != 1 || listResp.Items[0].ID != "1" {
		t.Fatalf("list should include only the reused current binding: %#v", listResp)
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
		{sql: `INSERT INTO robot_types (id, name, model, deleted_at) VALUES (10, 'arm-v1', 'arm-v1', NULL)`},
		{sql: `INSERT INTO factories (id, name, deleted_at) VALUES (30, 'F1', NULL)`},
		{sql: `INSERT INTO organizations (id, factory_id, name, deleted_at) VALUES (60, 30, 'Org A', NULL)`},
		{sql: `INSERT INTO data_collectors (id, organization_id, name, operator_id, status, deleted_at) VALUES (100, 60, 'Alice', 'C001', 'active', NULL)`},
		{sql: `INSERT INTO robots (id, robot_type_id, device_id, factory_id, status, deleted_at) VALUES (1, 10, 'device-a', 30, 'active', NULL)`},
		{
			sql: `INSERT INTO workstations (
				id, robot_id, robot_name, robot_serial, data_collector_id,
				collector_name, collector_operator_id, factory_id, organization_id,
				name, status, is_current, metadata, created_at, updated_at, deleted_at
			) VALUES (1, 1, 'arm-v1', 'device-a', 100, 'Alice', 'C001', 30, 60, 'ws-a', 'active', TRUE, '{"old":true}', ?, ?, NULL)`,
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

func TestStationHandlerUpdateHistoricalStationReturnsNotFound(t *testing.T) {
	db := newTestStationHandlerDB(t)
	defer db.Close()

	now := time.Now().UTC()
	stmts := []struct {
		sql  string
		args []any
	}{
		{sql: `INSERT INTO robot_types (id, name, model, deleted_at) VALUES (10, 'arm-v1', 'arm-v1', NULL)`},
		{sql: `INSERT INTO factories (id, name, deleted_at) VALUES (30, 'F1', NULL)`},
		{sql: `INSERT INTO organizations (id, factory_id, name, deleted_at) VALUES (60, 30, 'Org A', NULL)`},
		{sql: `INSERT INTO data_collectors (id, organization_id, name, operator_id, status, deleted_at) VALUES (100, 60, 'Alice', 'C001', 'active', NULL)`},
		{sql: `INSERT INTO robots (id, robot_type_id, device_id, factory_id, status, deleted_at) VALUES (1, 10, 'device-a', 30, 'active', NULL)`},
		{
			sql: `INSERT INTO workstations (
				id, robot_id, robot_name, robot_serial, data_collector_id,
				collector_name, collector_operator_id, factory_id, organization_id,
				name, status, is_current, superseded_at, metadata, created_at, updated_at, deleted_at
			) VALUES (1, 1, 'arm-v1', 'device-a', 100, 'Alice', 'C001', 30, 60, 'ws-a', 'active', FALSE, ?, '{}', ?, ?, NULL)`,
			args: []any{now, now, now},
		},
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed station fixture failed: %v", err)
		}
	}

	r := newTestStationRouter(t, db)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/stations/1", strings.NewReader(`{"status":"offline"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusNotFound, w.Body.String())
	}
}

func TestStationHandlerDeleteRejectsPendingOrActiveBatches(t *testing.T) {
	db := newTestStationHandlerDB(t)
	defer db.Close()

	now := time.Now().UTC()
	stmts := []struct {
		sql  string
		args []any
	}{
		{sql: `INSERT INTO robot_types (id, name, model, deleted_at) VALUES (10, 'arm-v1', 'arm-v1', NULL)`},
		{sql: `INSERT INTO factories (id, name, deleted_at) VALUES (30, 'F1', NULL)`},
		{sql: `INSERT INTO organizations (id, factory_id, name, deleted_at) VALUES (60, 30, 'Org A', NULL)`},
		{sql: `INSERT INTO data_collectors (id, organization_id, name, operator_id, status, deleted_at) VALUES (100, 60, 'Alice', 'C001', 'active', NULL)`},
		{sql: `INSERT INTO robots (id, robot_type_id, device_id, factory_id, status, deleted_at) VALUES (1, 10, 'device-a', 30, 'active', NULL)`},
		{
			sql: `INSERT INTO workstations (
				id, robot_id, robot_name, robot_serial, data_collector_id,
				collector_name, collector_operator_id, factory_id, organization_id,
				name, status, is_current, metadata, created_at, updated_at, deleted_at
			) VALUES (1, 1, 'arm-v1', 'device-a', 100, 'Alice', 'C001', 30, 60, 'ws-a', 'active', TRUE, '{}', ?, ?, NULL)`,
			args: []any{now, now},
		},
		{
			sql:  `INSERT INTO batches (id, workstation_id, status, deleted_at) VALUES (1, 1, 'pending', NULL)`,
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
		`CREATE TABLE robot_types (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE factories (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE organizations (
			id INTEGER PRIMARY KEY,
			factory_id INTEGER NOT NULL DEFAULT 30,
			name TEXT NOT NULL,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE robots (
			id INTEGER PRIMARY KEY,
			robot_type_id INTEGER NOT NULL,
			device_id TEXT NOT NULL,
			factory_id INTEGER NOT NULL,
			status TEXT NOT NULL DEFAULT 'active',
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE data_collectors (
			id INTEGER PRIMARY KEY,
			organization_id INTEGER NOT NULL,
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
			factory_id INTEGER NOT NULL,
			organization_id INTEGER NOT NULL,
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
		`CREATE TABLE batches (
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
