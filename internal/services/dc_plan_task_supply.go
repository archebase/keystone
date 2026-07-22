// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

var (
	// ErrDCPlanTaskSupplyNotFound indicates the requested active plan does not exist.
	ErrDCPlanTaskSupplyNotFound = errors.New("dc plan task supply plan not found")
	// ErrDCPlanTaskSupplyWorkstationMismatch indicates the workstation cannot execute the plan.
	ErrDCPlanTaskSupplyWorkstationMismatch = errors.New("dc plan task supply workstation mismatch")
	// ErrDCPlanTaskSupplyTargetReached indicates the plan has no remaining execution slots.
	ErrDCPlanTaskSupplyTargetReached = errors.New("dc plan task supply target reached")
	// ErrDCPlanTaskSupplyActiveTask indicates another task is already ready or recording.
	ErrDCPlanTaskSupplyActiveTask = errors.New("dc plan task supply active task exists")
)

// DCPlanSuppliedTask is the task returned by on-demand plan task supply.
type DCPlanSuppliedTask struct {
	ID            int64  `json:"id" db:"id"`
	TaskID        string `json:"task_id" db:"task_id"`
	Status        string `json:"status" db:"status"`
	DCPlanID      int64  `json:"dc_plan_id" db:"dc_plan_id"`
	WorkstationID int64  `json:"workstation_id" db:"workstation_id"`
}

// DCPlanTaskSupplyResult reports whether EnsureNextTask created or reused a task.
type DCPlanTaskSupplyResult struct {
	Task    DCPlanSuppliedTask `json:"task"`
	Created bool               `json:"created"`
}

// DCPlanTaskSupplyService creates at most one pending task for a plan on demand.
type DCPlanTaskSupplyService struct {
	db *sqlx.DB
}

// NewDCPlanTaskSupplyService creates a DCPlanTaskSupplyService.
func NewDCPlanTaskSupplyService(db *sqlx.DB) *DCPlanTaskSupplyService {
	return &DCPlanTaskSupplyService{db: db}
}

type taskSupplyPlanRow struct {
	ID                   int64  `db:"id"`
	WorkspaceID          int64  `db:"workspace_id"`
	Name                 string `db:"name"`
	Operator             string `db:"operator"`
	DCProjectDescription string `db:"dc_project_description"`
	DCTaskDescription    string `db:"dc_task_description"`
	DCDeviceID           int64  `db:"dc_device_id"`
	DCType               string `db:"dc_type"`
	TargetCount          int64  `db:"target_count"`
	TargetDuration       int64  `db:"target_duration"`
}

func taskSupplyForUpdateClause(tx *sqlx.Tx) string {
	if tx != nil && tx.DriverName() == "sqlite" {
		return ""
	}
	return " FOR UPDATE"
}

