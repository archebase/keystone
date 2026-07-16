// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
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

func TestCreateDataCollectorUsesWorkspaceContract(t *testing.T) {
	db := newDataCollectorWorkspaceTestDB(t)
	router := gin.New()
	NewDataCollectorHandler(db).RegisterRoutes(router.Group("/api/v1"))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/data_collectors", strings.NewReader(`{
		"workspace_id":"123",
		"name":"Alice",
		"operator_id":"op-1"
	}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if body["workspace_id"] != "123" || body["workspace_name"] != "Workspace A" {
		t.Fatalf("workspace fields=%#v", body)
	}
	workspaceIDs, ok := body["workspace_ids"].([]any)
	if !ok || len(workspaceIDs) != 1 || workspaceIDs[0] != "123" {
		t.Fatalf("workspace_ids=%#v", body["workspace_ids"])
	}
	if _, ok := body["organization_id"]; ok {
		t.Fatalf("response contains legacy organization_id: %#v", body)
	}
	var members string
	if err := db.Get(&members, `SELECT members FROM workspaces WHERE id = 123`); err != nil {
		t.Fatalf("query workspace members: %v", err)
	}
	if members != `["op-1"]` {
		t.Fatalf("members=%s", members)
	}
}

func TestListDataCollectorsFiltersByWorkspaceID(t *testing.T) {
	db := newDataCollectorWorkspaceTestDB(t)
	if _, err := db.Exec(`
		INSERT INTO workspaces (id, name, source, admins, members, deleted_at)
		VALUES (456, 'Workspace B', 'hilbert', '[]', '["op-2"]', NULL)
	`); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO data_collectors (name, operator_id, status, metadata)
		VALUES ('Alice', 'op-1', 'active', '{}'), ('Bob', 'op-2', 'active', '{}')
	`); err != nil {
		t.Fatalf("seed collectors: %v", err)
	}

	router := gin.New()
	NewDataCollectorHandler(db).RegisterRoutes(router.Group("/api/v1"))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/data_collectors?workspace_id=456", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body DataCollectorListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if body.Total != 1 || len(body.Items) != 1 || body.Items[0].WorkspaceID != "456" {
		t.Fatalf("response=%#v", body)
	}
	if body.Items[0].OperatorID != "op-2" {
		t.Fatalf("collector=%#v", body.Items[0])
	}
}

func TestCreateDataCollectorRejectsHilbertWorkspace(t *testing.T) {
	db := newDataCollectorWorkspaceTestDB(t)
	if _, err := db.Exec(`UPDATE workspaces SET source = 'hilbert' WHERE id = 123`); err != nil {
		t.Fatalf("update workspace source: %v", err)
	}
	router := gin.New()
	NewDataCollectorHandler(db).RegisterRoutes(router.Group("/api/v1"))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/data_collectors", strings.NewReader(`{
		"workspace_id":"123",
		"name":"Alice",
		"operator_id":"op-1"
	}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestGlobalDataCollectorCanBelongToMultipleWorkspaces(t *testing.T) {
	db := newDataCollectorWorkspaceTestDB(t)
	if _, err := db.Exec(`
		UPDATE workspaces SET members = '["op-1"]' WHERE id = 123;
		INSERT INTO workspaces (id, name, source, admins, members, deleted_at)
		VALUES (456, 'Workspace B', 'hilbert', '[]', '["op-1"]', NULL);
		INSERT INTO data_collectors (name, operator_id, status, metadata)
		VALUES ('Alice', 'op-1', 'active', '{}')
	`); err != nil {
		t.Fatalf("seed shared collector: %v", err)
	}
	router := gin.New()
	NewDataCollectorHandler(db).RegisterRoutes(router.Group("/api/v1"))

	var collectorIDs []string
	for _, workspaceID := range []string{"123", "456"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/data_collectors?workspace_id="+workspaceID, nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("workspace=%s status=%d body=%s", workspaceID, rec.Code, rec.Body.String())
		}
		var body DataCollectorListResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal workspace %s response: %v", workspaceID, err)
		}
		if body.Total != 1 || len(body.Items) != 1 || body.Items[0].WorkspaceID != workspaceID {
			t.Fatalf("workspace=%s response=%#v", workspaceID, body)
		}
		collectorIDs = append(collectorIDs, body.Items[0].ID)
	}
	if collectorIDs[0] != collectorIDs[1] {
		t.Fatalf("collector ids differ: %v", collectorIDs)
	}
}

func TestDeleteDataCollectorRemovesDefaultWorkspaceMembershipOnly(t *testing.T) {
	db := newDataCollectorWorkspaceTestDB(t)
	result, err := db.Exec(`INSERT INTO data_collectors (name, operator_id, status, metadata) VALUES ('Alice', 'op-1', 'active', '{}')`)
	if err != nil {
		t.Fatalf("seed collector: %v", err)
	}
	collectorID, _ := result.LastInsertId()
	if _, err := db.Exec(`UPDATE workspaces SET members = '["op-1"]' WHERE id = 123`); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	router := gin.New()
	NewDataCollectorHandler(db).RegisterRoutes(router.Group("/api/v1"))
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/data_collectors/"+strconv.FormatInt(collectorID, 10)+"?workspace_id=123", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var members string
	if err := db.Get(&members, `SELECT members FROM workspaces WHERE id = 123`); err != nil {
		t.Fatalf("query members: %v", err)
	}
	if members != "[]" {
		t.Fatalf("members=%s", members)
	}
	var active int
	if err := db.Get(&active, `SELECT COUNT(*) FROM data_collectors WHERE id = ? AND deleted_at IS NULL`, collectorID); err != nil {
		t.Fatalf("query collector: %v", err)
	}
	if active != 1 {
		t.Fatalf("global collector was deleted")
	}
}

func TestDeleteDataCollectorRejectsHilbertWorkspaceMembership(t *testing.T) {
	db := newDataCollectorWorkspaceTestDB(t)
	result, err := db.Exec(`INSERT INTO data_collectors (name, operator_id, status, metadata) VALUES ('Alice', 'op-1', 'active', '{}')`)
	if err != nil {
		t.Fatalf("seed collector: %v", err)
	}
	collectorID, _ := result.LastInsertId()
	if _, err := db.Exec(`UPDATE workspaces SET source = 'hilbert', members = '["op-1"]' WHERE id = 123`); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	router := gin.New()
	NewDataCollectorHandler(db).RegisterRoutes(router.Group("/api/v1"))
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/data_collectors/"+strconv.FormatInt(collectorID, 10)+"?workspace_id=123", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func newDataCollectorWorkspaceTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, stmt := range []string{
		`CREATE TABLE workspaces (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			source TEXT NOT NULL,
			admins TEXT NOT NULL,
			members TEXT NOT NULL,
			updated_at TIMESTAMP,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE data_collectors (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			operator_id TEXT NOT NULL UNIQUE,
			email TEXT,
			password_hash TEXT,
			certification TEXT,
			status TEXT NOT NULL,
			metadata TEXT,
			created_at TIMESTAMP,
			updated_at TIMESTAMP,
			deleted_at TIMESTAMP NULL
		)`,
		`INSERT INTO workspaces (id, name, source, admins, members, deleted_at)
		 VALUES (123, 'Workspace A', 'default', '[]', '[]', NULL)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("create collector schema: %v", err)
		}
	}
	return db
}
