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

const (
	egoPortalStereoDeviceType = "Ego Portal Stereo"
	egoPortalLiteDeviceType   = "Ego Portal Lite"
	egoPortalE2DeviceType     = "Ego Portal E2"
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

// EgoPortalPendingPoolResult reports task-pool maintenance for one DC plan.
type EgoPortalPendingPoolResult struct {
	Enabled       bool
	PlanID        int64
	WorkstationID int64
	DesiredCount  int
	PendingCount  int
	CreatedCount  int
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
	DCDeviceID           *int64 `db:"dc_device_id"`
	Status               string `db:"status"`
	DCType               string `db:"dc_type"`
	TargetCount          int64  `db:"target_count"`
	CurCount             int64  `db:"cur_count"`
	TargetDuration       int64  `db:"target_duration"`
}

type taskSupplyWorkstationRow struct {
	ID         int64  `db:"id"`
	DeviceType string `db:"device_type"`
}

type taskSupplyCounts struct {
	LocalReserved int64 `db:"local_reserved"`
	Active        int64 `db:"active"`
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

	plan, err := loadTaskSupplyPlan(ctx, tx, planID)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(strings.TrimSpace(plan.Status), "collected") {
		return nil, ErrDCPlanTaskSupplyTargetReached
	}
	workstation, err := loadTaskSupplyWorkstation(ctx, tx, plan, workstationID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrDCPlanTaskSupplyWorkstationMismatch
	}
	if err != nil {
		return nil, err
	}
	// Offline-capable devices retain their prebuilt pending-task pool.
	preservePendingPool := usesEgoPortalPendingPool(workstation.DeviceType)

	counts, err := loadTaskSupplyCounts(ctx, tx, plan.ID)
	if err != nil {
		return nil, err
	}
	if plan.CurCount+counts.LocalReserved >= plan.TargetCount {
		if !preservePendingPool {
			if _, err := tx.ExecContext(ctx, `
				UPDATE tasks
				SET status = 'cancelled', updated_at = ?
				WHERE dc_plan_id = ? AND status = 'pending' AND deleted_at IS NULL
			`, now, plan.ID); err != nil {
				return nil, fmt.Errorf("cancel pending tasks at target: %w", err)
			}
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit target-reached task supply: %w", err)
		}
		return nil, ErrDCPlanTaskSupplyTargetReached
	}
	if counts.Active > 0 {
		if !preservePendingPool {
			if _, err := tx.ExecContext(ctx, `
				UPDATE tasks
				SET status = 'cancelled', updated_at = ?
				WHERE dc_plan_id = ? AND status = 'pending' AND deleted_at IS NULL
			`, now, plan.ID); err != nil {
				return nil, fmt.Errorf("cancel pending tasks blocked by active task: %w", err)
			}
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
		if !preservePendingPool {
			if _, err := tx.ExecContext(ctx, `
				UPDATE tasks
				SET status = 'cancelled', updated_at = ?
				WHERE dc_plan_id = ? AND status = 'pending' AND deleted_at IS NULL AND id <> ?
			`, now, plan.ID, reusable.ID); err != nil {
				return nil, fmt.Errorf("collapse duplicate pending tasks: %w", err)
			}
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit reused pending task: %w", err)
		}
		return &DCPlanTaskSupplyResult{Task: *reusable, Created: false}, nil
	}
	if len(pendingTasks) > 0 && !preservePendingPool {
		if _, err := tx.ExecContext(ctx, `
			UPDATE tasks
			SET status = 'cancelled', updated_at = ?
			WHERE dc_plan_id = ? AND status = 'pending' AND deleted_at IS NULL
		`, now, plan.ID); err != nil {
			return nil, fmt.Errorf("cancel mismatched pending tasks: %w", err)
		}
	}

	supplied, err := insertPendingPlanTask(ctx, tx, plan, workstationID, now, 0, false)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit created pending task: %w", err)
	}
	return &DCPlanTaskSupplyResult{Task: *supplied, Created: true}, nil
}

// EnsureUnboundEgoCandidateTask ensures one pending candidate task exists for a Hilbert plan
// that has not been bound to a device yet. The workstation is derived from the currently
// authenticated Ego device and operator, not from dc_plan.dc_device_id, so the plan remains
// selectable by both ego-portal and ego-portal-lite before the real device binding happens.
func (s *DCPlanTaskSupplyService) EnsureUnboundEgoCandidateTask(
	ctx context.Context,
	planID int64,
	workstationID int64,
	now time.Time,
) error {
	if s == nil || s.db == nil || planID <= 0 || workstationID <= 0 {
		return ErrDCPlanTaskSupplyNotFound
	}

	var count int
	if err := s.db.GetContext(ctx, &count, `
		SELECT COUNT(*)
		FROM tasks
		WHERE dc_plan_id = ? AND workstation_id = ?
			AND status = 'pending' AND deleted_at IS NULL
	`, planID, workstationID); err != nil {
		return fmt.Errorf("count unbound candidate tasks: %w", err)
	}
	if count > 0 {
		return nil
	}

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin unbound candidate task transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	plan, err := loadTaskSupplyPlan(ctx, tx, planID)
	if err != nil {
		return err
	}
	if plan.DCDeviceID != nil || strings.EqualFold(strings.TrimSpace(plan.Status), "collected") {
		return nil
	}
	if _, err := insertPendingPlanTask(ctx, tx, plan, workstationID, now, 0, false); err != nil {
		return err
	}
	return tx.Commit()
}

// EnsureEgoPortalPendingPool fills the pending-task pool for an offline-capable Ego Portal plan.
func (s *DCPlanTaskSupplyService) EnsureEgoPortalPendingPool(
	ctx context.Context,
	planID int64,
	now time.Time,
) (*EgoPortalPendingPoolResult, error) {
	if s == nil || s.db == nil || planID <= 0 {
		return nil, ErrDCPlanTaskSupplyNotFound
	}

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin pending pool transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	plan, err := loadTaskSupplyPlan(ctx, tx, planID)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(strings.TrimSpace(plan.Status), "collected") {
		return nil, ErrDCPlanTaskSupplyTargetReached
	}
	workstation, err := loadTaskSupplyWorkstation(ctx, tx, plan, 0)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrDCPlanTaskSupplyWorkstationMismatch
	}
	if err != nil {
		return nil, err
	}
	result := &EgoPortalPendingPoolResult{
		Enabled:       usesEgoPortalPendingPool(workstation.DeviceType),
		PlanID:        plan.ID,
		WorkstationID: workstation.ID,
	}
	if !result.Enabled {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit disabled pending pool: %w", err)
		}
		return result, nil
	}

	counts, err := loadTaskSupplyCounts(ctx, tx, plan.ID)
	if err != nil {
		return nil, err
	}
	remaining := plan.TargetCount - plan.CurCount - counts.LocalReserved
	if remaining > 0 {
		result.DesiredCount = int(remaining)
	}
	if err := tx.GetContext(ctx, &result.PendingCount, `
		SELECT COUNT(*)
		FROM tasks
		WHERE dc_plan_id = ? AND workstation_id = ?
			AND status = 'pending' AND deleted_at IS NULL
	`, plan.ID, workstation.ID); err != nil {
		return nil, fmt.Errorf("count pending pool tasks: %w", err)
	}

	missing := result.DesiredCount - result.PendingCount
	for sequence := 0; sequence < missing; sequence++ {
		if _, err := insertPendingPlanTask(ctx, tx, plan, workstation.ID, now, sequence, true); err != nil {
			return nil, err
		}
		result.CreatedCount++
	}
	result.PendingCount += result.CreatedCount
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit pending pool maintenance: %w", err)
	}
	return result, nil
}

