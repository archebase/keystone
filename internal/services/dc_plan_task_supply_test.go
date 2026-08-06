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
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

func TestDCPlanTaskSupplyCreatesAndReusesSinglePendingTask(t *testing.T) {
	db := newTestDCPlanTaskSupplyDB(t)
	defer db.Close()

	plan := testTaskSupplyPlan(1001, 123, 10)
	seedTaskSupplyPlan(t, db, plan)
	seedTaskSupplyResources(t, db, plan)
	workstationID := seedCurrentTaskSupplyWorkstation(t, db, plan.WorkspaceID)

	service := NewDCPlanTaskSupplyService(db)
	now := time.Date(2026, 7, 21, 14, 30, 0, 0, time.UTC)
	first, err := service.EnsureNextTask(context.Background(), plan.ID, workstationID, now)
	if err != nil {
		t.Fatalf("first EnsureNextTask() error = %v", err)
	}
	second, err := service.EnsureNextTask(context.Background(), plan.ID, workstationID, now.Add(time.Second))
	if err != nil {
		t.Fatalf("second EnsureNextTask() error = %v", err)
	}

	if !first.Created || second.Created {
		t.Fatalf("created flags first=%t second=%t", first.Created, second.Created)
	}
	if first.Task.ID <= 0 || second.Task.ID != first.Task.ID || second.Task.TaskID != first.Task.TaskID {
		t.Fatalf("first=%#v second=%#v", first, second)
	}

	var pendingCount int
	if err := db.Get(&pendingCount, `
		SELECT COUNT(*)
		FROM tasks
		WHERE dc_plan_id = ? AND status = 'pending' AND deleted_at IS NULL
	`, plan.ID); err != nil {
		t.Fatalf("count pending tasks: %v", err)
	}
	if pendingCount != 1 {
		t.Fatalf("pendingCount=%d want 1", pendingCount)
	}
}

func TestDCPlanTaskSupplyCollapsesPrecreatedPendingTasks(t *testing.T) {
	db := newTestDCPlanTaskSupplyDB(t)
	defer db.Close()

	plan := testTaskSupplyPlan(1001, 123, 10)
	seedTaskSupplyPlan(t, db, plan)
	seedTaskSupplyResources(t, db, plan)
	workstationID := seedCurrentTaskSupplyWorkstation(t, db, plan.WorkspaceID)
	if _, err := db.Exec(`
		INSERT INTO tasks (task_id, workstation_id, organization_id, dc_plan_id, status) VALUES
			('task-old-1', ?, ?, ?, 'pending'),
			('task-old-2', ?, ?, ?, 'pending'),
			('task-old-3', ?, ?, ?, 'pending')
	`, workstationID, plan.WorkspaceID, plan.ID, workstationID, plan.WorkspaceID, plan.ID, workstationID, plan.WorkspaceID, plan.ID); err != nil {
		t.Fatalf("seed pending tasks: %v", err)
	}

	result, err := NewDCPlanTaskSupplyService(db).EnsureNextTask(
		context.Background(), plan.ID, workstationID, time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("EnsureNextTask() error = %v", err)
	}
	if result.Created || result.Task.TaskID != "task-old-1" {
		t.Fatalf("unexpected result: %#v", result)
	}

	var pendingCount int
	if err := db.Get(&pendingCount, "SELECT COUNT(*) FROM tasks WHERE dc_plan_id = ? AND status = 'pending'", plan.ID); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	var cancelledCount int
	if err := db.Get(&cancelledCount, "SELECT COUNT(*) FROM tasks WHERE dc_plan_id = ? AND status = 'cancelled'", plan.ID); err != nil {
		t.Fatalf("count cancelled: %v", err)
	}
	if pendingCount != 1 || cancelledCount != 2 {
		t.Fatalf("pending=%d cancelled=%d want 1/2", pendingCount, cancelledCount)
	}
}

