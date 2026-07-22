// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"context"
	"errors"
	"strconv"
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
	if len(client.queries) != 2 || client.queries[0].pageNum != 1 || client.queries[1].pageNum != 2 {
		t.Fatalf("unexpected page queries: %#v", client.queries)
	}

	var rows []struct {
		ID            int64  `db:"id"`
		WorkspaceID   int64  `db:"workspace_id"`
		Name          string `db:"name"`
		OperatorName  string `db:"operator_display_name"`
		DCProjectName string `db:"dc_project_name"`
		DCProjectDesc string `db:"dc_project_description"`
		DCTaskName    string `db:"dc_task_name"`
		DCTaskDesc    string `db:"dc_task_description"`
		DCDeviceName  string `db:"dc_device_name"`
		DCType        string `db:"dc_type"`
		RawPayload    string `db:"raw_payload"`
	}
	if err := db.Select(&rows, "SELECT id, workspace_id, name, operator_display_name, dc_project_name, dc_project_description, dc_task_name, dc_task_description, dc_device_name, dc_type, raw_payload FROM dc_plan ORDER BY id"); err != nil {
		t.Fatalf("query dc_plan: %v", err)
	}
	if len(rows) != 2 || rows[0].ID != 1001 || rows[0].WorkspaceID != 123 || rows[0].Name != "Plan A" || rows[0].OperatorName != "Collector 1001" || rows[0].DCProjectName != "Project 1001" || rows[0].DCProjectDesc != "Project description 1001" || rows[0].DCTaskName != "Task 1001" || rows[0].DCTaskDesc != "Description 1001" || rows[0].DCDeviceName != "Device 1001" || rows[0].DCType != "ego" || rows[0].RawPayload == "" {
		t.Fatalf("unexpected rows: %#v", rows)
	}
}

