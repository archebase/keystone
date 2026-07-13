// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"context"
	"strings"
	"testing"
	"time"

	"archebase.com/keystone-edge/internal/auth"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

func TestDCPlanTaskGenerationCreatesMissingTasks(t *testing.T) {
	db := newTestDCPlanTaskGenerationDB(t)
	defer db.Close()
	plan := testTaskGenerationPlan(1001, 123, 3)
	seedTaskGenerationPlan(t, db, plan)
	seedTaskGenerationResources(t, db, plan)

	service := NewDCPlanTaskGenerationService(db)
	summary := service.GenerateForPlans(context.Background(), []auth.HilbertDCPlan{plan}, time.Now().UTC())

	if summary.CreatedCount != 3 || summary.BlockedCount != 0 || len(summary.Plans) != 1 {
		t.Fatalf("unexpected summary: %#v", summary)
	}
	if summary.Plans[0].Status != dcPlanTaskGenerationStatusCreated ||
		summary.Plans[0].ExistingTaskCount != 0 ||
		summary.Plans[0].CreatedTaskCount != 3 {
		t.Fatalf("unexpected plan summary: %#v", summary.Plans[0])
	}
	assertTaskGenerationCount(t, db, plan.ID, 3)
	assertTaskGenerationDoesNotWriteOrderBatch(t, db)
	assertTaskGenerationDoesNotWriteLegacyProductionMetadata(t, db)
}

func TestDCPlanTaskGenerationIsIdempotent(t *testing.T) {
	db := newTestDCPlanTaskGenerationDB(t)
	defer db.Close()
	plan := testTaskGenerationPlan(1001, 123, 2)
	seedTaskGenerationPlan(t, db, plan)
	seedTaskGenerationResources(t, db, plan)
	service := NewDCPlanTaskGenerationService(db)

	first := service.GenerateForPlans(context.Background(), []auth.HilbertDCPlan{plan}, time.Now().UTC())
	second := service.GenerateForPlans(context.Background(), []auth.HilbertDCPlan{plan}, time.Now().UTC())

	if first.CreatedCount != 2 {
		t.Fatalf("first CreatedCount=%d want 2", first.CreatedCount)
	}
	if second.CreatedCount != 0 || second.Plans[0].Status != dcPlanTaskGenerationStatusNoop || second.Plans[0].ExistingTaskCount != 2 {
		t.Fatalf("unexpected second summary: %#v", second)
	}
	assertTaskGenerationCount(t, db, plan.ID, 2)
}

func TestDCPlanTaskGenerationHandlesTargetIncreaseAndDecrease(t *testing.T) {
	db := newTestDCPlanTaskGenerationDB(t)
	defer db.Close()
	plan := testTaskGenerationPlan(1001, 123, 2)
	seedTaskGenerationPlan(t, db, plan)
	seedTaskGenerationResources(t, db, plan)
	service := NewDCPlanTaskGenerationService(db)

	service.GenerateForPlans(context.Background(), []auth.HilbertDCPlan{plan}, time.Now().UTC())
	plan.TargetCount = 5
	updateTaskGenerationPlanTarget(t, db, plan.ID, 5)
	increased := service.GenerateForPlans(context.Background(), []auth.HilbertDCPlan{plan}, time.Now().UTC())
	if increased.CreatedCount != 3 || increased.Plans[0].ExistingTaskCount != 2 {
		t.Fatalf("unexpected increased summary: %#v", increased)
	}

	plan.TargetCount = 1
	updateTaskGenerationPlanTarget(t, db, plan.ID, 1)
	decreased := service.GenerateForPlans(context.Background(), []auth.HilbertDCPlan{plan}, time.Now().UTC())
	if decreased.CreatedCount != 0 || decreased.Plans[0].ExistingTaskCount != 5 || decreased.Plans[0].TargetCount != 1 {
		t.Fatalf("unexpected decreased summary: %#v", decreased)
	}
	assertTaskGenerationCount(t, db, plan.ID, 5)
}