func TestDCPlanTaskSupplyPreservesEgoPortalStereoPendingPool(t *testing.T) {
	db := newTestDCPlanTaskSupplyDB(t)
	defer db.Close()

	plan := testTaskSupplyPlan(1001, 123, 10)
	seedTaskSupplyPlan(t, db, plan)
	seedTaskSupplyResources(t, db, plan)
	if _, err := db.Exec("UPDATE robots SET device_type = ? WHERE id = 9", egoPortalStereoDeviceType); err != nil {
		t.Fatalf("mark stereo robot: %v", err)
	}
	workstationID := seedCurrentTaskSupplyWorkstation(t, db, plan.WorkspaceID)
	if _, err := db.Exec(`
		INSERT INTO tasks (task_id, workstation_id, organization_id, dc_plan_id, status) VALUES
			('task-pool-1', ?, ?, ?, 'pending'),
			('task-pool-2', ?, ?, ?, 'pending'),
			('task-pool-3', ?, ?, ?, 'pending')
	`, workstationID, plan.WorkspaceID, plan.ID, workstationID, plan.WorkspaceID, plan.ID,
		workstationID, plan.WorkspaceID, plan.ID); err != nil {
		t.Fatalf("seed pending pool: %v", err)
	}

	result, err := NewDCPlanTaskSupplyService(db).EnsureNextTask(
		context.Background(), plan.ID, workstationID, time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("EnsureNextTask() error = %v", err)
	}
	if result.Created || result.Task.TaskID != "task-pool-1" {
		t.Fatalf("unexpected result: %#v", result)
	}

	var pendingCount int
	if err := db.Get(&pendingCount, "SELECT COUNT(*) FROM tasks WHERE dc_plan_id = ? AND status = 'pending'", plan.ID); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	var cancelledCount int
	if err := db.Get(&cancelledCount, "SELECT COUNT(*) FROM tasks WHERE dc_plan_id = ? AND status = 'cancelled'", plan.ID); err != nil {
		t.Fatalf("count cancelled: %v", err)
	}
	if pendingCount != 3 || cancelledCount != 0 {
		t.Fatalf("pending=%d cancelled=%d want 3/0", pendingCount, cancelledCount)
	}
}

func TestDCPlanTaskSupplyPreservesEgoPortalStereoPoolWhenNextTaskIsBlocked(t *testing.T) {
	tests := []struct {
		name         string
		targetCount  int64
		curCount     int64
		activeStatus string
		wantErr      error
	}{
		{name: "target reached", targetCount: 2, curCount: 2, wantErr: ErrDCPlanTaskSupplyTargetReached},
		{name: "active task", targetCount: 10, activeStatus: "ready", wantErr: ErrDCPlanTaskSupplyActiveTask},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := newTestDCPlanTaskSupplyDB(t)
			defer db.Close()

			plan := testTaskSupplyPlan(1001, 123, test.targetCount)
			plan.CurCount = test.curCount
			seedTaskSupplyPlan(t, db, plan)
			seedTaskSupplyResources(t, db, plan)
			if _, err := db.Exec("UPDATE robots SET device_type = ? WHERE id = 9", egoPortalStereoDeviceType); err != nil {
				t.Fatalf("mark stereo robot: %v", err)
			}
			workstationID := seedCurrentTaskSupplyWorkstation(t, db, plan.WorkspaceID)
			if _, err := db.Exec(`
				INSERT INTO tasks (task_id, workstation_id, organization_id, dc_plan_id, status) VALUES
					('task-pool-1', ?, ?, ?, 'pending'),
					('task-pool-2', ?, ?, ?, 'pending')
			`, workstationID, plan.WorkspaceID, plan.ID, workstationID, plan.WorkspaceID, plan.ID); err != nil {
				t.Fatalf("seed pending pool: %v", err)
			}
			if test.activeStatus != "" {
				if _, err := db.Exec(`
					INSERT INTO tasks (task_id, workstation_id, organization_id, dc_plan_id, status)
					VALUES ('task-active', ?, ?, ?, ?)
				`, workstationID, plan.WorkspaceID, plan.ID, test.activeStatus); err != nil {
					t.Fatalf("seed active task: %v", err)
				}
			}

			_, err := NewDCPlanTaskSupplyService(db).EnsureNextTask(
				context.Background(), plan.ID, workstationID, time.Now().UTC(),
			)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("EnsureNextTask() error = %v, want %v", err, test.wantErr)
			}
			var pendingCount int
			if err := db.Get(&pendingCount, `
				SELECT COUNT(*) FROM tasks WHERE dc_plan_id = ? AND status = 'pending'
			`, plan.ID); err != nil {
				t.Fatalf("count pending tasks: %v", err)
			}
			if pendingCount != 2 {
				t.Fatalf("pending count=%d want 2", pendingCount)
			}
		})
	}
}

