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

func TestWorkspaceCreateListGetUpdateDelete(t *testing.T) {
	db := newTestWorkspaceDB(t)
	defer db.Close()
	router := newTestWorkspaceRouter(db)

	createResp := postWorkspace(t, router, CreateWorkspaceRequest{
		Name:    "  Workspace A  ",
		Admins:  []string{" admin-a ", "admin-b"},
		Members: []string{"member-a", " member-b "},
	}, http.StatusCreated)

	if createResp.ID == "" || createResp.Name != "Workspace A" {
		t.Fatalf("unexpected create response: %#v", createResp)
	}
	if strings.Join(createResp.Admins, ",") != "admin-a,admin-b" {
		t.Fatalf("admins=%#v", createResp.Admins)
	}
	if strings.Join(createResp.Members, ",") != "member-a,member-b" {
		t.Fatalf("members=%#v", createResp.Members)
	}

	var stored struct {
		AdminsStr  string `db:"admins_str"`
		MembersStr string `db:"members_str"`
	}
	if err := db.Get(&stored, "SELECT admins_str, members_str FROM workspaces WHERE id = ?", createResp.ID); err != nil {
		t.Fatalf("query stored workspace: %v", err)
	}
	if stored.AdminsStr != "#admin-a#admin-b#" || stored.MembersStr != "#member-a#member-b#" {
		t.Fatalf("stored people=%#v", stored)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces?limit=10&offset=0", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", w.Code, w.Body.String())
	}
	var listResp WorkspaceListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if listResp.Total != 1 || len(listResp.Items) != 1 || listResp.Items[0].ID != createResp.ID {
		t.Fatalf("unexpected list response: %#v", listResp)
	}

	updateBody := `{"name":"Workspace Renamed","admins":["admin-c"],"members":[]}`
	req = httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/"+createResp.ID, strings.NewReader(updateBody))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", w.Code, w.Body.String())
	}
	var updateResp WorkspaceResponse
	if err := json.Unmarshal(w.Body.Bytes(), &updateResp); err != nil {
		t.Fatalf("unmarshal update: %v", err)
	}
	if updateResp.Name != "Workspace Renamed" || strings.Join(updateResp.Admins, ",") != "admin-c" || len(updateResp.Members) != 0 {
		t.Fatalf("unexpected update response: %#v", updateResp)
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/v1/workspaces/"+createResp.ID, nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/"+createResp.ID, nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("get deleted status=%d want=%d body=%s", w.Code, http.StatusNotFound, w.Body.String())
	}
}

func TestWorkspaceValidation(t *testing.T) {
	db := newTestWorkspaceDB(t)
	defer db.Close()
	router := newTestWorkspaceRouter(db)

	tests := []struct {
		name       string
		body       string
		wantError  string
		wantStatus int
	}{
		{
			name:       "missing name",
			body:       `{"admins":["admin-a"]}`,
			wantStatus: http.StatusBadRequest,
			wantError:  "name is required",
		},
		{
			name:       "empty admins after trim",
			body:       `{"name":"Workspace A","admins":[" ",""]}`,
			wantStatus: http.StatusBadRequest,
			wantError:  "admins is required",
		},
		{
			name:       "duplicate admins",
			body:       `{"name":"Workspace A","admins":["admin-a","admin-a"]}`,
			wantStatus: http.StatusBadRequest,
			wantError:  "admins contains duplicate",
		},
		{
			name:       "duplicate members",
			body:       `{"name":"Workspace A","admins":["admin-a"],"members":["member-a","member-a"]}`,
			wantStatus: http.StatusBadRequest,
			wantError:  "members contains duplicate",
		},
		{
			name:       "overlap",
			body:       `{"name":"Workspace A","admins":["user-a"],"members":["user-a"]}`,
			wantStatus: http.StatusBadRequest,
			wantError:  "admins and members cannot overlap",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces", strings.NewReader(tt.body))
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

func TestWorkspaceNameUniqueAmongActiveRows(t *testing.T) {
	db := newTestWorkspaceDB(t)
	defer db.Close()
	router := newTestWorkspaceRouter(db)

	first := postWorkspace(t, router, CreateWorkspaceRequest{Name: "Workspace A", Admins: []string{"admin-a"}}, http.StatusCreated)
	postWorkspace(t, router, CreateWorkspaceRequest{Name: "Workspace A", Admins: []string{"admin-b"}}, http.StatusBadRequest)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/workspaces/"+first.ID, nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s", w.Code, w.Body.String())
	}

	postWorkspace(t, router, CreateWorkspaceRequest{Name: "Workspace A", Admins: []string{"admin-b"}}, http.StatusCreated)
}

func newTestWorkspaceRouter(db *sqlx.DB) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	NewWorkspaceHandler(db).RegisterRoutes(router.Group("/api/v1"))
	return router
}

func newTestWorkspaceDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE workspaces (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			admins_str TEXT NOT NULL,
			members_str TEXT,
			created_at TIMESTAMP,
			updated_at TIMESTAMP,
			deleted_at TIMESTAMP
		);
		CREATE UNIQUE INDEX idx_workspaces_name_active ON workspaces(name) WHERE deleted_at IS NULL;
	`); err != nil {
		db.Close()
		t.Fatalf("create workspaces table: %v", err)
	}
	return db
}

func postWorkspace(t *testing.T, router *gin.Engine, body CreateWorkspaceRequest, wantStatus int) WorkspaceResponse {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal workspace body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != wantStatus {
		t.Fatalf("status=%d want=%d body=%s", w.Code, wantStatus, w.Body.String())
	}
	if wantStatus != http.StatusCreated {
		return WorkspaceResponse{}
	}
	var resp WorkspaceResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal create response: %v body=%s", err, w.Body.String())
	}
	if resp.CreatedAt == "" {
		t.Fatalf("created_at is empty: %#v", resp)
	}
	if _, err := time.Parse(time.RFC3339, resp.CreatedAt); err != nil {
		t.Fatalf("created_at=%q is not RFC3339: %v", resp.CreatedAt, err)
	}
	return resp
}