func TestDCPlanTaskGenerationAllowsMultiplePlansForSameRobot(t *testing.T) {
	db := newTestDCPlanTaskGenerationDB(t)
	defer db.Close()
	planA := testTaskGenerationPlan(1001, 123, 1)
	planB := testTaskGenerationPlan(1002, 123, 3)
	planB.Operator = "collector-b"
	planB.DCType = "ego"
	seedTaskGenerationPlan(t, db, planA)
	seedTaskGenerationPlan(t, db, planB)
	seedTaskGenerationResources(t, db, planA)
	seedTaskGenerationCollector(t, db, 8, planB.WorkspaceID, "Collector B", planB.Operator)
	service := NewDCPlanTaskGenerationService(db)

	summary := service.GenerateForPlans(context.Background(), []auth.HilbertDCPlan{planA, planB}, time.Now().UTC())

	if summary.CreatedCount != 4 || summary.BlockedCount != 0 || len(summary.Plans) != 2 {
		t.Fatalf("unexpected summary: %#v", summary)
	}
	assertTaskGenerationCount(t, db, planA.ID, 1)
	assertTaskGenerationCount(t, db, planB.ID, 3)
	assertTaskGenerationWorkstationCollector(t, db, planA.ID, "collector-a")
	assertTaskGenerationWorkstationCollector(t, db, planB.ID, "collector-b")
}

func TestDCPlanTaskGenerationBlocksMissingResources(t *testing.T) {
	db := newTestDCPlanTaskGenerationDB(t)
	defer db.Close()
	plan := testTaskGenerationPlan(1001, 123, 2)
	seedTaskGenerationPlan(t, db, plan)

	service := NewDCPlanTaskGenerationService(db)
	summary := service.GenerateForPlans(context.Background(), []auth.HilbertDCPlan{plan}, time.Now().UTC())

	if summary.CreatedCount != 0 || summary.BlockedCount != 1 {
		t.Fatalf("unexpected summary: %#v", summary)
	}
	if summary.Plans[0].Status != dcPlanTaskGenerationStatusBlocked || summary.Plans[0].Reason != "collector_missing" {
		t.Fatalf("unexpected plan summary: %#v", summary.Plans[0])
	}
	assertTaskGenerationCount(t, db, plan.ID, 0)
}

func testTaskGenerationPlan(id int64, workspaceID int64, targetCount int64) auth.HilbertDCPlan {
	plan := testHilbertDCPlan(id, workspaceID, "Plan")
	plan.TargetCount = targetCount
	plan.DCFactoryID = 321
	plan.DCDeviceID = 456
	plan.Operator = "collector-a"
	return plan
}

