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

	"archebase.com/keystone-edge/internal/services"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

func TestRobotHandlerListRobots_DeviceIDSearchSemantics(t *testing.T) {
	db := newTestRobotHandlerDB(t)
	defer db.Close()

	now := time.Now().UTC()
	stmts := []struct {
		sql  string
		args []any
	}{
		{sql: `INSERT INTO robots (id, robot_type_id, device_id, factory_id, asset_id, status, created_at, updated_at) VALUES (1, 10, 'wt1_robot_060', 30, 'asset-060', 'active', ?, ?)`, args: []any{now, now}},
		{sql: `INSERT INTO robots (id, robot_type_id, device_id, factory_id, asset_id, status, created_at, updated_at) VALUES (2, 10, 'wt1_robot_120', 30, 'asset-120', 'active', ?, ?)`, args: []any{now, now}},
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed robot fixture failed: %v", err)
		}
	}

	r := newTestRobotRouter(t, db)

	tests := []struct {
		name          string
		path          string
		wantDeviceIDs []string
	}{
		{
			name:          "q partially matches device_id",
			path:          "/api/v1/robots?q=060",
			wantDeviceIDs: []string{"wt1_robot_060"},
		},
		{
			name:          "device_id requires exact match",
			path:          "/api/v1/robots?device_id=wt1_robot_060",
			wantDeviceIDs: []string{"wt1_robot_060"},
		},
		{
			name:          "partial device_id does not match",
			path:          "/api/v1/robots?device_id=060",
			wantDeviceIDs: nil,
		},
		{
			name:          "device_id exact filter is combined with keyword search",
			path:          "/api/v1/robots?q=060&search=060&device_id=060",
			wantDeviceIDs: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
			}

			var resp RobotListResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal response: %v body=%s", err, w.Body.String())
			}
			if resp.Total != len(tt.wantDeviceIDs) || len(resp.Items) != len(tt.wantDeviceIDs) {
				t.Fatalf("unexpected filtered response: %#v", resp)
			}
			for i, wantDeviceID := range tt.wantDeviceIDs {
				if resp.Items[i].DeviceID != wantDeviceID {
					t.Fatalf("item[%d].DeviceID=%q want=%q item=%#v", i, resp.Items[i].DeviceID, wantDeviceID, resp.Items[i])
				}
			}
		})
	}
}

func TestRobotHandlerListRobotsIncludesDisplayLabels(t *testing.T) {
	db := newTestRobotHandlerDB(t)
	defer db.Close()

	if _, err := db.Exec(`INSERT INTO robot_types (id, name, model, deleted_at) VALUES (51, 'Arm Type 51', 'Model-51', NULL)`); err != nil {
		t.Fatalf("seed robot type fixture failed: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO factories (id, name, slug, deleted_at) VALUES (81, 'Factory 81', 'fac-81', NULL)`); err != nil {
		t.Fatalf("seed factory fixture failed: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO robots (id, robot_type_id, device_id, factory_id, status) VALUES (1, 51, 'label_robot', 81, 'active')`); err != nil {
		t.Fatalf("seed robot fixture failed: %v", err)
	}

	r := newTestRobotRouter(t, db)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/robots", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp RobotListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v body=%s", err, w.Body.String())
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items=%#v, want one item", resp.Items)
	}
	got := resp.Items[0]
	if got.RobotTypeModel != "Model-51" || got.RobotTypeName != "Arm Type 51" {
		t.Fatalf("unexpected robot type labels: %#v", got)
	}
	if got.FactoryName != "Factory 81" || got.FactorySlug != "fac-81" {
		t.Fatalf("unexpected factory labels: %#v", got)
	}
}

func TestRobotHandlerListRobotsRejectsOversizedMultiValueFilter(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	NewRobotHandler(nil, nil, nil).RegisterRoutes(r.Group("/api/v1"))

	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/robots?device_id="+joinedStringList("robot", maxMultiValueFilterItems+1),
		nil,
	)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestRobotHandlerListRobots_ConnectedFilterUsesSQLLimit(t *testing.T) {
	db := newTestRobotHandlerDB(t)
	defer db.Close()

	now := time.Now().UTC()
	stmts := []struct {
		sql  string
		args []any
	}{
		{sql: `INSERT INTO robots (id, robot_type_id, device_id, factory_id, status, created_at, updated_at) VALUES (1, 10, 'older_robot', 30, 'active', ?, ?)`, args: []any{now, now}},
		{sql: `INSERT INTO robots (id, robot_type_id, device_id, factory_id, status, created_at, updated_at) VALUES (2, 10, 'page_external_robot', 30, 'active', 'not-a-time', 'not-a-time')`},
		{sql: `INSERT INTO robots (id, robot_type_id, device_id, factory_id, status, created_at, updated_at) VALUES (3, 10, 'newest_robot', 30, 'active', ?, ?)`, args: []any{now, now}},
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed robot fixture failed: %v", err)
		}
	}

	r := newTestRobotRouter(t, db)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/robots?connected=false&limit=1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp RobotListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v body=%s", err, w.Body.String())
	}
	if resp.Total != 3 || len(resp.Items) != 1 {
		t.Fatalf("unexpected paginated response: %#v", resp)
	}
	if resp.Items[0].DeviceID != "newest_robot" {
		t.Fatalf("item[0].DeviceID=%q want newest_robot", resp.Items[0].DeviceID)
	}
	if !resp.HasNext {
		t.Fatalf("HasNext=false want true response=%#v", resp)
	}
}

