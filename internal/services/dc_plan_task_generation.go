// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"archebase.com/keystone-edge/internal/auth"
	"github.com/jmoiron/sqlx"
)

const (
	dcPlanTaskGenerationStatusCreated = "created"
	dcPlanTaskGenerationStatusNoop    = "noop"
	dcPlanTaskGenerationStatusBlocked = "blocked"
)

// DCPlanTaskGenerationSummary summarizes task generation after a dc_plan sync.
type DCPlanTaskGenerationSummary struct {
	TotalPlans   int                          `json:"total_plans"`
	CreatedCount int                          `json:"created_count"`
	BlockedCount int                          `json:"blocked_count"`
	Plans        []DCPlanTaskGenerationResult `json:"plans"`
}

// DCPlanTaskGenerationResult summarizes task generation for one dc_plan.
type DCPlanTaskGenerationResult struct {
	DCPlanID          int64  `json:"dc_plan_id"`
	Status            string `json:"status"`
	Reason            string `json:"reason,omitempty"`
	TargetCount       int64  `json:"target_count"`
	ExistingTaskCount int64  `json:"existing_task_count"`
	CreatedTaskCount  int64  `json:"created_task_count"`
}

// DCPlanTaskGenerationService creates Keystone tasks from Hilbert dc_plan projections.
type DCPlanTaskGenerationService struct {
	db *sqlx.DB
}

// NewDCPlanTaskGenerationService creates a DCPlanTaskGenerationService.
func NewDCPlanTaskGenerationService(db *sqlx.DB) *DCPlanTaskGenerationService {
	return &DCPlanTaskGenerationService{db: db}
}

// GenerateForPlans creates missing tasks for the plans returned by the current Hilbert sync.
func (s *DCPlanTaskGenerationService) GenerateForPlans(ctx context.Context, plans []auth.HilbertDCPlan, now time.Time) *DCPlanTaskGenerationSummary {
	summary := &DCPlanTaskGenerationSummary{
		TotalPlans: len(plans),
		Plans:      []DCPlanTaskGenerationResult{},
	}
	if s == nil || s.db == nil {
		for _, plan := range plans {
			result := blockedPlanGenerationResult(plan, "not_configured")
			summary.BlockedCount++
			summary.Plans = append(summary.Plans, result)
		}
		return summary
	}

	for _, plan := range plans {
		result := s.generateForPlan(ctx, plan, now)
		summary.CreatedCount += int(result.CreatedTaskCount)
		if result.Status == dcPlanTaskGenerationStatusBlocked {
			summary.BlockedCount++
		}
		summary.Plans = append(summary.Plans, result)
	}
	return summary
}

