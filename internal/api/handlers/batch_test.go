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
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

func TestValidateTaskGroupUniqueness(t *testing.T) {
	tests := []struct {
		name       string
		taskGroups []TaskGroupItem
		wantDupA   int
		wantDupB   int
		wantOK     bool
	}{
		{
			name: "no duplicates returns false",
			taskGroups: []TaskGroupItem{
				{SOPID: 1, SubsceneID: 1, Quantity: 1},
				{SOPID: 1, SubsceneID: 2, Quantity: 1},
				{SOPID: 2, SubsceneID: 1, Quantity: 1},
			},
			wantOK: false,
		},
		{
			name: "duplicate sop and subscene returns first collision",
			taskGroups: []TaskGroupItem{
				{SOPID: 3, SubsceneID: 4, Quantity: 1},
				{SOPID: 5, SubsceneID: 6, Quantity: 1},
				{SOPID: 3, SubsceneID: 4, Quantity: 2},
			},
			wantDupA: 0,
			wantDupB: 2,
			wantOK:   true,
		},
		{
			name: "invalid ids are skipped from uniqueness check",
			taskGroups: []TaskGroupItem{
				{SOPID: 0, SubsceneID: 1, Quantity: 1},
				{SOPID: 1, SubsceneID: 0, Quantity: 1},
				{SOPID: -1, SubsceneID: 2, Quantity: 1},
				{SOPID: 8, SubsceneID: 9, Quantity: 1},
				{SOPID: 8, SubsceneID: 9, Quantity: 2},
			},
			wantDupA: 3,
			wantDupB: 4,
			wantOK:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotDupA, gotDupB, gotOK := validateTaskGroupUniqueness(tt.taskGroups)
			if gotDupA != tt.wantDupA || gotDupB != tt.wantDupB || gotOK != tt.wantOK {
				t.Fatalf("validateTaskGroupUniqueness() = (%d, %d, %v), want (%d, %d, %v)", gotDupA, gotDupB, gotOK, tt.wantDupA, tt.wantDupB, tt.wantOK)
			}
		})
	}
}