func loadTaskSupplyPlan(
	ctx context.Context,
	tx *sqlx.Tx,
	planID int64,
) (taskSupplyPlanRow, error) {
	var plan taskSupplyPlanRow
	if err := tx.GetContext(ctx, &plan, `
		SELECT id, workspace_id, name, operator,
			COALESCE(status, 'pending_collection') AS status,
			COALESCE(dc_project_description, '') AS dc_project_description,
			COALESCE(dc_task_description, '') AS dc_task_description,
			dc_device_id, dc_type, target_count, cur_count, target_duration
		FROM dc_plan
		WHERE id = ? AND deleted_at IS NULL
		LIMIT 1`+taskSupplyForUpdateClause(tx), planID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return plan, ErrDCPlanTaskSupplyNotFound
		}
		return plan, fmt.Errorf("query task supply plan: %w", err)
	}
	return plan, nil
}

func loadTaskSupplyWorkstation(
	ctx context.Context,
	tx *sqlx.Tx,
	plan taskSupplyPlanRow,
	workstationID int64,
) (taskSupplyWorkstationRow, error) {
	if plan.DCDeviceID == nil {
		return taskSupplyWorkstationRow{}, sql.ErrNoRows
	}
	query := `
		SELECT ws.id, COALESCE(r.device_type, '') AS device_type
		FROM workstations ws
		INNER JOIN data_collectors dc ON dc.id = ws.data_collector_id AND dc.deleted_at IS NULL
		INNER JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
		WHERE ws.workspace_id = ?
			AND ws.deleted_at IS NULL
			AND dc.operator_id = ?
			AND r.device_id = ?`
	args := []any{plan.WorkspaceID, strings.TrimSpace(plan.Operator), strconv.FormatInt(*plan.DCDeviceID, 10)}
	if workstationID > 0 {
		query += " AND ws.id = ?"
		args = append(args, workstationID)
		query += " ORDER BY ws.id DESC"
	} else {
		query += " ORDER BY ws.is_current DESC, ws.id DESC"
	}
	query += " LIMIT 1" + taskSupplyForUpdateClause(tx)

	var workstation taskSupplyWorkstationRow
	if err := tx.GetContext(ctx, &workstation, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return workstation, sql.ErrNoRows
		}
		return workstation, fmt.Errorf("validate task supply workstation: %w", err)
	}
	return workstation, nil
}