func TestDCPlanSyncServiceInvalidPlanDoesNotPartiallyUpsert(t *testing.T) {
	db := newTestDCPlanSyncDB(t)
	defer db.Close()
	seedDCPlanWorkspace(t, db, 123, workspaceSourceHilbert)

	client := &fakeHilbertDCPlanClient{
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

func TestDCPlanSyncServiceDeactivatesMissingPlansWithoutCreatingTasks(t *testing.T) {
	db := newTestDCPlanSyncDB(t)
	defer db.Close()
	seedDCPlanWorkspace(t, db, 123, workspaceSourceHilbert)
	seedDCPlanRow(t, db, testHilbertDCPlan(1001, 123, "Existing Plan"))

	client := &fakeHilbertDCPlanClient{
		pages: []*auth.HilbertDCPlanPage{
			{Records: []auth.HilbertDCPlan{testHilbertDCPlan(1002, 123, "Incoming Plan")}, Total: 1, PageNum: 1, PageSize: dcPlanSyncPageSize},
		},
	}
	service := NewDCPlanSyncService(db, testDCPlanSyncHilbertConfig(), client)

	if _, err := service.SyncWorkspace(context.Background(), 123); err != nil {
		t.Fatalf("SyncWorkspace() error = %v", err)
	}

	var rows []struct {
		ID        int64      `db:"id"`
		DeletedAt *time.Time `db:"deleted_at"`
	}
	if err := db.Select(&rows, "SELECT id, deleted_at FROM dc_plan WHERE workspace_id = ? ORDER BY id", 123); err != nil {
		t.Fatalf("query dc_plan: %v", err)
	}
	if len(rows) != 2 || rows[0].ID != 1001 || rows[0].DeletedAt == nil || rows[1].ID != 1002 || rows[1].DeletedAt != nil {
		t.Fatalf("unexpected rows: %#v", rows)
	}

	var taskCount int
	if err := db.Get(&taskCount, "SELECT COUNT(*) FROM tasks"); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if taskCount != 0 {
		t.Fatalf("taskCount=%d want 0", taskCount)
	}
}

func TestDCPlanSyncServiceProjectsWorkstationWithoutCreatingTasks(t *testing.T) {
	db := newTestDCPlanSyncDB(t)
	defer db.Close()
	seedDCPlanWorkspace(t, db, 123, workspaceSourceHilbert)
	seedDCPlanProjectionResources(t, db, 123, "collector-a", 15)

	client := &fakeHilbertDCPlanClient{
		pages: []*auth.HilbertDCPlanPage{
			{
				Records: []auth.HilbertDCPlan{
					testHilbertDCPlan(1001, 123, "Plan A"),
					testHilbertDCPlan(1002, 123, "Plan B"),
				},
				Total:    2,
				PageNum:  1,
				PageSize: dcPlanSyncPageSize,
			},
		},
	}
	service := NewDCPlanSyncService(db, testDCPlanSyncHilbertConfig(), client)

	if _, err := service.SyncWorkspace(context.Background(), 123); err != nil {
		t.Fatalf("SyncWorkspace() error = %v", err)
	}

	var workstationCount int
	if err := db.Get(&workstationCount, `
		SELECT COUNT(*) FROM workstations WHERE workspace_id = ? AND deleted_at IS NULL
	`, 123); err != nil {
		t.Fatalf("query projected workstation: %v", err)
	}
	if workstationCount != 1 {
		t.Fatalf("projected workstation count=%d want 1", workstationCount)
	}
	var workstation struct {
		Status    string `db:"status"`
		IsCurrent bool   `db:"is_current"`
	}
	if err := db.Get(&workstation, `
		SELECT status, is_current
		FROM workstations
		WHERE workspace_id = ? AND deleted_at IS NULL
	`, 123); err != nil {
		t.Fatalf("read projected workstation: %v", err)
	}
	if workstation.Status != "offline" || workstation.IsCurrent {
		t.Fatalf("projected workstation=%+v want offline non-current binding", workstation)
	}

	var taskCount int
	if err := db.Get(&taskCount, "SELECT COUNT(*) FROM tasks"); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if taskCount != 0 {
		t.Fatalf("taskCount=%d want 0", taskCount)
	}
}

func TestDCPlanSyncServiceSyncAllWorkspaces(t *testing.T) {
	db := newTestDCPlanSyncDB(t)
	defer db.Close()
	seedDCPlanWorkspace(t, db, 0, workspaceSourceDefault)
	seedDCPlanWorkspace(t, db, 123, workspaceSourceHilbert)
	seedDCPlanWorkspace(t, db, 456, workspaceSourceHilbert)
	seedDCPlanWorkspace(t, db, 789, workspaceSourceDefault)

	client := &fakeHilbertDCPlanClient{
		pagesByWorkspace: map[int64][]*auth.HilbertDCPlanPage{
			123: {
				{Records: []auth.HilbertDCPlan{testHilbertDCPlan(1001, 123, "Plan A")}, Total: 1, PageNum: 1, PageSize: dcPlanSyncPageSize},
			},
			456: {
				{Records: []auth.HilbertDCPlan{testHilbertDCPlan(2001, 456, "Plan B")}, Total: 1, PageNum: 1, PageSize: dcPlanSyncPageSize},
			},
		},
	}
	service := NewDCPlanSyncService(db, testDCPlanSyncHilbertConfig(), client)

	result, err := service.SyncAllWorkspaces(context.Background())
	if err != nil {
		t.Fatalf("SyncAllWorkspaces() error = %v", err)
	}
	if result.WorkspaceCount != 2 || result.FailedCount != 0 || result.SyncedCount != 2 || result.PageCount != 2 {
		t.Fatalf("unexpected result: %#v", result)
	}
	if len(client.queries) != 2 || client.queries[0].workspaceID != 123 || client.queries[1].workspaceID != 456 {
		t.Fatalf("unexpected queries: %#v", client.queries)
	}

	var count int
	if err := db.Get(&count, "SELECT COUNT(*) FROM dc_plan"); err != nil {
		t.Fatalf("count dc_plan: %v", err)
	}
	if count != 2 {
		t.Fatalf("count=%d want 2", count)
	}
}

func TestDCPlanSyncServiceSyncAllWorkspacesIsolatesFailures(t *testing.T) {
	db := newTestDCPlanSyncDB(t)
	defer db.Close()
	seedDCPlanWorkspace(t, db, 123, workspaceSourceHilbert)
	seedDCPlanWorkspace(t, db, 456, workspaceSourceHilbert)

	client := &fakeHilbertDCPlanClient{
		pagesByWorkspace: map[int64][]*auth.HilbertDCPlanPage{
			123: {
				{
					Records: []auth.HilbertDCPlan{testHilbertDCPlan(0, 123, "Invalid Plan")},
					Total:   1,
				},
			},
			456: {
				{Records: []auth.HilbertDCPlan{testHilbertDCPlan(2001, 456, "Plan B")}, Total: 1, PageNum: 1, PageSize: dcPlanSyncPageSize},
			},
		},
	}
	service := NewDCPlanSyncService(db, testDCPlanSyncHilbertConfig(), client)

	result, err := service.SyncAllWorkspaces(context.Background())
	if err != nil {
		t.Fatalf("SyncAllWorkspaces() error = %v", err)
	}
	if result.WorkspaceCount != 2 || result.FailedCount != 1 || result.SyncedCount != 1 || len(result.Errors) != 1 {
		t.Fatalf("unexpected result: %#v", result)
	}

	var ids []int64
	if err := db.Select(&ids, "SELECT id FROM dc_plan ORDER BY id"); err != nil {
		t.Fatalf("query dc_plan ids: %v", err)
	}
	if len(ids) != 1 || ids[0] != 2001 {
		t.Fatalf("ids=%#v want [2001]", ids)
	}
}

type dcPlanQueryCall struct {
	workspaceID int64
	pageNum     int64
	pageSize    int64
}

type fakeHilbertDCPlanClient struct {
	configured       bool
	pages            []*auth.HilbertDCPlanPage
	pagesByWorkspace map[int64][]*auth.HilbertDCPlanPage
	queryErr         error
	queries          []dcPlanQueryCall
}

func (f *fakeHilbertDCPlanClient) Configured() bool {
	if f.configured {
		return true
	}
	return len(f.pages) > 0 || len(f.pagesByWorkspace) > 0 || f.queryErr != nil
}

func (f *fakeHilbertDCPlanClient) ServiceAuthConfigured() bool {
	return f.Configured()
}

func (f *fakeHilbertDCPlanClient) QueryDCPlans(_ context.Context, workspaceID int64, pageNum int64, pageSize int64) (*auth.HilbertDCPlanPage, error) {
	f.queries = append(f.queries, dcPlanQueryCall{workspaceID: workspaceID, pageNum: pageNum, pageSize: pageSize})
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	pages := f.pages
	if f.pagesByWorkspace != nil {
		pages = f.pagesByWorkspace[workspaceID]
	}
	index := int(pageNum - 1)
	if index < 0 || index >= len(pages) {
		return &auth.HilbertDCPlanPage{Records: nil, Total: int64(len(pages)), PageNum: pageNum, PageSize: pageSize}, nil
	}
	return pages[index], nil
}

func testHilbertDCPlan(id int64, workspaceID int64, name string) auth.HilbertDCPlan {
	createdAt := time.Date(2026, 7, 9, 3, 4, 5, 0, time.UTC)
	return auth.HilbertDCPlan{
		ID:                   id,
		WorkspaceID:          workspaceID,
		Name:                 name,
		Description:          nil,
		DCFactoryID:          11,
		DCServiceProviderID:  12,
		Operator:             "collector-a",
		OperatorDisplayName:  "Collector " + strconv.FormatInt(id, 10),
		DCProjectID:          13,
		DCProjectName:        "Project " + strconv.FormatInt(id, 10),
		DCProjectDescription: "Project description " + strconv.FormatInt(id, 10),
		DCTaskID:             14,
		DCTaskName:           "Task " + strconv.FormatInt(id, 10),
		DCTaskDescription:    "Description " + strconv.FormatInt(id, 10),
		DCDeviceID:           15,
		DCDeviceName:         "Device " + strconv.FormatInt(id, 10),
		DCType:               "ego",
		DCDate:               "2026-07-09",
		TargetCount:          20,
		CurCount:             2,
		TargetDuration:       3600,
		CurDuration:          120,
		CreatedBy:            "planner",
		CreatedTime:          createdAt,
	}
}

func seedDCPlanWorkspace(t *testing.T, db *sqlx.DB, id int64, source string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO workspaces (id, name, source, admins, members, created_at, updated_at)
		VALUES (?, ?, ?, '[]', '[]', ?, ?)
	`, id, "Workspace", source, time.Now().UTC(), time.Now().UTC()); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
}

func seedDCPlanProjectionResources(
	t *testing.T,
	db *sqlx.DB,
	workspaceID int64,
	operatorID string,
	deviceID int64,
) {
	t.Helper()
	if _, err := db.Exec("UPDATE workspaces SET members = ? WHERE id = ?", `["`+operatorID+`"]`, workspaceID); err != nil {
		t.Fatalf("seed workspace member: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO data_collectors (name, operator_id, status)
		VALUES (?, ?, 'active')
	`, "Collector A", operatorID); err != nil {
		t.Fatalf("seed data collector: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO robots (device_id, device_name, workspace_id, status)
		VALUES (?, ?, ?, 'active')
	`, strconv.FormatInt(deviceID, 10), "Robot A", workspaceID); err != nil {
		t.Fatalf("seed robot: %v", err)
	}
}

func seedDCPlanRow(t *testing.T, db *sqlx.DB, plan auth.HilbertDCPlan) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO dc_plan (
			id, workspace_id, name, dc_factory_id, dc_service_provider_id, operator, operator_display_name,
			dc_project_id, dc_project_name, dc_project_description, dc_task_id, dc_task_name, dc_task_description, dc_device_id, dc_device_name, dc_type, dc_date,
			target_count, cur_count, target_duration, cur_duration, created_by,
			created_time, raw_payload, last_synced_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, plan.ID, plan.WorkspaceID, plan.Name, plan.DCFactoryID, plan.DCServiceProviderID, plan.Operator,
		plan.OperatorDisplayName, plan.DCProjectID, plan.DCProjectName, plan.DCProjectDescription, plan.DCTaskID, plan.DCTaskName, plan.DCTaskDescription, plan.DCDeviceID, plan.DCDeviceName, plan.DCType, plan.DCDate,
		plan.TargetCount, plan.CurCount, plan.TargetDuration, plan.CurDuration, plan.CreatedBy,
		plan.CreatedTime, "{}", time.Now().UTC()); err != nil {
		t.Fatalf("seed dc_plan: %v", err)
	}
}

func testDCPlanSyncHilbertConfig() *config.HilbertConfig {
	return &config.HilbertConfig{
		BaseURL:        "http://hilbert",
		TimeoutSeconds: 2,
		AccessKey:      "hilbert-ak",
		SecretKey:      "hilbert-sk",
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
			admins TEXT,
			members TEXT,
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
			operator_display_name TEXT,
			dc_project_id INTEGER NOT NULL,
			dc_project_name TEXT,
			dc_project_description TEXT,
			dc_task_id INTEGER NOT NULL,
			dc_task_name TEXT,
			dc_task_description TEXT,
			dc_device_id INTEGER NOT NULL,
			dc_device_name TEXT,
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
		CREATE TABLE tasks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			task_id TEXT NOT NULL,
			dc_plan_id INTEGER,
			status TEXT,
			deleted_at TIMESTAMP
		);
		CREATE TABLE data_collectors (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			operator_id TEXT NOT NULL UNIQUE,
			status TEXT NOT NULL,
			metadata TEXT,
			deleted_at TIMESTAMP
		);
		CREATE TABLE robots (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			device_id TEXT NOT NULL UNIQUE,
			device_name TEXT,
			workspace_id INTEGER NOT NULL,
			status TEXT NOT NULL,
			metadata TEXT,
			deleted_at TIMESTAMP
		);
		CREATE TABLE workstations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			robot_id INTEGER NOT NULL,
			robot_name TEXT,
			robot_serial TEXT,
			data_collector_id INTEGER NOT NULL,
			collector_name TEXT,
			collector_operator_id TEXT,
			workspace_id INTEGER NOT NULL,
			name TEXT,
			status TEXT NOT NULL,
			metadata TEXT,
			created_at TIMESTAMP,
			updated_at TIMESTAMP,
			deleted_at TIMESTAMP,
			is_current BOOLEAN NOT NULL DEFAULT FALSE,
			superseded_at TIMESTAMP,
			superseded_by INTEGER
		);
	`); err != nil {
		db.Close()
		t.Fatalf("create tables: %v", err)
	}
	return db
}