func TestParseNullableJSON(t *testing.T) {
	tests := []struct {
		name string
		in   sql.NullString
		want any
	}{
		{
			name: "invalid null string returns nil",
			in:   sql.NullString{Valid: false},
			want: nil,
		},
		{
			name: "empty string returns nil",
			in:   sql.NullString{Valid: true, String: ""},
			want: nil,
		},
		{
			name: "json null returns nil",
			in:   sql.NullString{Valid: true, String: "null"},
			want: nil,
		},
		{
			name: "invalid json returns nil",
			in:   sql.NullString{Valid: true, String: "{"},
			want: nil,
		},
		{
			name: "valid object json returns object",
			in:   sql.NullString{Valid: true, String: `{"k":"v","n":2}`},
			want: map[string]any{"k": "v", "n": float64(2)},
		},
		{
			name: "valid array json returns array",
			in:   sql.NullString{Valid: true, String: `[1,2,3]`},
			want: []any{float64(1), float64(2), float64(3)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseNullableJSON(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseNullableJSON() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestBatchListItemFromRow(t *testing.T) {
	startedAt := time.Date(2026, 4, 20, 16, 30, 0, 0, time.FixedZone("UTC+8", 8*3600))
	endedAt := startedAt.Add(2 * time.Hour)
	createdAt := startedAt.Add(-10 * time.Minute)
	updatedAt := startedAt.Add(5 * time.Minute)

	row := batchRow{
		ID:             100,
		BatchID:        "BATCH-100",
		OrderID:        10,
		WorkstationID:  20,
		Name:           sql.NullString{Valid: true, String: "batch-name"},
		Notes:          sql.NullString{Valid: true, String: "notes"},
		Status:         "active",
		CompletedCount: 3,
		TaskCount:      8,
		CancelledCount: 1,
		FailedCount:    2,
		EpisodeCount:   6,
		StartedAt:      sql.NullTime{Valid: true, Time: startedAt},
		EndedAt:        sql.NullTime{Valid: true, Time: endedAt},
		Metadata:       sql.NullString{Valid: true, String: `{"source":"manual"}`},
		CreatedAt:      sql.NullTime{Valid: true, Time: createdAt},
		UpdatedAt:      sql.NullTime{Valid: true, Time: updatedAt},
	}

	got := batchListItemFromRow(row)

	if got.ID != "100" || got.BatchID != "BATCH-100" || got.OrderID != "10" || got.WorkstationID != "20" {
		t.Fatalf("unexpected id mapping: %#v", got)
	}
	if got.Name != "batch-name" || got.Notes != "notes" || got.Status != "active" {
		t.Fatalf("unexpected text fields: %#v", got)
	}
	if got.CompletedCount != 3 || got.TaskCount != 8 || got.CancelledCount != 1 || got.FailedCount != 2 || got.EpisodeCount != 6 {
		t.Fatalf("unexpected counter fields: %#v", got)
	}

	wantStartedAt := startedAt.UTC().Format(time.RFC3339)
	wantEndedAt := endedAt.UTC().Format(time.RFC3339)
	wantCreatedAt := createdAt.UTC().Format(time.RFC3339)
	wantUpdatedAt := updatedAt.UTC().Format(time.RFC3339)
	if got.StartedAt != wantStartedAt || got.EndedAt != wantEndedAt || got.CreatedAt != wantCreatedAt || got.UpdatedAt != wantUpdatedAt {
		t.Fatalf("unexpected time mapping: got=(%q,%q,%q,%q) want=(%q,%q,%q,%q)", got.StartedAt, got.EndedAt, got.CreatedAt, got.UpdatedAt, wantStartedAt, wantEndedAt, wantCreatedAt, wantUpdatedAt)
	}

	meta, ok := got.Metadata.(map[string]any)
	if !ok {
		t.Fatalf("metadata type = %T, want map[string]any", got.Metadata)
	}
	if meta["source"] != "manual" {
		t.Fatalf("unexpected metadata value: %#v", got.Metadata)
	}
}

func TestBatchListItemFromRow_HandlesNullFields(t *testing.T) {
	row := batchRow{
		ID:            1,
		BatchID:       "BATCH-1",
		OrderID:       2,
		WorkstationID: 3,
		Status:        "pending",
	}

	got := batchListItemFromRow(row)

	if got.Name != "" || got.Notes != "" || got.StartedAt != "" || got.EndedAt != "" || got.CreatedAt != "" || got.UpdatedAt != "" {
		t.Fatalf("expected empty optional fields, got %#v", got)
	}
	if got.Metadata != nil {
		t.Fatalf("metadata = %#v, want nil", got.Metadata)
	}
}

func TestBatchHandlerListBatches_InvalidLimit(t *testing.T) {
	db := newTestBatchHandlerDB(t)
	defer db.Close()

	r := newTestBatchRouter(t, db)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/batches?limit=0", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "limit must be a positive integer") {
		t.Fatalf("unexpected error response: %s", w.Body.String())
	}
}

func TestBatchHandlerListBatches_DefaultPaginationAndFilter(t *testing.T) {
	db := newTestBatchHandlerDB(t)
	defer db.Close()
	seedBatchListFixtures(t, db)

	r := newTestBatchRouter(t, db)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/batches?status=active", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp ListBatchesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v body=%s", err, w.Body.String())
	}
	if resp.Total != 1 || resp.Limit != 50 || resp.Offset != 0 || resp.HasNext || resp.HasPrev {
		t.Fatalf("unexpected pagination response: %#v", resp)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items length=%d want=1", len(resp.Items))
	}
	if resp.Items[0].Status != "active" || resp.Items[0].OrderID != "10" {
		t.Fatalf("unexpected item: %#v", resp.Items[0])
	}
}

func TestBatchHandlerCreateBatch_DuplicateTaskGroups(t *testing.T) {
	db := newTestBatchHandlerDB(t)
	defer db.Close()
	seedBatchCreateFixtures(t, db)

	r := newTestBatchRouter(t, db)
	payload := `{
		"order_id": 10,
		"workstation_id": 20,
		"task_groups": [
			{"sop_id": 40, "subscene_id": 50, "quantity": 1},
			{"sop_id": 40, "subscene_id": 50, "quantity": 2}
		]
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/batches", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "duplicate task_groups entries") {
		t.Fatalf("unexpected error response: %s", w.Body.String())
	}
}

func TestBatchHandlerListBatches_PaginationFlags(t *testing.T) {
	db := newTestBatchHandlerDB(t)
	defer db.Close()
	seedBatchListFixtures(t, db)
	seedBatchListFixturesForPagination(t, db)

	r := newTestBatchRouter(t, db)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/batches?limit=2&offset=1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp ListBatchesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v body=%s", err, w.Body.String())
	}

	if resp.Total != 4 || resp.Limit != 2 || resp.Offset != 1 {
		t.Fatalf("unexpected paging fields: %#v", resp)
	}
	if !resp.HasNext || !resp.HasPrev {
		t.Fatalf("expected hasNext and hasPrev true, got hasNext=%v hasPrev=%v", resp.HasNext, resp.HasPrev)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("items length=%d want=2", len(resp.Items))
	}
}

func TestBatchHandlerCreateBatch_InvalidQuantity(t *testing.T) {
	db := newTestBatchHandlerDB(t)
	defer db.Close()
	seedBatchCreateFixtures(t, db)

	r := newTestBatchRouter(t, db)
	payload := `{
		"order_id": 10,
		"workstation_id": 20,
		"task_groups": [
			{"sop_id": 40, "subscene_id": 50, "quantity": 0}
		]
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/batches", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	var errResp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("unmarshal error response: %v body=%s", err, w.Body.String())
	}
	if !strings.Contains(errResp["error"], "task_groups[0].quantity must be") {
		t.Fatalf("unexpected error response: %s", w.Body.String())
	}
}

func TestBatchHandlerCreateBatch_MissingOrderID(t *testing.T) {
	db := newTestBatchHandlerDB(t)
	defer db.Close()
	seedBatchCreateFixtures(t, db)

	r := newTestBatchRouter(t, db)
	payload := `{
		"workstation_id": 20,
		"task_groups": [
			{"sop_id": 40, "subscene_id": 50, "quantity": 1}
		]
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/batches", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "order_id is required") {
		t.Fatalf("unexpected error response: %s", w.Body.String())
	}
}

func TestBatchHandlerListBatches_InvalidStatus(t *testing.T) {
	db := newTestBatchHandlerDB(t)
	defer db.Close()

	r := newTestBatchRouter(t, db)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/batches?status=bad-status", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid status") {
		t.Fatalf("unexpected error response: %s", w.Body.String())
	}
}

func TestBatchHandlerAdjustBatchTasks_InvalidBatchID(t *testing.T) {
	db := newTestBatchHandlerDB(t)
	defer db.Close()

	r := newTestBatchRouter(t, db)
	payload := `{"task_groups":[{"sop_id":1,"subscene_id":1,"quantity":1}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/batches/abc/tasks", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid batch id") {
		t.Fatalf("unexpected error response: %s", w.Body.String())
	}
}

func TestBatchHandlerAdjustBatchTasks_EmptyTaskGroups(t *testing.T) {
	db := newTestBatchHandlerDB(t)
	defer db.Close()

	r := newTestBatchRouter(t, db)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/batches/1/tasks", bytes.NewBufferString(`{"task_groups":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "task_groups must not be empty") {
		t.Fatalf("unexpected error response: %s", w.Body.String())
	}
}

func TestBatchHandlerAdjustBatchTasks_DuplicateTaskGroups(t *testing.T) {
	db := newTestBatchHandlerDB(t)
	defer db.Close()

	r := newTestBatchRouter(t, db)
	payload := `{
		"task_groups": [
			{"sop_id": 1, "subscene_id": 2, "quantity": 1},
			{"sop_id": 1, "subscene_id": 2, "quantity": 3}
		]
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/batches/1/tasks", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "duplicate task_groups entries") {
		t.Fatalf("unexpected error response: %s", w.Body.String())
	}
}

func newTestBatchRouter(t *testing.T, db *sqlx.DB) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()

	h := NewBatchHandler(db, nil, 0)
	v1 := r.Group("/api/v1")
	h.RegisterRoutes(v1)

	return r
}

func newTestBatchHandlerDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}

	schema := []string{
		`CREATE TABLE batches (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			batch_id TEXT NOT NULL,
			order_id INTEGER NOT NULL,
			workstation_id INTEGER NOT NULL,
			organization_id INTEGER NOT NULL DEFAULT 0,
			name TEXT,
			notes TEXT,
			status TEXT NOT NULL,
			episode_count INTEGER NOT NULL DEFAULT 0,
			metadata TEXT,
			started_at TIMESTAMP NULL,
			ended_at TIMESTAMP NULL,
			created_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE tasks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			task_id TEXT NOT NULL,
			batch_id INTEGER NOT NULL,
			order_id INTEGER NOT NULL,
			sop_id INTEGER NOT NULL,
			workstation_id INTEGER NOT NULL,
			scene_id INTEGER,
			subscene_id INTEGER,
			batch_name TEXT,
			scene_name TEXT,
			subscene_name TEXT,
			factory_id INTEGER,
			organization_id INTEGER,
			initial_scene_layout TEXT,
			status TEXT NOT NULL,
			assigned_at TIMESTAMP,
			created_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE orders (
			id INTEGER PRIMARY KEY,
			target_count INTEGER NOT NULL,
			organization_id INTEGER NOT NULL DEFAULT 0,
			scene_id INTEGER,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE organizations (
			id INTEGER PRIMARY KEY,
			factory_id INTEGER NOT NULL DEFAULT 0,
			name TEXT NOT NULL,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE workstations (
			id INTEGER PRIMARY KEY,
			factory_id INTEGER NOT NULL,
			organization_id INTEGER NOT NULL DEFAULT 0,
			status TEXT,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE factories (
			id INTEGER PRIMARY KEY,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE sops (
			id INTEGER PRIMARY KEY,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE scenes (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE subscenes (
			id INTEGER PRIMARY KEY,
			scene_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			initial_scene_layout TEXT,
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

func seedBatchListFixtures(t *testing.T, db *sqlx.DB) {
	t.Helper()
	now := time.Now().UTC()
	// Insert a shared organization so the LEFT JOIN in ListBatches resolves correctly.
	if _, err := db.Exec(`INSERT OR IGNORE INTO organizations (id, factory_id, name) VALUES (60, 30, 'Test Org')`); err != nil {
		t.Fatalf("seed organization failed: %v", err)
	}
	stmts := []string{
		`INSERT INTO batches (id, batch_id, order_id, workstation_id, organization_id, name, status, episode_count, created_at, updated_at) VALUES (1, 'B1', 10, 20, 60, 'A', 'active', 0, ?, ?)`,
		`INSERT INTO batches (id, batch_id, order_id, workstation_id, organization_id, name, status, episode_count, created_at, updated_at) VALUES (2, 'B2', 11, 21, 60, 'B', 'pending', 0, ?, ?)`,
		`INSERT INTO tasks (task_id, batch_id, order_id, sop_id, workstation_id, status, created_at, updated_at) VALUES ('T1', 1, 10, 40, 20, 'completed', ?, ?)`,
		`INSERT INTO tasks (task_id, batch_id, order_id, sop_id, workstation_id, status, created_at, updated_at) VALUES ('T2', 1, 10, 41, 20, 'failed', ?, ?)`,
	}
	for i, stmt := range stmts {
		if i < 2 {
			if _, err := db.Exec(stmt, now, now); err != nil {
				t.Fatalf("seed batches failed: %v", err)
			}
			continue
		}
		if _, err := db.Exec(stmt, now, now); err != nil {
			t.Fatalf("seed tasks failed: %v", err)
		}
	}
}

func seedBatchListFixturesForPagination(t *testing.T, db *sqlx.DB) {
	t.Helper()
	now := time.Now().UTC()
	stmts := []string{
		`INSERT INTO batches (id, batch_id, order_id, workstation_id, organization_id, name, status, episode_count, created_at, updated_at) VALUES (3, 'B3', 12, 22, 60, 'C', 'pending', 0, ?, ?)`,
		`INSERT INTO batches (id, batch_id, order_id, workstation_id, organization_id, name, status, episode_count, created_at, updated_at) VALUES (4, 'B4', 13, 23, 60, 'D', 'pending', 0, ?, ?)`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt, now, now); err != nil {
			t.Fatalf("seed pagination fixtures failed: %v", err)
		}
	}
}

func seedBatchCreateFixtures(t *testing.T, db *sqlx.DB) {
	t.Helper()
	stmts := []string{
		`INSERT INTO organizations (id, factory_id, name) VALUES (60, 30, 'Test Org')`,
		`INSERT INTO orders (id, target_count, organization_id) VALUES (10, 10, 60)`,
		`INSERT INTO factories (id) VALUES (30)`,
		`INSERT INTO workstations (id, factory_id, organization_id, status) VALUES (20, 30, 60, 'idle')`,
		`INSERT INTO sops (id) VALUES (40)`,
		`INSERT INTO scenes (id, name) VALUES (70, 'scene-a')`,
		`INSERT INTO subscenes (id, scene_id, name, initial_scene_layout) VALUES (50, 70, 'sub-a', '{}')`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed create fixtures failed: %v", err)
		}
	}
}
