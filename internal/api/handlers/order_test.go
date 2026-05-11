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

func TestOrderHandlerListOrdersFilters(t *testing.T) {
	db := newTestOrderHandlerDB(t)
	defer db.Close()
	seedOrderListFixtures(t, db)

	r := newTestOrderRouter(t, db)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/orders?order_name=厨房取物&organization_id=1&scene_id=10&priority=normal&status=created", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var body OrderListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if body.Total != 1 || len(body.Items) != 1 {
		t.Fatalf("got total=%d len=%d body=%s", body.Total, len(body.Items), w.Body.String())
	}
	got := body.Items[0]
	if got.ID != "1" || got.Name != "厨房取物" || got.OrganizationID != "1" || got.SceneID != "10" || got.Priority != "normal" || got.Status != "created" {
		t.Fatalf("unexpected order item: %+v", got)
	}
}

func TestOrderHandlerListOrdersRejectsInvalidStatus(t *testing.T) {
	db := newTestOrderHandlerDB(t)
	defer db.Close()

	r := newTestOrderRouter(t, db)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/orders?status=created,bad", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func newTestOrderRouter(t *testing.T, db *sqlx.DB) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()

	h := NewOrderHandler(db, nil, 0)
	v1 := r.Group("/api/v1")
	h.RegisterRoutes(v1)

	return r
}

func newTestOrderHandlerDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}

	schema := []string{
		`CREATE TABLE organizations (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE scenes (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE orders (
			id INTEGER PRIMARY KEY,
			scene_id INTEGER NOT NULL,
			organization_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			target_count INTEGER NOT NULL,
			status TEXT NOT NULL,
			priority TEXT NOT NULL,
			deadline TIMESTAMP NULL,
			metadata TEXT,
			created_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE tasks (
			id INTEGER PRIMARY KEY,
			order_id INTEGER NOT NULL,
			status TEXT NOT NULL,
			deleted_at TIMESTAMP NULL
		)`,
	}
	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("create schema failed: %v", err)
		}
	}

	return db
}

func seedOrderListFixtures(t *testing.T, db *sqlx.DB) {
	t.Helper()
	now := time.Now().UTC()
	stmts := []string{
		`INSERT INTO organizations (id, name) VALUES (1, '组织A')`,
		`INSERT INTO organizations (id, name) VALUES (2, '组织B')`,
		`INSERT INTO scenes (id, name) VALUES (10, '厨房')`,
		`INSERT INTO scenes (id, name) VALUES (11, '卧室')`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed order lookup fixtures failed: %v", err)
		}
	}

	rows := []struct {
		id             int
		sceneID        int
		organizationID int
		name           string
		priority       string
		status         string
	}{
		{id: 1, sceneID: 10, organizationID: 1, name: "厨房取物", priority: "normal", status: "created"},
		{id: 2, sceneID: 11, organizationID: 2, name: "卧室整理", priority: "high", status: "in_progress"},
		{id: 3, sceneID: 11, organizationID: 1, name: "厨房移动水瓶", priority: "urgent", status: "completed"},
	}
	for _, row := range rows {
		if _, err := db.Exec(
			`INSERT INTO orders (id, scene_id, organization_id, name, target_count, status, priority, metadata, created_at, updated_at)
			 VALUES (?, ?, ?, ?, 10, ?, ?, '{}', ?, ?)`,
			row.id, row.sceneID, row.organizationID, row.name, row.status, row.priority, now, now,
		); err != nil {
			t.Fatalf("seed orders failed: %v", err)
		}
	}
	if _, err := db.Exec(`INSERT INTO tasks (order_id, status) VALUES (1, 'completed')`); err != nil {
		t.Fatalf("seed order tasks failed: %v", err)
	}
}