// EnsureNextTask returns the existing pending task or creates one for the plan.
func (s *DCPlanTaskSupplyService) EnsureNextTask(
	ctx context.Context,
	planID int64,
	workstationID int64,
	now time.Time,
) (*DCPlanTaskSupplyResult, error) {
	if s == nil || s.db == nil || planID <= 0 || workstationID <= 0 {
		return nil, ErrDCPlanTaskSupplyNotFound
	}

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin task supply transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var plan taskSupplyPlanRow
	if err := tx.GetContext(ctx, &plan, `
		SELECT id, workspace_id, name, operator,
			COALESCE(dc_project_description, '') AS dc_project_description,
			COALESCE(dc_task_description, '') AS dc_task_description,
			dc_device_id, dc_type, target_count, target_duration
		FROM dc_plan
		WHERE id = ? AND deleted_at IS NULL
		LIMIT 1`+taskSupplyForUpdateClause(tx), planID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrDCPlanTaskSupplyNotFound
		}
		return nil, fmt.Errorf("query task supply plan: %w", err)
	}

	var compatible bool
	if err := tx.GetContext(ctx, &compatible, `
		SELECT EXISTS(
			SELECT 1
			FROM workstations ws
			INNER JOIN data_collectors dc ON dc.id = ws.data_collector_id AND dc.deleted_at IS NULL
			INNER JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
			WHERE ws.id = ?
				AND ws.workspace_id = ?
				AND ws.deleted_at IS NULL
				AND dc.operator_id = ?
				AND r.device_id = ?
		)
	`, workstationID, plan.WorkspaceID, strings.TrimSpace(plan.Operator), strconv.FormatInt(plan.DCDeviceID, 10)); err != nil {
		return nil, fmt.Errorf("validate task supply workstation: %w", err)
	}
	if !compatible {
		return nil, ErrDCPlanTaskSupplyWorkstationMismatch
	}

	var counts struct {
		Committed int64 `db:"committed"`
		Active    int64 `db:"active"`
	}
	if err := tx.GetContext(ctx, &counts, `
		SELECT
			COALESCE(SUM(CASE WHEN status IN ('ready', 'in_progress', 'uploading', 'completed') THEN 1 ELSE 0 END), 0) AS committed,
			COALESCE(SUM(CASE WHEN status IN ('ready', 'in_progress') THEN 1 ELSE 0 END), 0) AS active
		FROM tasks
		WHERE dc_plan_id = ? AND deleted_at IS NULL
	`, plan.ID); err != nil {
		return nil, fmt.Errorf("count committed plan tasks: %w", err)
	}
	if counts.Committed >= plan.TargetCount {
		if _, err := tx.ExecContext(ctx, `
			UPDATE tasks
			SET status = 'cancelled', updated_at = ?
			WHERE dc_plan_id = ? AND status = 'pending' AND deleted_at IS NULL
		`, now, plan.ID); err != nil {
			return nil, fmt.Errorf("cancel pending tasks at target: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit target-reached task supply: %w", err)
		}
		return nil, ErrDCPlanTaskSupplyTargetReached
	}
	if counts.Active > 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE tasks
			SET status = 'cancelled', updated_at = ?
			WHERE dc_plan_id = ? AND status = 'pending' AND deleted_at IS NULL
		`, now, plan.ID); err != nil {
			return nil, fmt.Errorf("cancel pending tasks blocked by active task: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit active-task pending cleanup: %w", err)
		}
		return nil, ErrDCPlanTaskSupplyActiveTask
	}

	pendingTasks := []DCPlanSuppliedTask{}
	if err := tx.SelectContext(ctx, &pendingTasks, `
		SELECT id, task_id, status, dc_plan_id, workstation_id
		FROM tasks
		WHERE dc_plan_id = ? AND status = 'pending' AND deleted_at IS NULL
		ORDER BY id`+taskSupplyForUpdateClause(tx), plan.ID); err != nil {
		return nil, fmt.Errorf("query pending plan tasks: %w", err)
	}
	var reusable *DCPlanSuppliedTask
	for index := range pendingTasks {
		if reusable == nil && pendingTasks[index].WorkstationID == workstationID {
			reusable = &pendingTasks[index]
		}
	}
	if reusable != nil {
		if _, err := tx.ExecContext(ctx, `
			UPDATE tasks
			SET status = 'cancelled', updated_at = ?
			WHERE dc_plan_id = ? AND status = 'pending' AND deleted_at IS NULL AND id <> ?
		`, now, plan.ID, reusable.ID); err != nil {
			return nil, fmt.Errorf("collapse duplicate pending tasks: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit reused pending task: %w", err)
		}
		return &DCPlanTaskSupplyResult{Task: *reusable, Created: false}, nil
	}
	if len(pendingTasks) > 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE tasks
			SET status = 'cancelled', updated_at = ?
			WHERE dc_plan_id = ? AND status = 'pending' AND deleted_at IS NULL
		`, now, plan.ID); err != nil {
			return nil, fmt.Errorf("cancel mismatched pending tasks: %w", err)
		}
	}

	metadata, err := marshalMetadata(map[string]any{
		"source":                 "hilbert_dc_plan",
		"workspace_id":           plan.WorkspaceID,
		"dc_plan_id":             plan.ID,
		"dc_plan_name":           strings.TrimSpace(plan.Name),
		"dc_type":                strings.TrimSpace(plan.DCType),
		"dc_project_description": strings.TrimSpace(plan.DCProjectDescription),
		"dc_task_description":    strings.TrimSpace(plan.DCTaskDescription),
		"dc_device_id":           plan.DCDeviceID,
		"operator":               strings.TrimSpace(plan.Operator),
		"target_count":           plan.TargetCount,
		"target_duration":        plan.TargetDuration,
		"last_plan_synced_at":    now.UTC().Format(time.RFC3339),
		"execution_config": map[string]any{
			"topics": []string{},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create task snapshot metadata: %w", err)
	}
	taskID, err := NewPublicTaskID(now, 0)
	if err != nil {
		return nil, fmt.Errorf("create task id: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO tasks (
			task_id, workstation_id, organization_id, dc_plan_id, local_dc_plan_id,
			status, assigned_at, metadata, created_at, updated_at
		) VALUES (?, ?, ?, ?, NULL, 'pending', ?, ?, ?, ?)
	`, taskID, workstationID, plan.WorkspaceID, plan.ID, now, metadata, now, now)
	if err != nil {
		return nil, fmt.Errorf("insert pending plan task: %w", err)
	}
	taskNumericID, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("read pending plan task id: %w", err)
	}
	supplied := DCPlanSuppliedTask{
		ID:            taskNumericID,
		TaskID:        taskID,
		Status:        "pending",
		DCPlanID:      plan.ID,
		WorkstationID: workstationID,
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit created pending task: %w", err)
	}
	return &DCPlanTaskSupplyResult{Task: supplied, Created: true}, nil
}
