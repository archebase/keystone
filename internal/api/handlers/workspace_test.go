// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"archebase.com/keystone-edge/internal/auth"
	"archebase.com/keystone-edge/internal/config"
	"archebase.com/keystone-edge/internal/services"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

func TestWorkspaceListEnsuresDefaultWorkspace(t *testing.T) {
	db := newTestWorkspaceDB(t)
	defer db.Close()
	router := newTestWorkspaceRouter(db)

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
	if listResp.Total != 1 || len(listResp.Items) != 1 {
		t.Fatalf("unexpected list response: %#v", listResp)
	}

	assertDefaultWorkspaceResponse(t, listResp.Items[0])

	var stored struct {
		ID            int64  `db:"id"`
		Source        string `db:"source"`
		UploadEnabled bool   `db:"upload_enabled"`
	}
	if err := db.Get(&stored, "SELECT id, source, upload_enabled FROM workspaces WHERE id = 0"); err != nil {
		t.Fatalf("query default workspace: %v", err)
	}
	if stored.ID != 0 || stored.Source != workspaceSourceDefault || stored.UploadEnabled {
		t.Fatalf("unexpected stored default workspace: %#v", stored)
	}
}

func TestWorkspaceGetDefaultWorkspace(t *testing.T) {
	db := newTestWorkspaceDB(t)
	defer db.Close()
	router := newTestWorkspaceRouter(db)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/0", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", w.Code, w.Body.String())
	}

	var resp WorkspaceResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal get: %v", err)
	}
	assertDefaultWorkspaceResponse(t, resp)
}

func TestWorkspaceListAllowsDefaultWorkspaceIDFilter(t *testing.T) {
	db := newTestWorkspaceDB(t)
	defer db.Close()
	router := newTestWorkspaceRouter(db)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces?workspace_id=0", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", w.Code, w.Body.String())
	}

	var listResp WorkspaceListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if listResp.Total != 1 || len(listResp.Items) != 1 || listResp.Items[0].ID != "0" {
		t.Fatalf("unexpected filtered list response: %#v", listResp)
	}
}

func TestWorkspaceWriteRoutesAreNotRegistered(t *testing.T) {
	db := newTestWorkspaceDB(t)
	defer db.Close()
	router := newTestWorkspaceRouter(db)

	tests := []struct {
		method string
		path   string
	}{
		{method: http.MethodPost, path: "/api/v1/workspaces"},
		{method: http.MethodPut, path: "/api/v1/workspaces/0"},
		{method: http.MethodDelete, path: "/api/v1/workspaces/0"},
	}

	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			if w.Code != http.StatusNotFound {
				t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusNotFound, w.Body.String())
			}
		})
	}
}

func TestWorkspaceSyncReturnsServiceUnavailableWhenNotConfigured(t *testing.T) {
	db := newTestWorkspaceDB(t)
	defer db.Close()
	router := newTestWorkspaceRouter(db)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/sync", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusServiceUnavailable, w.Body.String())
	}
}

func TestWorkspaceSyncReturnsResult(t *testing.T) {
	db := newTestWorkspaceDB(t)
	defer db.Close()

	client := &fakeWorkspaceHilbertClient{
		loginResult: &auth.HilbertLoginResult{SessionKey: "session-key"},
		workspaces:  []auth.HilbertWorkspace{{ID: 123, Name: "Hilbert Workspace"}},
	}
	syncService := services.NewWorkspaceSyncService(db, &config.HilbertConfig{
		BaseURL:                "http://hilbert",
		TimeoutSeconds:         2,
		ServiceAccountCode:     "svc-keystone",
		ServiceAccountPassword: "svc-secret",
	}, client)
	router := newTestWorkspaceRouter(db, syncService)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/sync", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp WorkspaceSyncResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal sync response: %v", err)
	}
	if resp.SyncedCount != 1 || !resp.DefaultIncluded || resp.LastSyncedAt == "" {
		t.Fatalf("unexpected sync response: %#v", resp)
	}
	if client.listSessionKey != "session-key" {
		t.Fatalf("listSessionKey=%q want session-key", client.listSessionKey)
	}
}

type fakeWorkspaceHilbertClient struct {
	loginResult    *auth.HilbertLoginResult
	workspaces     []auth.HilbertWorkspace
	listSessionKey string
}

func (f *fakeWorkspaceHilbertClient) Configured() bool {
	return true
}

func (f *fakeWorkspaceHilbertClient) Login(_ context.Context, _ string, _ string) (*auth.HilbertLoginResult, error) {
	return f.loginResult, nil
}

func (f *fakeWorkspaceHilbertClient) ListAvailableWorkspaces(_ context.Context, sessionKey string) ([]auth.HilbertWorkspace, error) {
	f.listSessionKey = sessionKey
	return f.workspaces, nil
}

func assertDefaultWorkspaceResponse(t *testing.T, workspace WorkspaceResponse) {
	t.Helper()
	if workspace.ID != "0" {
		t.Fatalf("id=%q want 0", workspace.ID)
	}
	if workspace.Name != defaultWorkspaceName {
		t.Fatalf("name=%q want %q", workspace.Name, defaultWorkspaceName)
	}
	if workspace.Source != workspaceSourceDefault {
		t.Fatalf("source=%q want %q", workspace.Source, workspaceSourceDefault)
	}
	if workspace.UploadEnabled {
		t.Fatalf("upload_enabled=true want false")
	}
	if workspace.LastSyncedAt != "" {
		t.Fatalf("last_synced_at=%q want empty", workspace.LastSyncedAt)
	}
	if len(workspace.Admins) != 0 || len(workspace.Members) != 0 {
		t.Fatalf("people=%#v/%#v want empty", workspace.Admins, workspace.Members)
	}
	if workspace.CreatedAt == "" {
		t.Fatalf("created_at is empty: %#v", workspace)
	}
	if _, err := time.Parse(time.RFC3339, workspace.CreatedAt); err != nil {
		t.Fatalf("created_at=%q is not RFC3339: %v", workspace.CreatedAt, err)
	}
}

func newTestWorkspaceRouter(db *sqlx.DB, syncService ...*services.WorkspaceSyncService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	NewWorkspaceHandler(db, syncService...).RegisterRoutes(router.Group("/api/v1"))
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
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			description TEXT,
			source TEXT NOT NULL,
			upload_enabled BOOLEAN NOT NULL,
			admins_str TEXT,
			members_str TEXT,
			last_synced_at TIMESTAMP,
			hilbert_created_at TIMESTAMP,
			hilbert_updated_at TIMESTAMP,
			created_at TIMESTAMP,
			updated_at TIMESTAMP,
			deleted_at TIMESTAMP
		);
	`); err != nil {
		db.Close()
		t.Fatalf("create workspaces table: %v", err)
	}
	return db
}