func TestDCPlanTaskSupplyMaintainsEgoPortalStereoPendingPool(t *testing.T) {
	db := newTestDCPlanTaskSupplyDB(t)
	defer db.Close()

	plan := testTaskSupplyPlan(1001, 123, 120)
	plan.CurCount = 10
	seedTaskSupplyPlan(t, db, plan)
	seedTaskSupplyResources(t, db, plan)
	if _, err := db.Exec("UPDATE robots SET device_type = ? WHERE id = 9", egoPortalStereoDeviceType); err != nil {
		t.Fatalf("mark stereo robot: %v", err)
	}
	seedCurrentTaskSupplyWorkstation(t, db, plan.WorkspaceID)

	service := NewDCPlanTaskSupplyService(db)
	first, err := service.EnsureEgoPortalPendingPool(context.Background(), plan.ID, time.Now().UTC())
	if err != nil {
		t.Fatalf("first EnsureEgoPortalPendingPool() error = %v", err)
	}
	second, err := service.EnsureEgoPortalPendingPool(context.Background(), plan.ID, time.Now().UTC())
	if err != nil {
		t.Fatalf("second EnsureEgoPortalPendingPool() error = %v", err)
	}
	wantCount := int(plan.TargetCount - plan.CurCount)
	if !first.Enabled || first.DesiredCount != wantCount || first.CreatedCount != wantCount {
		t.Fatalf("unexpected first result: %#v", first)
	}
	if !second.Enabled || second.CreatedCount != 0 || second.PendingCount != wantCount {
		t.Fatalf("unexpected second result: %#v", second)
	}
}

func TestDCPlanTaskSupplyMaintainsEgoPortalLitePendingPool(t *testing.T) {
	db := newTestDCPlanTaskSupplyDB(t)
	defer db.Close()

	plan := testTaskSupplyPlan(1001, 123, 4)
	plan.CurCount = 1
	seedTaskSupplyPlan(t, db, plan)
	seedTaskSupplyResources(t, db, plan)
	if _, err := db.Exec("UPDATE robots SET device_type = ? WHERE id = 9", egoPortalLiteDeviceType); err != nil {
		t.Fatalf("mark lite robot: %v", err)
	}
	workstationID := seedCurrentTaskSupplyWorkstation(t, db, plan.WorkspaceID)

	service := NewDCPlanTaskSupplyService(db)
	first, err := service.EnsureEgoPortalPendingPool(context.Background(), plan.ID, time.Now().UTC())
	if err != nil {
		t.Fatalf("first EnsureEgoPortalPendingPool() error = %v", err)
	}
	if !first.Enabled || first.DesiredCount != 3 || first.CreatedCount != 3 || first.PendingCount != 3 {
		t.Fatalf("unexpected first result: %#v", first)
	}

	next, err := service.EnsureNextTask(context.Background(), plan.ID, workstationID, time.Now().UTC())
	if err != nil {
		t.Fatalf("EnsureNextTask() error = %v", err)
	}
	if next.Created {
		t.Fatalf("EnsureNextTask() created a task instead of reusing the Lite pool: %#v", next)
	}

	var pendingCount int
	if err := db.Get(&pendingCount, `
		SELECT COUNT(*) FROM tasks WHERE dc_plan_id = ? AND status = 'pending'
	`, plan.ID); err != nil {
		t.Fatalf("count pending tasks: %v", err)
	}
	if pendingCount != 3 {
		t.Fatalf("pendingCount=%d want 3", pendingCount)
	}
}

