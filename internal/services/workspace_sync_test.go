// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"archebase.com/keystone-edge/internal/auth"
	"archebase.com/keystone-edge/internal/config"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

func TestWorkspaceSyncServiceSyncUpsertsHilbertWorkspaces(t *testing.T) {
	db := newTestWorkspaceSyncDB(t)
	defer db.Close()

	createdAt := time.Date(2026, 7, 8, 1, 2, 3, 0, time.UTC)
	updatedAt := createdAt.Add(time.Hour)
	description := " synced from Hilbert "
	client := &fakeHilbertWorkspaceClient{
		loginResult: auth.NewHilbertLoginResult(auth.HilbertAccount{}, "session-key"),
		workspaces: []auth.HilbertWorkspace{
			{
				ID:          123,
				Name:        "  Customer Workspace  ",
				Description: &description,
				Admins:      []string{" admin-a ", "admin-a", ""},
				Members:     []string{"member-a", " member-b "},
				CreatedTime: createdAt,
				UpdatedTime: &updatedAt,
			},
		},
	}
	service := NewWorkspaceSyncService(db, testWorkspaceSyncHilbertConfig(), client)

	result, err := service.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if result.SyncedCount != 1 || !result.DefaultIncluded || result.LastSyncedAt.IsZero() {
		t.Fatalf("unexpected result: %#v", result)
	}
	if client.loginCode != "svc-keystone" || client.loginPassword != "svc-secret" || client.listSessionKey != "session-key" {
		t.Fatalf("unexpected client calls: %#v", client)
	}

	var rows []struct {
		ID               int64          `db:"id"`
		Name             string         `db:"name"`
		Description      string         `db:"description"`
		Source           string         `db:"source"`
		AdminsStr        sql.NullString `db:"admins_str"`
		MembersStr       sql.NullString `db:"members_str"`
		LastSyncedAt     sql.NullTime   `db:"last_synced_at"`
		HilbertCreatedAt sql.NullTime   `db:"hilbert_created_at"`
		HilbertUpdatedAt sql.NullTime   `db:"hilbert_updated_at"`
	}
	if err := db.Select(&rows, `
		SELECT id, name, description, source, admins_str, members_str,
		       last_synced_at, hilbert_created_at, hilbert_updated_at
		FROM workspaces
		ORDER BY id
	`); err != nil {
		t.Fatalf("query workspaces: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows=%#v want default + one Hilbert workspace", rows)
	}
	if rows[0].ID != 0 || rows[0].Source != workspaceSourceDefault {
		t.Fatalf("unexpected default row: %#v", rows[0])
	}
	if rows[1].ID != 123 ||
		rows[1].Name != "Customer Workspace" ||
		rows[1].Description != "synced from Hilbert" ||
		rows[1].Source != workspaceSourceHilbert ||
		rows[1].AdminsStr.String != "#admin-a#" ||
		rows[1].MembersStr.String != "#member-a#member-b#" ||
		!rows[1].LastSyncedAt.Valid ||
		!rows[1].HilbertCreatedAt.Valid ||
		!rows[1].HilbertUpdatedAt.Valid {
		t.Fatalf("unexpected Hilbert row: %#v", rows[1])
	}
}

func TestWorkspaceSyncServiceSyncRequiresConfig(t *testing.T) {
	db := newTestWorkspaceSyncDB(t)
	defer db.Close()

	service := NewWorkspaceSyncService(db, &config.HilbertConfig{BaseURL: "http://hilbert", TimeoutSeconds: 2}, &fakeHilbertWorkspaceClient{configured: true})
	if _, err := service.Sync(context.Background()); !errors.Is(err, ErrWorkspaceSyncNotConfigured) {
		t.Fatalf("Sync() error = %v, want ErrWorkspaceSyncNotConfigured", err)
	}
}

func TestWorkspaceSyncServiceInvalidHilbertRecordDoesNotPartiallyUpsert(t *testing.T) {
	db := newTestWorkspaceSyncDB(t)
	defer db.Close()

	client := &fakeHilbertWorkspaceClient{
		loginResult: auth.NewHilbertLoginResult(auth.HilbertAccount{}, "session-key"),
		workspaces: []auth.HilbertWorkspace{
			{ID: 123, Name: "Valid Workspace"},
			{ID: 0, Name: "Invalid Workspace"},
		},
	}
	service := NewWorkspaceSyncService(db, testWorkspaceSyncHilbertConfig(), client)

	if _, err := service.Sync(context.Background()); !errors.Is(err, ErrWorkspaceSyncFailed) {
		t.Fatalf("Sync() error = %v, want ErrWorkspaceSyncFailed", err)
	}

	var count int
	if err := db.Get(&count, "SELECT COUNT(*) FROM workspaces WHERE id = 123"); err != nil {
		t.Fatalf("count workspace: %v", err)
	}
	if count != 0 {
		t.Fatalf("workspace 123 was partially upserted")
	}

	var defaultCount int
	if err := db.Get(&defaultCount, "SELECT COUNT(*) FROM workspaces WHERE id = 0"); err != nil {
		t.Fatalf("count default workspace: %v", err)
	}
	if defaultCount != 1 {
		t.Fatalf("defaultCount=%d want 1", defaultCount)
	}
}

type fakeHilbertWorkspaceClient struct {
	configured     bool
	loginResult    *auth.HilbertLoginResult
	loginErr       error
	workspaces     []auth.HilbertWorkspace
	listErr        error
	loginCode      string
	loginPassword  string
	listSessionKey string
}

func (f *fakeHilbertWorkspaceClient) Configured() bool {
	if f.configured {
		return true
	}
	return f.loginResult != nil || len(f.workspaces) > 0 || f.loginErr != nil || f.listErr != nil
}

func (f *fakeHilbertWorkspaceClient) Login(_ context.Context, code string, password string) (*auth.HilbertLoginResult, error) {
	f.loginCode = code
	f.loginPassword = password
	if f.loginErr != nil {
		return nil, f.loginErr
	}
	return f.loginResult, nil
}

func (f *fakeHilbertWorkspaceClient) ListAvailableWorkspaces(_ context.Context, sessionKey string) ([]auth.HilbertWorkspace, error) {
	f.listSessionKey = sessionKey
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.workspaces, nil
}

func testWorkspaceSyncHilbertConfig() *config.HilbertConfig {
	return &config.HilbertConfig{
		BaseURL:                "http://hilbert",
		TimeoutSeconds:         2,
		ServiceAccountCode:     "svc-keystone",
		ServiceAccountPassword: "svc-secret",
	}
}

func newTestWorkspaceSyncDB(t *testing.T) *sqlx.DB {
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
