// SPDX-FileCopyrightText: 2026 ArcheBase
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"context"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
)

func seedTrimPendingTasks(t *testing.T, db *sqlx.DB, planID int64, workstationID int64, workspaceID int64, count int) {
	t.Helper()
	for index := 0; index < count; index++ {
		if _, err := db.Exec(`
			INSERT INTO tasks (task_id, workstation_id, organization_id, dc_plan_id, status, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'pending', ?, ?)
		`, "task-trim-"+string(rune('a'+index)), workstationID, workspaceID, planID, time.Now().UTC(), time.Now().UTC()); err != nil {
			t.Fatalf("seed pending task: %v", err)
		}
	}
}

func TestTrimPendingTasksCancelsTasksAboveReducedTarget(t *testing.T) {
	db := newTestDCPlanTaskSupplyDB(t)
	defer db.Close()

	plan := testTaskSupplyPlan(1001, 123, 5)
	seedTaskSupplyPlan(t, db, plan)
	seedTaskSupplyResources(t, db, plan)
	workstationID := seedCurrentTaskSupplyWorkstation(t, db, plan.WorkspaceID)
	seedTrimPendingTasks(t, db, plan.ID, workstationID, plan.WorkspaceID, 5)

	if _, err := db.Exec(`UPDATE dc_plan SET target_count = 2 WHERE id = ?`, plan.ID); err != nil {
		t.Fatalf("reduce plan target: %v", err)
	}

	cancelled, err := NewDCPlanTaskSupplyService(db).TrimPendingTasks(context.Background(), plan.ID, time.Now().UTC())
	if err != nil {
		t.Fatalf("TrimPendingTasks() error = %v", err)
	}
	if cancelled != 3 {
		t.Fatalf("cancelled=%d want=3", cancelled)
	}

	var pendingCount, cancelledCount int
	if err := db.Get(&pendingCount, `SELECT COUNT(*) FROM tasks WHERE dc_plan_id = ? AND status = 'pending' AND deleted_at IS NULL`, plan.ID); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if err := db.Get(&cancelledCount, `SELECT COUNT(*) FROM tasks WHERE dc_plan_id = ? AND status = 'cancelled'`, plan.ID); err != nil {
		t.Fatalf("count cancelled: %v", err)
	}
	if pendingCount != 2 || cancelledCount != 3 {
		t.Fatalf("pending=%d cancelled=%d want=2/3", pendingCount, cancelledCount)
	}
}

func TestTrimPendingTasksCancelsAllPendingWhenTargetAlreadyConsumed(t *testing.T) {
	db := newTestDCPlanTaskSupplyDB(t)
	defer db.Close()

	plan := testTaskSupplyPlan(1001, 123, 5)
	seedTaskSupplyPlan(t, db, plan)
	seedTaskSupplyResources(t, db, plan)
	workstationID := seedCurrentTaskSupplyWorkstation(t, db, plan.WorkspaceID)
	seedTrimPendingTasks(t, db, plan.ID, workstationID, plan.WorkspaceID, 4)

	// 当前数量已达到新目标，剩余预算为 0。
	if _, err := db.Exec(`UPDATE dc_plan SET target_count = 3, cur_count = 3 WHERE id = ?`, plan.ID); err != nil {
		t.Fatalf("reduce plan target: %v", err)
	}

	cancelled, err := NewDCPlanTaskSupplyService(db).TrimPendingTasks(context.Background(), plan.ID, time.Now().UTC())
	if err != nil {
		t.Fatalf("TrimPendingTasks() error = %v", err)
	}
	if cancelled != 4 {
		t.Fatalf("cancelled=%d want=4", cancelled)
	}

	var pendingCount int
	if err := db.Get(&pendingCount, `SELECT COUNT(*) FROM tasks WHERE dc_plan_id = ? AND status = 'pending' AND deleted_at IS NULL`, plan.ID); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pendingCount != 0 {
		t.Fatalf("pendingCount=%d want=0", pendingCount)
	}
}

func TestTrimPendingTasksKeepsPoolWhenTargetUnchanged(t *testing.T) {
	db := newTestDCPlanTaskSupplyDB(t)
	defer db.Close()

	plan := testTaskSupplyPlan(1001, 123, 5)
	seedTaskSupplyPlan(t, db, plan)
	seedTaskSupplyResources(t, db, plan)
	workstationID := seedCurrentTaskSupplyWorkstation(t, db, plan.WorkspaceID)
	seedTrimPendingTasks(t, db, plan.ID, workstationID, plan.WorkspaceID, 3)

	cancelled, err := NewDCPlanTaskSupplyService(db).TrimPendingTasks(context.Background(), plan.ID, time.Now().UTC())
	if err != nil {
		t.Fatalf("TrimPendingTasks() error = %v", err)
	}
	if cancelled != 0 {
		t.Fatalf("cancelled=%d want=0", cancelled)
	}

	var pendingCount int
	if err := db.Get(&pendingCount, `SELECT COUNT(*) FROM tasks WHERE dc_plan_id = ? AND status = 'pending' AND deleted_at IS NULL`, plan.ID); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pendingCount != 3 {
		t.Fatalf("pendingCount=%d want=3", pendingCount)
	}
}