func loadTaskSupplyCounts(
	ctx context.Context,
	tx *sqlx.Tx,
	planID int64,
) (taskSupplyCounts, error) {
	var counts taskSupplyCounts
	if err := tx.GetContext(ctx, &counts, `
		SELECT
			(
				SELECT COUNT(*)
				FROM episodes e
				WHERE e.dc_plan_id = ?
					AND COALESCE(e.cloud_synced, FALSE) = FALSE
					AND COALESCE(e.qa_status, 'pending_qa') NOT IN ('failed', 'manual_review_failed')
					AND e.deleted_at IS NULL
			) + (
				SELECT COUNT(*)
				FROM tasks t
				WHERE t.dc_plan_id = ?
					AND t.status = 'uploading'
					AND t.deleted_at IS NULL
					AND NOT EXISTS (
						SELECT 1
						FROM episodes e
						WHERE e.task_id = t.id AND e.deleted_at IS NULL
					)
			) + (
				SELECT COUNT(*)
				FROM tasks t
				WHERE t.dc_plan_id = ?
					AND t.status IN ('ready', 'in_progress')
					AND t.deleted_at IS NULL
			) AS local_reserved,
			(
				SELECT COUNT(*)
				FROM tasks t
				WHERE t.dc_plan_id = ?
					AND t.status IN ('ready', 'in_progress')
					AND t.deleted_at IS NULL
			) AS active
	`, planID, planID, planID, planID); err != nil {
		return counts, fmt.Errorf("count committed plan tasks: %w", err)
	}
	return counts, nil
}

func usesEgoPortalPendingPool(deviceType string) bool {
	return deviceType == egoPortalStereoDeviceType ||
		deviceType == egoPortalLiteDeviceType ||
		deviceType == egoPortalE2DeviceType
}

func insertPendingPlanTask(
	ctx context.Context,
	tx *sqlx.Tx,
	plan taskSupplyPlanRow,
	workstationID int64,
	now time.Time,
	sequence int,
	pooled bool,
) (*DCPlanSuppliedTask, error) {
	metadataValues := map[string]any{
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
	}
	if pooled {
		metadataValues["supply_mode"] = "ego_portal_pending_pool"
	}
	metadata, err := marshalMetadata(metadataValues)
	if err != nil {
		return nil, fmt.Errorf("create task snapshot metadata: %w", err)
	}
	taskID, err := NewPublicTaskID(now, sequence)
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
	return &DCPlanSuppliedTask{
		ID:            taskNumericID,
		TaskID:        taskID,
		Status:        "pending",
		DCPlanID:      plan.ID,
		WorkstationID: workstationID,
	}, nil
}

// EnsureUnboundEgoCandidateTasksForWorkstation binds every unbound dc plan owned by the
// operator to the current device, then ensures one pending task exists for every newly
// executable plan assigned to that device. It is safe to call from both /operator/plans/refresh
// and /tasks list flows; existing pending tasks are reused.
func EnsureUnboundEgoCandidateTasksForWorkstation(
	ctx context.Context,
	db *sqlx.DB,
	hilbert HilbertDCPlanBinder,
	workspaceID int64,
	operator string,
	workstationID int64,
	deviceID int64,
	now time.Time,
) error {
	if db == nil || workspaceID <= 0 || strings.TrimSpace(operator) == "" || workstationID <= 0 || deviceID <= 0 {
		return nil
	}
	planIDs := []int64{}
	if err := db.SelectContext(ctx, &planIDs, `
		SELECT id
		FROM dc_plan
		WHERE workspace_id = ?
			AND operator = ?
			AND dc_device_id IS NULL
			AND COALESCE(status, 'pending_collection') != 'collected'
			AND deleted_at IS NULL
		ORDER BY id
	`, workspaceID, strings.TrimSpace(operator)); err != nil {
		return err
	}
	supply := NewDCPlanTaskSupplyService(db)
	for _, planID := range planIDs {
		if err := BindDCPlanDevice(ctx, db, hilbert, workspaceID, planID, deviceID); err != nil {
			continue
		}
		if _, err := supply.EnsureNextTask(ctx, planID, workstationID, now); err != nil {
			continue
		}
	}
	return nil
}