func seedTaskGenerationPlan(t *testing.T, db *sqlx.DB, plan auth.HilbertDCPlan) {
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

func seedTaskGenerationResources(t *testing.T, db *sqlx.DB, plan auth.HilbertDCPlan) {
	t.Helper()
	seedTaskGenerationCollector(t, db, 7, plan.WorkspaceID, "Collector A", plan.Operator)
	if _, err := db.Exec(`
		INSERT INTO robots (id, device_id, workspace_id, status, metadata, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, 9, "456", plan.WorkspaceID, "active", `{"source":"hilbert","hilbert_workspace_id":123}`, time.Now().UTC(), time.Now().UTC()); err != nil {
		t.Fatalf("seed robot: %v", err)
	}
}

func seedTaskGenerationCollector(t *testing.T, db *sqlx.DB, id int64, workspaceID int64, name string, operatorID string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO data_collectors (id, organization_id, name, operator_id, status, metadata, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, id, workspaceID, name, operatorID, "active", `{"source":"hilbert","hilbert_workspace_id":123}`, time.Now().UTC(), time.Now().UTC()); err != nil {
		t.Fatalf("seed collector: %v", err)
	}
}

func updateTaskGenerationPlanTarget(t *testing.T, db *sqlx.DB, id int64, targetCount int64) {
	t.Helper()
	if _, err := db.Exec("UPDATE dc_plan SET target_count = ? WHERE id = ?", targetCount, id); err != nil {
		t.Fatalf("update target_count: %v", err)
	}
}

func assertTaskGenerationCount(t *testing.T, db *sqlx.DB, dcPlanID int64, want int) {
	t.Helper()
	var count int
	if err := db.Get(&count, "SELECT COUNT(*) FROM tasks WHERE dc_plan_id = ? AND deleted_at IS NULL", dcPlanID); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if count != want {
		t.Fatalf("task count=%d want %d", count, want)
	}
}

func assertTaskGenerationWorkstationCollector(t *testing.T, db *sqlx.DB, dcPlanID int64, wantOperatorID string) {
	t.Helper()
	var got string
	if err := db.Get(&got, `
		SELECT ws.collector_operator_id
		FROM tasks t
		JOIN workstations ws ON ws.id = t.workstation_id
		WHERE t.dc_plan_id = ? AND t.deleted_at IS NULL
		LIMIT 1
	`, dcPlanID); err != nil {
		t.Fatalf("query task workstation collector: %v", err)
	}
	if got != wantOperatorID {
		t.Fatalf("collector_operator_id=%q want %q", got, wantOperatorID)
	}
}

func assertTaskGenerationDoesNotWriteOrderBatch(t *testing.T, db *sqlx.DB) {
	t.Helper()
	for _, table := range []string{"orders", "batches"} {
		var count int
		if err := db.Get(&count, "SELECT COUNT(*) FROM "+table); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("%s count=%d want 0", table, count)
		}
	}
}

func assertTaskGenerationDoesNotWriteLegacyProductionMetadata(t *testing.T, db *sqlx.DB) {
	t.Helper()
	var task struct {
		Metadata string `db:"metadata"`
	}
	if err := db.Get(&task, `
		SELECT metadata
		FROM tasks
		LIMIT 1
	`); err != nil {
		t.Fatalf("query generated task: %v", err)
	}
	for _, value := range []string{`"workspace_id":123`, `"dc_plan_id":1001`, `"execution_config"`} {
		if !strings.Contains(task.Metadata, value) {
			t.Fatalf("task metadata %q missing %q", task.Metadata, value)
		}
	}
}

func newTestDCPlanTaskGenerationDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if _, err := db.Exec(`
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
		CREATE TABLE data_collectors (
			id INTEGER PRIMARY KEY,
			organization_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			operator_id TEXT NOT NULL,
			status TEXT,
			metadata TEXT,
			created_at TIMESTAMP,
			updated_at TIMESTAMP,
			deleted_at TIMESTAMP
		);
		CREATE TABLE robots (
			id INTEGER PRIMARY KEY,
			device_id TEXT NOT NULL,
			workspace_id INTEGER NOT NULL DEFAULT 0,
			status TEXT,
			metadata TEXT,
			created_at TIMESTAMP,
			updated_at TIMESTAMP,
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
			organization_id INTEGER NOT NULL,
			name TEXT,
			status TEXT,
			metadata TEXT,
			created_at TIMESTAMP,
			updated_at TIMESTAMP,
			deleted_at TIMESTAMP,
			is_current BOOLEAN NOT NULL DEFAULT TRUE
		);
		CREATE TABLE orders (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			organization_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			target_count INTEGER NOT NULL,
			priority TEXT,
			status TEXT,
			metadata TEXT,
			created_at TIMESTAMP,
			updated_at TIMESTAMP,
			deleted_at TIMESTAMP,
			UNIQUE(organization_id, name)
		);
		CREATE TABLE batches (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			batch_id TEXT NOT NULL,
			order_id INTEGER NOT NULL,
			workstation_id INTEGER NOT NULL,
			organization_id INTEGER NOT NULL,
			name TEXT,
			status TEXT,
			episode_count INTEGER,
			metadata TEXT,
			created_at TIMESTAMP,
			updated_at TIMESTAMP,
			deleted_at TIMESTAMP
		);
		CREATE TABLE tasks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			task_id TEXT NOT NULL,
			batch_id INTEGER,
			order_id INTEGER,
			workstation_id INTEGER,
			batch_name TEXT,
			organization_id INTEGER,
			dc_plan_id INTEGER,
			local_dc_plan_id INTEGER,
			status TEXT,
			assigned_at TIMESTAMP,
			metadata TEXT,
			created_at TIMESTAMP,
			updated_at TIMESTAMP,
			deleted_at TIMESTAMP
		);
	`); err != nil {
		db.Close()
		t.Fatalf("create tables: %v", err)
	}
	return db
}
