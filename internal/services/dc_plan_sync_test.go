// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"archebase.com/keystone-edge/internal/auth"
	"archebase.com/keystone-edge/internal/config"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

func TestDCPlanSyncServiceRejectsDefaultWorkspace(t *testing.T) {
	db := newTestDCPlanSyncDB(t)
	defer db.Close()
	seedDCPlanWorkspace(t, db, 0, workspaceSourceDefault)

	service := NewDCPlanSyncService(db, testDCPlanSyncHilbertConfig(), &fakeHilbertDCPlanClient{configured: true})
	if _, err := service.SyncWorkspace(context.Background(), 0); !errors.Is(err, ErrDCPlanSyncInvalidWorkspace) {
		t.Fatalf("SyncWorkspace() error = %v, want ErrDCPlanSyncInvalidWorkspace", err)
	}
}

func TestDCPlanSyncServiceRejectsNonHilbertWorkspace(t *testing.T) {
	db := newTestDCPlanSyncDB(t)
	defer db.Close()
	seedDCPlanWorkspace(t, db, 9, workspaceSourceDefault)

	service := NewDCPlanSyncService(db, testDCPlanSyncHilbertConfig(), &fakeHilbertDCPlanClient{configured: true})
	if _, err := service.SyncWorkspace(context.Background(), 9); !errors.Is(err, ErrDCPlanSyncInvalidWorkspace) {
		t.Fatalf("SyncWorkspace() error = %v, want ErrDCPlanSyncInvalidWorkspace", err)
	}
}

func TestDCPlanSyncServiceSyncsPagedPlans(t *testing.T) {
	db := newTestDCPlanSyncDB(t)
	defer db.Close()
	seedDCPlanWorkspace(t, db, 123, workspaceSourceHilbert)

	client := &fakeHilbertDCPlanClient{
		loginResult: auth.NewHilbertLoginResult(auth.HilbertAccount{}, "session-key"),
		pages: []*auth.HilbertDCPlanPage{
			{Records: []auth.HilbertDCPlan{testHilbertDCPlan(1001, 123, "Plan A")}, Total: 2, PageNum: 1, PageSize: dcPlanSyncPageSize},
			{Records: []auth.HilbertDCPlan{testHilbertDCPlan(1002, 123, "Plan B")}, Total: 2, PageNum: 2, PageSize: dcPlanSyncPageSize},
		},
	}
	service := NewDCPlanSyncService(db, testDCPlanSyncHilbertConfig(), client)

	result, err := service.SyncWorkspace(context.Background(), 123)
	if err != nil {
		t.Fatalf("SyncWorkspace() error = %v", err)
	}
	if result.WorkspaceID != 123 || result.SyncedCount != 2 || result.PageCount != 2 || result.LastSyncedAt.IsZero() {
		t.Fatalf("unexpected result: %#v", result)
	}
	if client.loginCode != "svc-keystone" || client.loginPassword != "svc-secret" {
		t.Fatalf("unexpected login args: %#v", client)
	}
	if len(client.queries) != 2 || client.queries[0].pageNum != 1 || client.queries[1].pageNum != 2 {
		t.Fatalf("unexpected page queries: %#v", client.queries)
	}

	var rows []struct {
		ID          int64  `db:"id"`
		WorkspaceID int64  `db:"workspace_id"`
		Name        string `db:"name"`
		DCType      string `db:"dc_type"`
		RawPayload  string `db:"raw_payload"`
	}
	if err := db.Select(&rows, "SELECT id, workspace_id, name, dc_type, raw_payload FROM dc_plan ORDER BY id"); err != nil {
		t.Fatalf("query dc_plan: %v", err)
	}
	if len(rows) != 2 || rows[0].ID != 1001 || rows[0].WorkspaceID != 123 || rows[0].Name != "Plan A" || rows[0].DCType != "ego" || rows[0].RawPayload == "" {
		t.Fatalf("unexpected rows: %#v", rows)
	}
}

