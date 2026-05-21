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
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

func TestRobotTypeConfigTemplatePublicGetSuccess(t *testing.T) {
	db := newTestRobotTypeConfigTemplateDB(t)
	defer db.Close()
	seedRobotTypeConfigTemplateFixtures(t, db)

	now := time.Now().UTC()
	if _, err := db.Exec(`
		INSERT INTO robot_type_config_templates (
			robot_type_id,
			filename,
			content,
			created_at,
			updated_at
		) VALUES (?, ?, ?, ?, ?)
	`, 12, "recorder.yaml", "record:\n  enabled: true\n", now, now); err != nil {
		t.Fatalf("seed template fixture: %v", err)
	}

	router := newTestRobotTypeConfigTemplateRouter(t, db)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/robot_types/12/configs/recorder.yaml", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); !strings.Contains(got, "text/yaml") {
		t.Fatalf("content-type=%q want text/yaml", got)
	}
	if got := w.Body.String(); got != "record:\n  enabled: true\n" {
		t.Fatalf("body=%q", got)
	}
}

func TestRobotTypeConfigTemplatePathErrors(t *testing.T) {
	db := newTestRobotTypeConfigTemplateDB(t)
	defer db.Close()
	seedRobotTypeConfigTemplateFixtures(t, db)

	router := newTestRobotTypeConfigTemplateRouter(t, db)
	tests := []struct {
		name       string
		path       string
		wantStatus int
		wantError  string
	}{
		{
			name:       "invalid robot type id",
			path:       "/api/v1/robot_types/0/configs/recorder.yaml",
			wantStatus: http.StatusBadRequest,
			wantError:  "invalid robot_type_id",
		},
		{
			name:       "invalid filename",
			path:       "/api/v1/robot_types/12/configs/other.yaml",
			wantStatus: http.StatusBadRequest,
			wantError:  "invalid config filename",
		},
		{
			name:       "missing robot type",
			path:       "/api/v1/robot_types/99/configs/recorder.yaml",
			wantStatus: http.StatusNotFound,
			wantError:  "robot_type not found",
		},
		{
			name:       "missing template",
			path:       "/api/v1/robot_types/12/configs/transfer.yaml",
			wantStatus: http.StatusNotFound,
			wantError:  "config template not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tt.wantStatus, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tt.wantError) {
				t.Fatalf("body=%q want error %q", w.Body.String(), tt.wantError)
			}
		})
	}
}

func TestRobotTypeConfigTemplateListReturnsFixedSlots(t *testing.T) {
	db := newTestRobotTypeConfigTemplateDB(t)
	defer db.Close()
	seedRobotTypeConfigTemplateFixtures(t, db)

	now := time.Now().UTC()
	if _, err := db.Exec(`
		INSERT INTO robot_type_config_templates (
			robot_type_id,
			filename,
			content,
			created_at,
			updated_at
		) VALUES (?, ?, ?, ?, ?)
	`, 12, "recorder.yaml", "record: true\n", now, now); err != nil {
		t.Fatalf("seed template fixture: %v", err)
	}

	router := newTestRobotTypeConfigTemplateRouter(t, db)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/robot_types/12/config_templates", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp RobotTypeConfigTemplateListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v body=%s", err, w.Body.String())
	}
	if len(resp.Templates) != 2 {
		t.Fatalf("templates=%#v want two fixed slots", resp.Templates)
	}
	if resp.Templates[0].Filename != "recorder.yaml" || !resp.Templates[0].Exists || resp.Templates[0].UpdatedAt == nil {
		t.Fatalf("unexpected recorder slot: %#v", resp.Templates[0])
	}
	if resp.Templates[1].Filename != "transfer.yaml" || resp.Templates[1].Exists || resp.Templates[1].UpdatedAt != nil {
		t.Fatalf("unexpected transfer slot: %#v", resp.Templates[1])
	}
}

