// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"context"
	"database/sql"
	"errors"
	"strings"
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

	var rows []struct {
		ID               int64          `db:"id"`
		Name             string         `db:"name"`
		Description      string         `db:"description"`
		Source           string         `db:"source"`
		Admins           sql.NullString `db:"admins"`
		Members          sql.NullString `db:"members"`
		LastSyncedAt     sql.NullTime   `db:"last_synced_at"`
		HilbertCreatedAt sql.NullTime   `db:"hilbert_created_at"`
		HilbertUpdatedAt sql.NullTime   `db:"hilbert_updated_at"`
	}
	if err := db.Select(&rows, `
		SELECT id, name, description, source, admins, members,
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
		rows[1].Admins.String != `["admin-a"]` ||
		rows[1].Members.String != `["member-a","member-b"]` ||
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

func TestWorkspaceSyncServiceSyncsWorkspaceResources(t *testing.T) {
	db := newTestWorkspaceSyncDB(t)
	defer db.Close()

	client := &fakeHilbertWorkspaceClient{
		workspaces: []auth.HilbertWorkspace{
			{ID: 123, Name: "Resource Workspace", Admins: []string{"collector-a", "customer-a"}, Members: []string{"missing-a"}},
		},
		accounts: map[string]*auth.HilbertAccount{
			"collector-a": {
				ID:               7,
				Code:             "collector-a",
				DisplayName:      "Collector A",
				Role:             "external_user",
				ExternalUserType: "data_supplier",
				Status:           "enabled",
			},
			"customer-a": {
				ID:               8,
				Code:             "customer-a",
				DisplayName:      "Customer A",
				Role:             "external_user",
				ExternalUserType: "customer",
				Status:           "enabled",
			},
		},
		devicesByWorkspace: map[int64][]auth.HilbertDCDevice{
			123: {
				{ID: 456, WorkspaceID: 123, Name: "Device A", SN: "SN-A", DCDeviceTypeID: 77},
			},
		},
		deviceTypes: map[int64]*auth.HilbertDCDeviceType{
			77: {ID: 77, Name: "Type 77"},
		},
	}
	service := NewWorkspaceSyncService(db, testWorkspaceSyncHilbertConfig(), client)

	result, err := service.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if result.ResourceSync == nil {
		t.Fatalf("ResourceSync is nil")
	}
	if result.ResourceSync.CollectorUpsertedCount != 2 ||
		result.ResourceSync.CollectorSkippedCount != 1 ||
		result.ResourceSync.RobotUpsertedCount != 1 {
		t.Fatalf("unexpected resource summary: %#v", result.ResourceSync)
	}

	var collector struct {
		Name     string `db:"name"`
		Status   string `db:"status"`
		Metadata string `db:"metadata"`
	}
	if err := db.Get(&collector, `
		SELECT name, status, metadata
		FROM data_collectors
		WHERE operator_id = 'collector-a'
	`); err != nil {
		t.Fatalf("query collector: %v", err)
	}
	if collector.Name != "Collector A" || collector.Status != "active" || metadataSource(collector.Metadata) != "hilbert" {
		t.Fatalf("unexpected collector: %#v", collector)
	}
	var customerCount int
	if err := db.Get(&customerCount, "SELECT COUNT(*) FROM data_collectors WHERE operator_id = 'customer-a'"); err != nil {
		t.Fatalf("count customer collector: %v", err)
	}
	if customerCount != 1 {
		t.Fatalf("customerCount=%d want 1", customerCount)
	}

	var robot struct {
		Status       string         `db:"status"`
		DeviceTypeID sql.NullInt64  `db:"device_type_id"`
		DeviceType   sql.NullString `db:"device_type"`
		Metadata     string         `db:"metadata"`
	}
	if err := db.Get(&robot, `
		SELECT status, device_type_id, device_type, metadata
		FROM robots
		WHERE device_id = '456'
	`); err != nil {
		t.Fatalf("query robot: %v", err)
	}
	if robot.Status != "active" ||
		!robot.DeviceTypeID.Valid ||
		robot.DeviceTypeID.Int64 != 77 ||
		!robot.DeviceType.Valid ||
		robot.DeviceType.String != "Type 77" ||
		metadataSource(robot.Metadata) != "hilbert" {
		t.Fatalf("unexpected robot: %#v", robot)
	}
}

func TestWorkspaceSyncServiceProjectsWorkspacePeopleAsCollectors(t *testing.T) {
	db := newTestWorkspaceSyncDB(t)
	defer db.Close()

	client := &fakeHilbertWorkspaceClient{
		workspaces: []auth.HilbertWorkspace{
			{ID: 123, Name: "Resource Workspace", Admins: []string{"operator-a", "internal-a"}},
		},
		accounts: map[string]*auth.HilbertAccount{
			"operator-a": {
				ID:               7,
				Code:             "operator-a",
				DisplayName:      "Operator A",
				Role:             "external_user",
				ExternalUserType: "customer",
				Status:           "enabled",
			},
			"internal-a": {
				ID:               8,
				Code:             "internal-a",
				DisplayName:      "Internal A",
				Role:             "internal_user",
				ExternalUserType: "",
				Status:           "disabled",
			},
		},
		devicesByWorkspace: map[int64][]auth.HilbertDCDevice{
			123: {
				{ID: 456, WorkspaceID: 123, Name: "Device A", SN: "SN-A", DCDeviceTypeID: 77},
			},
		},
		deviceTypes: map[int64]*auth.HilbertDCDeviceType{
			77: {ID: 77, Name: "Type 77"},
		},
	}
	service := NewWorkspaceSyncService(db, testWorkspaceSyncHilbertConfig(), client)

	result, err := service.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if result.ResourceSync.CollectorUpsertedCount != 2 || result.ResourceSync.CollectorSkippedCount != 0 {
		t.Fatalf("unexpected resource summary: %#v", result.ResourceSync)
	}

	var collectorCount int
	if err := db.Get(&collectorCount, "SELECT COUNT(*) FROM data_collectors WHERE operator_id IN ('operator-a', 'internal-a')"); err != nil {
		t.Fatalf("count workspace people collectors: %v", err)
	}
	if collectorCount != 2 {
		t.Fatalf("collectorCount=%d want 2", collectorCount)
	}
}

func TestWorkspaceSyncServiceResourceConflictDoesNotRollbackWorkspace(t *testing.T) {
	db := newTestWorkspaceSyncDB(t)
	defer db.Close()

	if _, err := db.Exec(`INSERT INTO robots (device_id, metadata, deleted_at) VALUES ('456', '{}', NULL)`); err != nil {
		t.Fatalf("insert local robot: %v", err)
	}

	client := &fakeHilbertWorkspaceClient{
		workspaces: []auth.HilbertWorkspace{{ID: 123, Name: "Resource Workspace"}},
		devicesByWorkspace: map[int64][]auth.HilbertDCDevice{
			123: {
				{ID: 456, WorkspaceID: 123, Name: "Device A", SN: "SN-A", DCDeviceTypeID: 77},
			},
		},
		deviceTypes: map[int64]*auth.HilbertDCDeviceType{
			77: {ID: 77, Name: "Type 77"},
		},
	}
	service := NewWorkspaceSyncService(db, testWorkspaceSyncHilbertConfig(), client)

	result, err := service.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if result.ResourceSync == nil || len(result.ResourceSync.WorkspaceResults) != 1 {
		t.Fatalf("unexpected resource summary: %#v", result.ResourceSync)
	}
	if len(result.ResourceSync.WorkspaceResults[0].Errors) == 0 {
		t.Fatalf("expected resource conflict in summary")
	}

	var workspaceCount int
	if err := db.Get(&workspaceCount, "SELECT COUNT(*) FROM workspaces WHERE id = 123 AND source = ?", workspaceSourceHilbert); err != nil {
		t.Fatalf("count workspace: %v", err)
	}
	if workspaceCount != 1 {
		t.Fatalf("workspaceCount=%d want 1", workspaceCount)
	}
}

func TestWorkspaceSyncServiceResourceQueryFailureDoesNotReportRolledBackWrites(t *testing.T) {
	db := newTestWorkspaceSyncDB(t)
	defer db.Close()

	client := &fakeHilbertWorkspaceClient{
		workspaces: []auth.HilbertWorkspace{
			{ID: 123, Name: "Resource Workspace", Admins: []string{"collector-a"}},
		},
		accounts: map[string]*auth.HilbertAccount{
			"collector-a": {
				ID:               7,
				Code:             "collector-a",
				DisplayName:      "Collector A",
				Role:             "external_user",
				ExternalUserType: "data_supplier",
				Status:           "enabled",
			},
		},
		deviceErrByWorkspace: map[int64]error{
			123: errors.New("hilbert device query failed"),
		},
	}
	service := NewWorkspaceSyncService(db, testWorkspaceSyncHilbertConfig(), client)

	result, err := service.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if result.ResourceSync == nil || len(result.ResourceSync.WorkspaceResults) != 1 {
		t.Fatalf("unexpected resource summary: %#v", result.ResourceSync)
	}
	workspaceResult := result.ResourceSync.WorkspaceResults[0]
	if workspaceResult.CollectorUpsertedCount != 0 ||
		workspaceResult.RobotUpsertedCount != 0 {
		t.Fatalf("resource summary reported rolled-back writes: %#v", workspaceResult)
	}
	if len(workspaceResult.Errors) == 0 || workspaceResult.Errors[0].Code != "dc_device_query_failed" {
		t.Fatalf("expected dc device query failure: %#v", workspaceResult.Errors)
	}

	var workspaceCount int
	if err := db.Get(&workspaceCount, "SELECT COUNT(*) FROM workspaces WHERE id = 123 AND source = ?", workspaceSourceHilbert); err != nil {
		t.Fatalf("count workspace: %v", err)
	}
	if workspaceCount != 1 {
		t.Fatalf("workspaceCount=%d want 1", workspaceCount)
	}

	var collectorCount int
	if err := db.Get(&collectorCount, "SELECT COUNT(*) FROM data_collectors WHERE operator_id = 'collector-a'"); err != nil {
		t.Fatalf("count collector: %v", err)
	}
	if collectorCount != 0 {
		t.Fatalf("collectorCount=%d want rolled-back collector", collectorCount)
	}
}

func TestWorkspaceSyncServiceDoesNotCreateCompatFactories(t *testing.T) {
	db := newTestWorkspaceSyncDB(t)
	defer db.Close()

	client := &fakeHilbertWorkspaceClient{
		workspaces: []auth.HilbertWorkspace{
			{ID: 123, Name: "Shared Name"},
			{ID: 124, Name: "Shared Name"},
		},
	}
	service := NewWorkspaceSyncService(db, testWorkspaceSyncHilbertConfig(), client)

	result, err := service.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if result.ResourceSync == nil {
		t.Fatalf("unexpected resource summary: %#v", result.ResourceSync)
	}
}

type fakeHilbertWorkspaceClient struct {
	configured           bool
	workspaces           []auth.HilbertWorkspace
	listErr              error
	accounts             map[string]*auth.HilbertAccount
	devicesByWorkspace   map[int64][]auth.HilbertDCDevice
	deviceErrByWorkspace map[int64]error
	deviceTypes          map[int64]*auth.HilbertDCDeviceType
}

func (f *fakeHilbertWorkspaceClient) Configured() bool {
	if f.configured {
		return true
	}
	return len(f.workspaces) > 0 || f.listErr != nil
}

func (f *fakeHilbertWorkspaceClient) ServiceAuthConfigured() bool {
	return len(f.workspaces) > 0 || f.listErr != nil
}

func (f *fakeHilbertWorkspaceClient) ListAvailableWorkspaces(_ context.Context) ([]auth.HilbertWorkspace, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.workspaces, nil
}

func (f *fakeHilbertWorkspaceClient) QueryAccountByCode(_ context.Context, code string) (*auth.HilbertAccount, error) {
	account := f.accounts[code]
	if account == nil {
		return nil, nil
	}
	return account, nil
}

func (f *fakeHilbertWorkspaceClient) QueryDCDevices(_ context.Context, workspaceID int64) (*auth.HilbertDCDevicePage, error) {
	if err := f.deviceErrByWorkspace[workspaceID]; err != nil {
		return nil, err
	}
	return &auth.HilbertDCDevicePage{Records: f.devicesByWorkspace[workspaceID], PageNum: 1, PageSize: -1}, nil
}

func (f *fakeHilbertWorkspaceClient) QueryDCDeviceTypeByID(_ context.Context, id int64) (*auth.HilbertDCDeviceType, error) {
	deviceType := f.deviceTypes[id]
	if deviceType == nil {
		return nil, nil
	}
	return deviceType, nil
}

func testWorkspaceSyncHilbertConfig() *config.HilbertConfig {
	return &config.HilbertConfig{
		BaseURL:        "http://hilbert",
		TimeoutSeconds: 2,
		AccessKey:      "hilbert-ak",
		SecretKey:      "hilbert-sk",
	}
}

func TestWorkspaceResourceSyncAllowsCollectorAcrossWorkspaceBindings(t *testing.T) {
	db := newTestWorkspaceSyncDB(t)
	defer db.Close()
	for _, stmt := range []string{
		`INSERT INTO robots (id, device_id, workspace_id, status, metadata) VALUES (1, '456', 60, 'active', '{"source":"hilbert"}')`,
		`INSERT INTO data_collectors (id, name, operator_id, status, metadata) VALUES (1, 'Collector', 'collector-a', 'active', '{"source":"hilbert"}')`,
		`INSERT INTO workstations (id, robot_id, data_collector_id, workspace_id, is_current) VALUES (1, 1, 1, 60, TRUE)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed collector workspace change fixture: %v", err)
		}
	}
	tx, err := db.BeginTxx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	defer tx.Rollback()

	_, err = upsertHilbertDataCollector(context.Background(), tx, auth.HilbertAccount{
		ID:          7,
		Code:        "collector-a",
		DisplayName: "Collector A",
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("upsert collector across Workspaces: %v", err)
	}
}