func TestRobotHandlerListRobots_ConnectedFilterUsesHubIntersection(t *testing.T) {
	db := newTestRobotHandlerDB(t)
	defer db.Close()

	now := time.Now().UTC()
	stmts := []struct {
		sql  string
		args []any
	}{
		{sql: `INSERT INTO robots (id, robot_type_id, device_id, factory_id, status, created_at, updated_at) VALUES (1, 10, 'recorder_only_robot', 30, 'active', ?, ?)`, args: []any{now, now}},
		{sql: `INSERT INTO robots (id, robot_type_id, device_id, factory_id, status, created_at, updated_at) VALUES (2, 10, 'connected_robot', 30, 'active', ?, ?)`, args: []any{now, now}},
		{sql: `INSERT INTO robots (id, robot_type_id, device_id, factory_id, status, created_at, updated_at) VALUES (3, 10, 'offline_robot', 30, 'active', ?, ?)`, args: []any{now, now}},
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed robot fixture failed: %v", err)
		}
	}

	recorderHub := services.NewRecorderHub()
	transferHub := services.NewTransferHub(10)
	connectRecorderForTest(t, recorderHub, "connected_robot", now)
	connectTransferForTest(t, transferHub, "connected_robot", now.Add(time.Second))
	connectRecorderForTest(t, recorderHub, "recorder_only_robot", now)

	r := newTestRobotRouterWithHubs(t, db, recorderHub, transferHub)

	t.Run("connected true returns only fully connected devices", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/robots?connected=true&limit=10", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
		}
		var resp RobotListResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal response: %v body=%s", err, w.Body.String())
		}
		if resp.Total != 1 || len(resp.Items) != 1 {
			t.Fatalf("unexpected connected response: %#v", resp)
		}
		if resp.Items[0].DeviceID != "connected_robot" || !resp.Items[0].Connected || resp.Items[0].ConnectedAt == "" {
			t.Fatalf("unexpected connected item: %#v", resp.Items[0])
		}
	})

	t.Run("connected false excludes fully connected devices", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/robots?connected=false&limit=10", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
		}
		var resp RobotListResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal response: %v body=%s", err, w.Body.String())
		}
		wantDeviceIDs := []string{"offline_robot", "recorder_only_robot"}
		if resp.Total != len(wantDeviceIDs) || len(resp.Items) != len(wantDeviceIDs) {
			t.Fatalf("unexpected disconnected response: %#v", resp)
		}
		for i, wantDeviceID := range wantDeviceIDs {
			if resp.Items[i].DeviceID != wantDeviceID || resp.Items[i].Connected {
				t.Fatalf("item[%d]=%#v want device_id=%q connected=false", i, resp.Items[i], wantDeviceID)
			}
		}
	})
}

func newTestRobotRouter(t *testing.T, db *sqlx.DB) *gin.Engine {
	t.Helper()
	return newTestRobotRouterWithHubs(t, db, nil, nil)
}

func newTestRobotRouterWithHubs(t *testing.T, db *sqlx.DB, recorderHub *services.RecorderHub, transferHub *services.TransferHub) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewRobotHandler(db, recorderHub, transferHub).RegisterRoutes(r.Group("/api/v1"))
	return r
}

func connectRecorderForTest(t *testing.T, hub *services.RecorderHub, deviceID string, connectedAt time.Time) {
	t.Helper()
	conn := hub.NewRecorderConn(nil, deviceID, "127.0.0.1")
	conn.ConnectedAt = connectedAt
	conn.LastSeenAt = connectedAt
	if !hub.Connect(deviceID, conn) {
		t.Fatalf("connect recorder %q failed", deviceID)
	}
}

func connectTransferForTest(t *testing.T, hub *services.TransferHub, deviceID string, connectedAt time.Time) {
	t.Helper()
	conn := hub.NewTransferConn(nil, deviceID, "127.0.0.1")
	conn.ConnectedAt = connectedAt
	conn.LastSeenAt = connectedAt
	if !hub.Connect(deviceID, conn) {
		t.Fatalf("connect transfer %q failed", deviceID)
	}
}

func newTestRobotHandlerDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}

	schema := `CREATE TABLE robots (
			id INTEGER PRIMARY KEY,
			robot_type_id INTEGER NOT NULL,
		device_id TEXT NOT NULL,
		factory_id INTEGER NOT NULL,
		asset_id TEXT,
		status TEXT NOT NULL,
		metadata TEXT,
		created_at TIMESTAMP,
			updated_at TIMESTAMP,
			deleted_at TIMESTAMP NULL
		)`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("create robot schema failed: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE robot_types (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			model TEXT NOT NULL,
			deleted_at TIMESTAMP NULL
		)`); err != nil {
		t.Fatalf("create robot type schema failed: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE factories (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			slug TEXT NOT NULL,
			deleted_at TIMESTAMP NULL
		)`); err != nil {
		t.Fatalf("create factory schema failed: %v", err)
	}

	return db
}