func TestDCPlanSyncServiceInvalidPlanDoesNotPartiallyUpsert(t *testing.T) {
	db := newTestDCPlanSyncDB(t)
	defer db.Close()
	seedDCPlanWorkspace(t, db, 123, workspaceSourceHilbert)

	client := &fakeHilbertDCPlanClient{
		loginResult: auth.NewHilbertLoginResult(auth.HilbertAccount{}, "session-key"),
		pages: []*auth.HilbertDCPlanPage{
			{
				Records: []auth.HilbertDCPlan{
					testHilbertDCPlan(1001, 123, "Plan A"),
					testHilbertDCPlan(0, 123, "Invalid Plan"),
				},
				Total:    2,
				PageNum:  1,
				PageSize: dcPlanSyncPageSize,
			},
		},
	}
	service := NewDCPlanSyncService(db, testDCPlanSyncHilbertConfig(), client)

	if _, err := service.SyncWorkspace(context.Background(), 123); !errors.Is(err, ErrDCPlanSyncFailed) {
		t.Fatalf("SyncWorkspace() error = %v, want ErrDCPlanSyncFailed", err)
	}

	var count int
	if err := db.Get(&count, "SELECT COUNT(*) FROM dc_plan"); err != nil {
		t.Fatalf("count dc_plan: %v", err)
	}
	if count != 0 {
		t.Fatalf("count=%d want 0", count)
	}
}

func TestDCPlanSyncServiceRejectsPlanIDOwnedByAnotherWorkspace(t *testing.T) {
	db := newTestDCPlanSyncDB(t)
	defer db.Close()
	seedDCPlanWorkspace(t, db, 123, workspaceSourceHilbert)
	seedDCPlanWorkspace(t, db, 456, workspaceSourceHilbert)
	seedDCPlanRow(t, db, testHilbertDCPlan(1001, 456, "Existing Plan"))

	client := &fakeHilbertDCPlanClient{
		loginResult: auth.NewHilbertLoginResult(auth.HilbertAccount{}, "session-key"),
		pages: []*auth.HilbertDCPlanPage{
			{Records: []auth.HilbertDCPlan{testHilbertDCPlan(1001, 123, "Incoming Plan")}, Total: 1, PageNum: 1, PageSize: dcPlanSyncPageSize},
		},
	}
	service := NewDCPlanSyncService(db, testDCPlanSyncHilbertConfig(), client)

	if _, err := service.SyncWorkspace(context.Background(), 123); !errors.Is(err, ErrDCPlanSyncFailed) {
		t.Fatalf("SyncWorkspace() error = %v, want ErrDCPlanSyncFailed", err)
	}

	var workspaceID int64
	if err := db.Get(&workspaceID, "SELECT workspace_id FROM dc_plan WHERE id = ?", 1001); err != nil {
		t.Fatalf("query existing dc_plan: %v", err)
	}
	if workspaceID != 456 {
		t.Fatalf("workspace_id=%d want 456", workspaceID)
	}
}

func TestDCPlanSyncServiceDoesNotDeleteMissingPlans(t *testing.T) {
	db := newTestDCPlanSyncDB(t)
	defer db.Close()
	seedDCPlanWorkspace(t, db, 123, workspaceSourceHilbert)
	seedDCPlanRow(t, db, testHilbertDCPlan(1001, 123, "Existing Plan"))

	client := &fakeHilbertDCPlanClient{
		loginResult: auth.NewHilbertLoginResult(auth.HilbertAccount{}, "session-key"),
		pages: []*auth.HilbertDCPlanPage{
			{Records: []auth.HilbertDCPlan{testHilbertDCPlan(1002, 123, "Incoming Plan")}, Total: 1, PageNum: 1, PageSize: dcPlanSyncPageSize},
		},
	}
	service := NewDCPlanSyncService(db, testDCPlanSyncHilbertConfig(), client)

	if _, err := service.SyncWorkspace(context.Background(), 123); err != nil {
		t.Fatalf("SyncWorkspace() error = %v", err)
	}

	var count int
	if err := db.Get(&count, "SELECT COUNT(*) FROM dc_plan WHERE workspace_id = ?", 123); err != nil {
		t.Fatalf("count dc_plan: %v", err)
	}
	if count != 2 {
		t.Fatalf("count=%d want 2", count)
	}
}

type dcPlanQueryCall struct {
	sessionKey  string
	workspaceID int64
	pageNum     int64
	pageSize    int64
}

type fakeHilbertDCPlanClient struct {
	configured    bool
	loginResult   *auth.HilbertLoginResult
	loginErr      error
	pages         []*auth.HilbertDCPlanPage
	queryErr      error
	loginCode     string
	loginPassword string
	queries       []dcPlanQueryCall
}

func (f *fakeHilbertDCPlanClient) Configured() bool {
	if f.configured {
		return true
	}
	return f.loginResult != nil || len(f.pages) > 0 || f.loginErr != nil || f.queryErr != nil
}