func TestRobotTypeConfigTemplateUpsertValidation(t *testing.T) {
	db := newTestRobotTypeConfigTemplateDB(t)
	defer db.Close()
	seedRobotTypeConfigTemplateFixtures(t, db)

	router := newTestRobotTypeConfigTemplateRouter(t, db)
	tests := []struct {
		name       string
		body       []byte
		wantStatus int
		wantError  string
	}{
		{
			name:       "empty content",
			body:       []byte(`{"content":"   "}`),
			wantStatus: http.StatusBadRequest,
			wantError:  "content is required",
		},
		{
			name:       "too large",
			body:       mustJSON(t, UpsertRobotTypeConfigTemplateRequest{Content: strings.Repeat("a", maxRobotTypeConfigTemplateContentBytes+1)}),
			wantStatus: http.StatusBadRequest,
			wantError:  "content too large",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, "/api/v1/robot_types/12/config_templates/recorder.yaml", bytes.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tt.wantStatus, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tt.wantError) {
				t.Fatalf("body=%q want error %q", w.Body.String(), tt.wantError)
			}
		})
	}
}

func TestRobotTypeConfigTemplateUpsertAndDelete(t *testing.T) {
	db := newTestRobotTypeConfigTemplateDB(t)
	defer db.Close()
	seedRobotTypeConfigTemplateFixtures(t, db)

	router := newTestRobotTypeConfigTemplateRouter(t, db)
	putTemplate(t, router, "recorder: first\n")
	putTemplate(t, router, "recorder: second\n")

	var activeCount int
	if err := db.Get(&activeCount, `
		SELECT COUNT(*)
		FROM robot_type_config_templates
		WHERE robot_type_id = 12 AND filename = 'recorder.yaml' AND deleted_at IS NULL
	`); err != nil {
		t.Fatalf("count active templates: %v", err)
	}
	if activeCount != 1 {
		t.Fatalf("activeCount=%d want=1", activeCount)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/robot_types/12/configs/recorder.yaml", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Body.String() != "recorder: second\n" {
		t.Fatalf("public get status=%d body=%q", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/v1/robot_types/12/config_templates/recorder.yaml", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d want=%d body=%s", w.Code, http.StatusNoContent, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/robot_types/12/configs/recorder.yaml", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("public get after delete status=%d want=%d body=%s", w.Code, http.StatusNotFound, w.Body.String())
	}
}

func TestRobotTypeConfigTemplateRoutesDoNotConflictWithRobotTypeRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	v1 := router.Group("/api/v1")
	handler := NewRobotTypeHandler(nil)

	handler.RegisterRoutes(v1)
	handler.RegisterConfigTemplatePublicRoutes(v1)
	handler.RegisterConfigTemplateAdminRoutes(v1)
}

func putTemplate(t *testing.T, router *gin.Engine, content string) {
	t.Helper()
	body := mustJSON(t, UpsertRobotTypeConfigTemplateRequest{Content: content})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/robot_types/12/config_templates/recorder.yaml", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("put status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp RobotTypeConfigTemplateStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal put response: %v body=%s", err, w.Body.String())
	}
	if resp.Filename != "recorder.yaml" || !resp.Exists || resp.UpdatedAt == "" {
		t.Fatalf("unexpected put response: %#v", resp)
	}
}

func newTestRobotTypeConfigTemplateRouter(t *testing.T, db *sqlx.DB) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()

	handler := NewRobotTypeHandler(db)
	v1 := router.Group("/api/v1")
	handler.RegisterConfigTemplatePublicRoutes(v1)
	handler.RegisterConfigTemplateAdminRoutes(v1)

	return router
}

func newTestRobotTypeConfigTemplateDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}

	schema := []string{
		`CREATE TABLE robot_types (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			model TEXT NOT NULL,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE robot_type_config_templates (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			robot_type_id INTEGER NOT NULL,
			filename TEXT NOT NULL,
			content TEXT NOT NULL,
			created_at TIMESTAMP,
			updated_at TIMESTAMP,
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

func seedRobotTypeConfigTemplateFixtures(t *testing.T, db *sqlx.DB) {
	t.Helper()
	stmts := []string{
		`INSERT INTO robot_types (id, name, model) VALUES (12, '搬运机器人', 'mover-v1')`,
		`INSERT INTO robot_types (id, name, model, deleted_at) VALUES (13, '已删除类型', 'deleted-v1', '2026-01-01T00:00:00Z')`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed fixture failed: %v", err)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal json: %v", err)
	}
	return data
}
