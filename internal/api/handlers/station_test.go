// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
			name TEXT NOT NULL,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE robots (
			id INTEGER PRIMARY KEY,
			robot_type_id INTEGER NOT NULL,
			device_id TEXT NOT NULL,
			factory_id INTEGER NOT NULL,
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
			metadata TEXT,
			created_at TIMESTAMP,
			updated_at TIMESTAMP,
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