func (s *DCPlanTaskGenerationService) generateForPlan(ctx context.Context, plan auth.HilbertDCPlan, now time.Time) DCPlanTaskGenerationResult {
	result := DCPlanTaskGenerationResult{
		DCPlanID:    plan.ID,
		Status:      dcPlanTaskGenerationStatusNoop,
		TargetCount: plan.TargetCount,
	}

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		result.Status = dcPlanTaskGenerationStatusBlocked
		result.Reason = "transaction_begin_failed"
		return result
	}
	defer func() { _ = tx.Rollback() }()

	var lockedPlanID int64
	if err := tx.GetContext(ctx, &lockedPlanID, "SELECT id FROM dc_plan WHERE id = ? AND deleted_at IS NULL"+forUpdateClause(tx), plan.ID); err != nil {
		result.Status = dcPlanTaskGenerationStatusBlocked
		result.Reason = "plan_not_found"
		return result
	}

	collector, err := resolvePlanCollector(ctx, tx, plan)
	if err != nil {
		result.Reason = err.Error()
		result.Status = dcPlanTaskGenerationStatusBlocked
		return result
	}
	robot, err := resolvePlanRobot(ctx, tx, plan)
	if err != nil {
		result.Reason = err.Error()
		result.Status = dcPlanTaskGenerationStatusBlocked
		return result
	}

	workstation, err := ensurePlanWorkstation(ctx, tx, plan, collector, robot, now)
	if err != nil {
		result.Reason = err.Error()
		result.Status = dcPlanTaskGenerationStatusBlocked
		return result
	}
	if err := tx.GetContext(ctx, &result.ExistingTaskCount, "SELECT COUNT(*) FROM tasks WHERE dc_plan_id = ? AND deleted_at IS NULL", plan.ID); err != nil {
		result.Reason = "count_tasks_failed"
		result.Status = dcPlanTaskGenerationStatusBlocked
		return result
	}
	if result.ExistingTaskCount >= plan.TargetCount {
		if err := tx.Commit(); err != nil {
			result.Reason = "transaction_commit_failed"
			result.Status = dcPlanTaskGenerationStatusBlocked
		}
		return result
	}

	toCreate := plan.TargetCount - result.ExistingTaskCount
	metadata, err := planTaskMetadata(plan, now)
	if err != nil {
		result.Reason = "metadata_failed"
		result.Status = dcPlanTaskGenerationStatusBlocked
		return result
	}
	for i := int64(0); i < toCreate; i++ {
		taskID, idErr := NewPublicTaskID(now, int(result.ExistingTaskCount+i))
		if idErr != nil {
			result.Reason = "task_id_failed"
			result.Status = dcPlanTaskGenerationStatusBlocked
			result.CreatedTaskCount = 0
			return result
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tasks (
				task_id, workstation_id, organization_id, dc_plan_id, local_dc_plan_id,
				status, assigned_at, metadata, created_at, updated_at
			) VALUES (?, ?, ?, ?, NULL, ?, ?, ?, ?, ?)
		`,
			taskID, workstation.ID, plan.WorkspaceID, plan.ID, "pending", now, metadata, now, now,
		); err != nil {
			result.Reason = "insert_task_failed"
			result.Status = dcPlanTaskGenerationStatusBlocked
			result.CreatedTaskCount = 0
			return result
		}
		result.CreatedTaskCount++
	}

	if err := tx.Commit(); err != nil {
		result.Reason = "transaction_commit_failed"
		result.Status = dcPlanTaskGenerationStatusBlocked
		result.CreatedTaskCount = 0
		return result
	}
	result.Status = dcPlanTaskGenerationStatusCreated
	return result
}

type planCollectorRow struct {
	ID         int64          `db:"id"`
	Name       string         `db:"name"`
	OperatorID string         `db:"operator_id"`
	Metadata   sql.NullString `db:"metadata"`
}

type planRobotRow struct {
	ID          int64          `db:"id"`
	DeviceID    string         `db:"device_id"`
	WorkspaceID int64          `db:"workspace_id"`
	Metadata    sql.NullString `db:"metadata"`
}

type planWorkstationRow struct {
	ID                  int64        `db:"id"`
	Name                string       `db:"name"`
	RobotName           string       `db:"robot_name"`
	RobotSerial         string       `db:"robot_serial"`
	CollectorName       string       `db:"collector_name"`
	CollectorOperatorID string       `db:"collector_operator_id"`
	SupersededAt        sql.NullTime `db:"superseded_at"`
}

func resolvePlanCollector(ctx context.Context, tx *sqlx.Tx, plan auth.HilbertDCPlan) (planCollectorRow, error) {
	var collector planCollectorRow
	err := tx.GetContext(ctx, &collector, `
		SELECT id, name, operator_id, metadata
		FROM data_collectors
		WHERE operator_id = ? AND deleted_at IS NULL
		LIMIT 1`+forUpdateClause(tx), strings.TrimSpace(plan.Operator))
	if err == sql.ErrNoRows {
		return collector, fmt.Errorf("collector_missing")
	}
	if err != nil {
		return collector, fmt.Errorf("collector_query_failed")
	}
	allowed, accessErr := OperatorHasWorkspaceAccess(ctx, tx, collector.OperatorID, plan.WorkspaceID)
	if accessErr != nil {
		return collector, fmt.Errorf("collector_workspace_query_failed")
	}
	if !allowed {
		return collector, fmt.Errorf("workspace_mismatch")
	}
	return collector, nil
}

func resolvePlanRobot(ctx context.Context, tx *sqlx.Tx, plan auth.HilbertDCPlan) (planRobotRow, error) {
	var robot planRobotRow
	err := tx.GetContext(ctx, &robot, `
		SELECT id, device_id, workspace_id, metadata
		FROM robots
		WHERE device_id = ? AND deleted_at IS NULL
		LIMIT 1`+forUpdateClause(tx), strconv.FormatInt(plan.DCDeviceID, 10))
	if err == sql.ErrNoRows {
		return robot, fmt.Errorf("robot_missing")
	}
	if err != nil {
		return robot, fmt.Errorf("robot_query_failed")
	}
	if robot.WorkspaceID != plan.WorkspaceID {
		return robot, fmt.Errorf("workspace_mismatch")
	}
	return robot, nil
}

func ensurePlanWorkstation(
	ctx context.Context,
	tx *sqlx.Tx,
	plan auth.HilbertDCPlan,
	collector planCollectorRow,
	robot planRobotRow,
	now time.Time,
) (planWorkstationRow, error) {
	var ws planWorkstationRow
	err := tx.GetContext(ctx, &ws, `
		SELECT id, COALESCE(name, '') AS name, COALESCE(robot_name, '') AS robot_name,
			COALESCE(robot_serial, '') AS robot_serial, COALESCE(collector_name, '') AS collector_name,
			COALESCE(collector_operator_id, '') AS collector_operator_id, superseded_at
		FROM workstations
		WHERE robot_id = ? AND data_collector_id = ? AND workspace_id = ? AND deleted_at IS NULL
		ORDER BY (superseded_at IS NULL) DESC, is_current DESC, id DESC
		LIMIT 1`+forUpdateClause(tx), robot.ID, collector.ID, plan.WorkspaceID)
	if err == nil {
		if ws.SupersededAt.Valid {
			if _, updateErr := tx.ExecContext(ctx, `
				UPDATE workstations
				SET status = 'offline', is_current = FALSE, superseded_at = NULL,
					superseded_by = NULL, updated_at = ?
				WHERE id = ? AND deleted_at IS NULL
			`, now, ws.ID); updateErr != nil {
				return ws, fmt.Errorf("workstation_reactivate_failed")
			}
			ws.SupersededAt = sql.NullTime{}
		}
		return ws, nil
	}
	if err != sql.ErrNoRows {
		return ws, fmt.Errorf("workstation_query_failed")
	}
	name := fmt.Sprintf("Hilbert Plan %d Workstation", plan.ID)
	metadata, err := marshalMetadata(map[string]any{
		"source":       "hilbert_dc_plan",
		"dc_plan_id":   plan.ID,
		"workspace_id": plan.WorkspaceID,
		"dc_device_id": plan.DCDeviceID,
		"operator":     strings.TrimSpace(plan.Operator),
		"last_seen_at": now.Format(time.RFC3339),
	})
	if err != nil {
		return ws, fmt.Errorf("workstation_metadata_failed")
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO workstations (
			robot_id, robot_name, robot_serial, data_collector_id, collector_name, collector_operator_id,
			workspace_id, name, status, metadata, created_at, updated_at, is_current
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, robot.ID, robot.DeviceID, robot.DeviceID, collector.ID, collector.Name, collector.OperatorID,
		plan.WorkspaceID, name, "offline", metadata, now, now, false)
	if err != nil {
		return ws, fmt.Errorf("workstation_create_failed")
	}
	id, err := res.LastInsertId()
	if err != nil {
		return ws, fmt.Errorf("workstation_id_failed")
	}
	ws = planWorkstationRow{
		ID:                  id,
		Name:                name,
		RobotName:           robot.DeviceID,
		RobotSerial:         robot.DeviceID,
		CollectorName:       collector.Name,
		CollectorOperatorID: collector.OperatorID,
	}
	return ws, nil
}

func forUpdateClause(tx *sqlx.Tx) string {
	if tx != nil && tx.DriverName() == "sqlite" {
		return ""
	}
	return " FOR UPDATE"
}

func blockedPlanGenerationResult(plan auth.HilbertDCPlan, reason string) DCPlanTaskGenerationResult {
	return DCPlanTaskGenerationResult{
		DCPlanID:    plan.ID,
		Status:      dcPlanTaskGenerationStatusBlocked,
		Reason:      reason,
		TargetCount: plan.TargetCount,
	}
}

func planTaskMetadata(plan auth.HilbertDCPlan, now time.Time) (string, error) {
	return marshalMetadata(map[string]any{
		"source":              "hilbert_dc_plan",
		"workspace_id":        plan.WorkspaceID,
		"dc_plan_id":          plan.ID,
		"dc_plan_name":        strings.TrimSpace(plan.Name),
		"dc_plan_sequence":    0,
		"dc_type":             strings.TrimSpace(plan.DCType),
		"dc_device_id":        plan.DCDeviceID,
		"operator":            strings.TrimSpace(plan.Operator),
		"target_count":        plan.TargetCount,
		"target_duration":     plan.TargetDuration,
		"last_plan_synced_at": now.Format(time.RFC3339),
		"execution_config": map[string]any{
			"topics": []string{},
		},
	})
}
