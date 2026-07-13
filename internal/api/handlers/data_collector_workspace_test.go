// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	if _, ok := body["organization_id"]; ok {
		t.Fatalf("response contains legacy organization_id: %#v", body)
	}
}

func TestListDataCollectorsFiltersByWorkspaceID(t *testing.T) {
	db := newDataCollectorWorkspaceTestDB(t)
	if _, err := db.Exec(`INSERT INTO workspaces (id, name, deleted_at) VALUES (456, 'Workspace B', NULL)`); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO data_collectors (organization_id, name, operator_id, status, metadata)
		VALUES (123, 'Alice', 'op-1', 'active', '{}'), (456, 'Bob', 'op-2', 'active', '{}')
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
}

func newDataCollectorWorkspaceTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, stmt := range []string{
		`CREATE TABLE workspaces (id INTEGER PRIMARY KEY, name TEXT NOT NULL, deleted_at TIMESTAMP NULL)`,
		`CREATE TABLE data_collectors (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			organization_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			operator_id TEXT NOT NULL,
			email TEXT,
			password_hash TEXT,
			certification TEXT,
			status TEXT NOT NULL,
			metadata TEXT,
			created_at TIMESTAMP,
			updated_at TIMESTAMP,
			deleted_at TIMESTAMP NULL
		)`,
		`INSERT INTO workspaces (id, name, deleted_at) VALUES (123, 'Workspace A', NULL)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("create collector schema: %v", err)
		}
	}
	return db
}