func TestWorkspaceResourceSyncRejectsRobotWorkspaceChangeAcrossCurrentBinding(t *testing.T) {
	db := newTestWorkspaceSyncDB(t)
	defer db.Close()
	for _, stmt := range []string{
		`INSERT INTO robots (id, device_id, workspace_id, status, metadata) VALUES (1, '456', 60, 'active', '{"source":"hilbert"}')`,
		`INSERT INTO data_collectors (id, name, operator_id, status, metadata) VALUES (1, 'Collector', 'collector-a', 'active', '{"source":"hilbert"}')`,
		`INSERT INTO workstations (id, robot_id, data_collector_id, workspace_id, is_current) VALUES (1, 1, 1, 60, TRUE)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed robot workspace change fixture: %v", err)
		}
	}
	tx, err := db.BeginTxx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	defer tx.Rollback()

	_, err = upsertHilbertRobot(context.Background(), tx, auth.HilbertDCDevice{
		ID:             456,
		WorkspaceID:    61,
		DCDeviceTypeID: 77,
	}, &auth.HilbertDCDeviceType{ID: 77, Name: "Type 77"}, time.Now().UTC())
	if err == nil || !strings.Contains(err.Error(), "another workspace") {
		t.Fatalf("error=%v want workspace binding conflict", err)
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
			admins TEXT,
			members TEXT,
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
	for _, stmt := range []string{
		`CREATE TABLE robots (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			device_id TEXT UNIQUE,
			workspace_id INTEGER,
			device_type_id INTEGER,
			device_type TEXT,
			asset_id TEXT,
			status TEXT,
			metadata TEXT,
			created_at TIMESTAMP,
			updated_at TIMESTAMP,
			deleted_at TIMESTAMP
		)`,
		`CREATE TABLE data_collectors (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT,
			operator_id TEXT UNIQUE,
			email TEXT,
			password_hash TEXT,
			last_login_at TIMESTAMP,
			certification TEXT,
			status TEXT,
			metadata TEXT,
			created_at TIMESTAMP,
			updated_at TIMESTAMP,
			deleted_at TIMESTAMP
		)`,
		`CREATE TABLE workstations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			robot_id INTEGER NOT NULL,
			data_collector_id INTEGER NOT NULL,
			workspace_id INTEGER NOT NULL,
			is_current BOOLEAN NOT NULL DEFAULT TRUE,
			deleted_at TIMESTAMP
		)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			t.Fatalf("create resource table: %v", err)
		}
	}
	return db
}