func TestDCPlanTaskSupplyCancelsPendingWhenTargetReached(t *testing.T) {
	db := newTestDCPlanTaskSupplyDB(t)
	defer db.Close()

	plan := testTaskSupplyPlan(1001, 123, 1)
	seedTaskSupplyPlan(t, db, plan)
	seedTaskSupplyResources(t, db, plan)
	workstationID := seedCurrentTaskSupplyWorkstation(t, db, plan.WorkspaceID)
	if _, err := db.Exec(`
		INSERT INTO tasks (task_id, workstation_id, organization_id, dc_plan_id, status) VALUES
			('task-completed', ?, ?, ?, 'completed'),
			('task-pending', ?, ?, ?, 'pending')
	`, workstationID, plan.WorkspaceID, plan.ID, workstationID, plan.WorkspaceID, plan.ID); err != nil {
		t.Fatalf("seed tasks: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO episodes (episode_id, task_id, dc_plan_id, duration_sec, cloud_synced)
		SELECT 'episode-completed', id, dc_plan_id, 30, FALSE
		FROM tasks
		WHERE task_id = 'task-completed'
	`); err != nil {
		t.Fatalf("seed completed episode: %v", err)
	}

	_, err := NewDCPlanTaskSupplyService(db).EnsureNextTask(
		context.Background(), plan.ID, workstationID, time.Now().UTC(),
	)
	if !errors.Is(err, ErrDCPlanTaskSupplyTargetReached) {
		t.Fatalf("EnsureNextTask() error = %v, want target reached", err)
	}

	var status string
	if err := db.Get(&status, "SELECT status FROM tasks WHERE task_id = 'task-pending'"); err != nil {
		t.Fatalf("query pending task: %v", err)
	}
	if status != "cancelled" {
		t.Fatalf("pending status=%q want cancelled", status)
	}
}

func TestDCPlanTaskSupplyAddsUnsyncedProgressToCloudBaseline(t *testing.T) {
	db := newTestDCPlanTaskSupplyDB(t)
	defer db.Close()

	plan := testTaskSupplyPlan(1001, 123, 3)
	plan.CurCount = 2
	seedTaskSupplyPlan(t, db, plan)
	seedTaskSupplyResources(t, db, plan)
	workstationID := seedCurrentTaskSupplyWorkstation(t, db, plan.WorkspaceID)
	if _, err := db.Exec(`
		INSERT INTO tasks (id, task_id, workstation_id, organization_id, dc_plan_id, status)
		VALUES (1, 'task-local', ?, ?, ?, 'completed')
	`, workstationID, plan.WorkspaceID, plan.ID); err != nil {
		t.Fatalf("seed local task: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO episodes (episode_id, task_id, dc_plan_id, duration_sec, cloud_synced)
		VALUES ('episode-local', 1, ?, 30, FALSE)
	`, plan.ID); err != nil {
		t.Fatalf("seed unsynced episode: %v", err)
	}

	_, err := NewDCPlanTaskSupplyService(db).EnsureNextTask(
		context.Background(), plan.ID, workstationID, time.Now().UTC(),
	)
	if !errors.Is(err, ErrDCPlanTaskSupplyTargetReached) {
		t.Fatalf("EnsureNextTask() error = %v, want target reached", err)
	}
}

func TestDCPlanTaskSupplyIgnoresFailedEpisodeForTarget(t *testing.T) {
	db := newTestDCPlanTaskSupplyDB(t)
	defer db.Close()

	plan := testTaskSupplyPlan(1001, 123, 1)
	seedTaskSupplyPlan(t, db, plan)
	seedTaskSupplyResources(t, db, plan)
	workstationID := seedCurrentTaskSupplyWorkstation(t, db, plan.WorkspaceID)
	if _, err := db.Exec(`
		INSERT INTO tasks (id, task_id, workstation_id, organization_id, dc_plan_id, status)
		VALUES (1, 'task-failed', ?, ?, ?, 'completed')
	`, workstationID, plan.WorkspaceID, plan.ID); err != nil {
		t.Fatalf("seed failed task: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO episodes (episode_id, task_id, dc_plan_id, duration_sec, cloud_synced, qa_status)
		VALUES ('episode-failed', 1, ?, 30, FALSE, 'failed')
	`, plan.ID); err != nil {
		t.Fatalf("seed failed episode: %v", err)
	}

	result, err := NewDCPlanTaskSupplyService(db).EnsureNextTask(
		context.Background(), plan.ID, workstationID, time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("EnsureNextTask() error = %v", err)
	}
	if !result.Created || result.Task.Status != "pending" {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestDCPlanTaskSupplyBlocksReadyButAllowsUploadingPredecessor(t *testing.T) {
	t.Run("ready blocks another task", func(t *testing.T) {
		db := newTestDCPlanTaskSupplyDB(t)
		defer db.Close()
		plan := testTaskSupplyPlan(1001, 123, 2)
		seedTaskSupplyPlan(t, db, plan)
		seedTaskSupplyResources(t, db, plan)
		workstationID := seedCurrentTaskSupplyWorkstation(t, db, plan.WorkspaceID)
		if _, err := db.Exec(`
			INSERT INTO tasks (task_id, workstation_id, organization_id, dc_plan_id, status) VALUES
				('task-ready', ?, ?, ?, 'ready'),
				('task-old-pending-1', ?, ?, ?, 'pending'),
				('task-old-pending-2', ?, ?, ?, 'pending')
		`, workstationID, plan.WorkspaceID, plan.ID, workstationID, plan.WorkspaceID, plan.ID,
			workstationID, plan.WorkspaceID, plan.ID); err != nil {
			t.Fatalf("seed ready task: %v", err)
		}

		_, err := NewDCPlanTaskSupplyService(db).EnsureNextTask(
			context.Background(), plan.ID, workstationID, time.Now().UTC(),
		)
		if !errors.Is(err, ErrDCPlanTaskSupplyActiveTask) {
			t.Fatalf("EnsureNextTask() error = %v, want active task", err)
		}
		var pendingCount int
		if err := db.Get(&pendingCount, `
			SELECT COUNT(*) FROM tasks WHERE dc_plan_id = ? AND status = 'pending'
		`, plan.ID); err != nil {
			t.Fatalf("count pending tasks: %v", err)
		}
		if pendingCount != 0 {
			t.Fatalf("pendingCount=%d want 0 while a task is active", pendingCount)
		}
	})

	t.Run("uploading allows next task", func(t *testing.T) {
		db := newTestDCPlanTaskSupplyDB(t)
		defer db.Close()
		plan := testTaskSupplyPlan(1001, 123, 2)
		seedTaskSupplyPlan(t, db, plan)
		seedTaskSupplyResources(t, db, plan)
		workstationID := seedCurrentTaskSupplyWorkstation(t, db, plan.WorkspaceID)
		if _, err := db.Exec(`
			INSERT INTO tasks (task_id, workstation_id, organization_id, dc_plan_id, status)
			VALUES ('task-uploading', ?, ?, ?, 'uploading')
		`, workstationID, plan.WorkspaceID, plan.ID); err != nil {
			t.Fatalf("seed uploading task: %v", err)
		}

		result, err := NewDCPlanTaskSupplyService(db).EnsureNextTask(
			context.Background(), plan.ID, workstationID, time.Now().UTC(),
		)
		if err != nil {
			t.Fatalf("EnsureNextTask() error = %v", err)
		}
		if !result.Created || result.Task.Status != "pending" {
			t.Fatalf("unexpected result: %#v", result)
		}
	})
}

func testTaskSupplyPlan(id int64, workspaceID int64, targetCount int64) auth.HilbertDCPlan {
	return auth.HilbertDCPlan{
		ID:                   id,
		WorkspaceID:          workspaceID,
		Name:                 "Plan",
		DCFactoryID:          321,
		DCServiceProviderID:  12,
		Operator:             "collector-a",
		DCProjectID:          13,
		DCProjectDescription: "Collect objects in the kitchen",
		DCTaskID:             14,
		DCTaskDescription:    "Follow the collection instructions",
		DCDeviceID:           456,
		DCType:               "ego",
		DCDate:               "2026-07-21",
		TargetCount:          targetCount,
		TargetDuration:       3600,
		CreatedBy:            "planner",
		CreatedTime:          time.Date(2026, 7, 21, 1, 2, 3, 0, time.UTC),
	}
}

func seedTaskSupplyPlan(t *testing.T, db *sqlx.DB, plan auth.HilbertDCPlan) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO workspaces (id, admins, members, deleted_at)
		VALUES (?, '[]', '["collector-a"]', NULL)
	`, plan.WorkspaceID); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO dc_plan (
			id, workspace_id, name, operator, dc_project_description, dc_task_description, dc_device_id, dc_type,
			target_count, cur_count, target_duration, deleted_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)
	`, plan.ID, plan.WorkspaceID, plan.Name, plan.Operator, plan.DCProjectDescription, plan.DCTaskDescription, plan.DCDeviceID,
		plan.DCType, plan.TargetCount, plan.CurCount, plan.TargetDuration); err != nil {
		t.Fatalf("seed dc_plan: %v", err)
	}
}

func seedTaskSupplyResources(t *testing.T, db *sqlx.DB, plan auth.HilbertDCPlan) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO data_collectors (id, name, operator_id, status, deleted_at)
		VALUES (7, 'Collector A', ?, 'active', NULL)
	`, plan.Operator); err != nil {
		t.Fatalf("seed collector: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO robots (id, device_id, workspace_id, device_type, status, deleted_at)
		VALUES (9, ?, ?, 'Axon', 'active', NULL)
	`, plan.DCDeviceID, plan.WorkspaceID); err != nil {
		t.Fatalf("seed robot: %v", err)
	}
}

func newTestDCPlanTaskSupplyDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE workspaces (
			id INTEGER PRIMARY KEY,
			admins TEXT NOT NULL,
			members TEXT NOT NULL,
			deleted_at TIMESTAMP
		);
		CREATE TABLE dc_plan (
			id INTEGER PRIMARY KEY,
			workspace_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			operator TEXT NOT NULL,
			dc_project_description TEXT,
			dc_task_description TEXT,
			dc_device_id INTEGER NOT NULL,
			dc_type TEXT NOT NULL,
			target_count INTEGER NOT NULL,
			cur_count INTEGER NOT NULL DEFAULT 0,
			target_duration INTEGER NOT NULL,
			deleted_at TIMESTAMP
		);
		CREATE TABLE data_collectors (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			operator_id TEXT NOT NULL,
			status TEXT,
			deleted_at TIMESTAMP
		);
		CREATE TABLE robots (
			id INTEGER PRIMARY KEY,
			device_id TEXT NOT NULL,
			workspace_id INTEGER NOT NULL,
			device_type TEXT,
			status TEXT,
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
			status TEXT,
			is_current BOOLEAN NOT NULL DEFAULT TRUE,
			deleted_at TIMESTAMP
		);
		CREATE TABLE tasks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			task_id TEXT NOT NULL,
			workstation_id INTEGER,
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
		CREATE TABLE episodes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			episode_id TEXT NOT NULL,
			task_id INTEGER NOT NULL,
			dc_plan_id INTEGER,
			duration_sec REAL,
			cloud_synced BOOLEAN NOT NULL DEFAULT FALSE,
			qa_status TEXT NOT NULL DEFAULT 'pending_qa',
			deleted_at TIMESTAMP
		);
	`); err != nil {
		db.Close()
		t.Fatalf("create tables: %v", err)
	}
	return db
}

func seedCurrentTaskSupplyWorkstation(t *testing.T, db *sqlx.DB, workspaceID int64) int64 {
	t.Helper()
	result, err := db.Exec(`
		INSERT INTO workstations (
			robot_id, robot_name, robot_serial, data_collector_id, collector_name,
			collector_operator_id, workspace_id, name, status, is_current
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, 9, "456", "456", 7, "Collector A", "collector-a", workspaceID, "Station A", "active", true)
	if err != nil {
		t.Fatalf("seed workstation: %v", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("workstation id: %v", err)
	}
	return id
}