func (f *fakeHilbertDCPlanClient) Login(_ context.Context, code string, password string) (*auth.HilbertLoginResult, error) {
	f.loginCode = code
	f.loginPassword = password
	if f.loginErr != nil {
		return nil, f.loginErr
	}
	return f.loginResult, nil
}

func (f *fakeHilbertDCPlanClient) QueryDCPlans(_ context.Context, sessionKey string, workspaceID int64, pageNum int64, pageSize int64) (*auth.HilbertDCPlanPage, error) {
	f.queries = append(f.queries, dcPlanQueryCall{sessionKey: sessionKey, workspaceID: workspaceID, pageNum: pageNum, pageSize: pageSize})
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	index := int(pageNum - 1)
	if index < 0 || index >= len(f.pages) {
		return &auth.HilbertDCPlanPage{Records: nil, Total: int64(len(f.pages)), PageNum: pageNum, PageSize: pageSize}, nil
	}
	return f.pages[index], nil
}

func testHilbertDCPlan(id int64, workspaceID int64, name string) auth.HilbertDCPlan {
	createdAt := time.Date(2026, 7, 9, 3, 4, 5, 0, time.UTC)
	return auth.HilbertDCPlan{
		ID:                  id,
		WorkspaceID:         workspaceID,
		Name:                name,
		Description:         nil,
		DCFactoryID:         11,
		DCServiceProviderID: 12,
		Operator:            "collector-a",
		DCProjectID:         13,
		DCTaskID:            14,
		DCDeviceID:          15,
		DCType:              "ego",
		DCDate:              "2026-07-09",
		TargetCount:         20,
		CurCount:            2,
		TargetDuration:      3600,
		CurDuration:         120,
		CreatedBy:           "planner",
		CreatedTime:         createdAt,
	}
}

func seedDCPlanWorkspace(t *testing.T, db *sqlx.DB, id int64, source string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO workspaces (id, name, source, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
	`, id, "Workspace", source, time.Now().UTC(), time.Now().UTC()); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
}

func seedDCPlanRow(t *testing.T, db *sqlx.DB, plan auth.HilbertDCPlan) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO dc_plan (
			id, workspace_id, name, dc_factory_id, dc_service_provider_id, operator,
			dc_project_id, dc_task_id, dc_device_id, dc_type, dc_date,
			target_count, cur_count, target_duration, cur_duration, created_by,
			created_time, raw_payload, last_synced_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, plan.ID, plan.WorkspaceID, plan.Name, plan.DCFactoryID, plan.DCServiceProviderID, plan.Operator,
		plan.DCProjectID, plan.DCTaskID, plan.DCDeviceID, plan.DCType, plan.DCDate,
		plan.TargetCount, plan.CurCount, plan.TargetDuration, plan.CurDuration, plan.CreatedBy,
		plan.CreatedTime, "{}", time.Now().UTC()); err != nil {
		t.Fatalf("seed dc_plan: %v", err)
	}
}

func testDCPlanSyncHilbertConfig() *config.HilbertConfig {
	return &config.HilbertConfig{
		BaseURL:                "http://hilbert",
		TimeoutSeconds:         2,
		ServiceAccountCode:     "svc-keystone",
		ServiceAccountPassword: "svc-secret",
	}
}

func newTestDCPlanSyncDB(t *testing.T) *sqlx.DB {
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
		CREATE TABLE dc_plan (
			id INTEGER PRIMARY KEY,
			workspace_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			description TEXT,
			dc_factory_id INTEGER NOT NULL,
			dc_service_provider_id INTEGER NOT NULL,
			operator TEXT NOT NULL,
			dc_project_id INTEGER NOT NULL,
			dc_task_id INTEGER NOT NULL,
			dc_device_id INTEGER NOT NULL,
			dc_type TEXT NOT NULL,
			dc_date TEXT NOT NULL,
			target_count INTEGER NOT NULL,
			cur_count INTEGER NOT NULL DEFAULT 0,
			target_duration INTEGER NOT NULL,
			cur_duration INTEGER NOT NULL DEFAULT 0,
			created_by TEXT NOT NULL,
			created_time TIMESTAMP NOT NULL,
			updated_by TEXT,
			updated_time TIMESTAMP,
			raw_payload TEXT,
			last_synced_at TIMESTAMP,
			sync_error TEXT,
			local_created_at TIMESTAMP,
			local_updated_at TIMESTAMP,
			deleted_at TIMESTAMP
		);
	`); err != nil {
		db.Close()
		t.Fatalf("create tables: %v", err)
	}
	return db
}
