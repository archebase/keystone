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
	"archebase.com/keystone-edge/internal/logger"
	"github.com/jmoiron/sqlx"
)

// DCPlanWorkstationProjectionSummary summarizes workstation bindings derived from plans.
type DCPlanWorkstationProjectionSummary struct {
	TotalPlans   int `json:"total_plans"`
	CreatedCount int `json:"created_count"`
	ReusedCount  int `json:"reused_count"`
	BlockedCount int `json:"blocked_count"`
}

type dcPlanWorkstationProjector struct {
	db *sqlx.DB
}

type planProjectionCollector struct {
	ID         int64  `db:"id"`
	Name       string `db:"name"`
	OperatorID string `db:"operator_id"`
}

type planProjectionRobot struct {
	ID          int64  `db:"id"`
	DeviceID    string `db:"device_id"`
	DeviceName  string `db:"device_name"`
	WorkspaceID int64  `db:"workspace_id"`
}

type planProjectionWorkstation struct {
	ID           int64        `db:"id"`
	SupersededAt sql.NullTime `db:"superseded_at"`
}

func newDCPlanWorkstationProjector(db *sqlx.DB) *dcPlanWorkstationProjector {
	return &dcPlanWorkstationProjector{db: db}
}

// EnsureDCPlanWorkstation creates or reuses the workstation required to execute one bound Hilbert plan.
func EnsureDCPlanWorkstation(
	ctx context.Context,
	db *sqlx.DB,
	plan auth.HilbertDCPlan,
	now time.Time,
) error {
	if db == nil || plan.DCDeviceID == nil {
		return fmt.Errorf("dc plan workstation projection requires a bound device")
	}
	_, err := newDCPlanWorkstationProjector(db).projectPlan(ctx, plan, now)
	return err
}

func projectionForUpdateClause(tx *sqlx.Tx) string {
	if tx != nil && tx.DriverName() == "sqlite" {
		return ""
	}
	return " FOR UPDATE"
}

func (p *dcPlanWorkstationProjector) project(
	ctx context.Context,
	plans []auth.HilbertDCPlan,
	now time.Time,
) *DCPlanWorkstationProjectionSummary {
	summary := &DCPlanWorkstationProjectionSummary{TotalPlans: len(plans)}
	if p == nil || p.db == nil {
		summary.BlockedCount = len(plans)
		return summary
	}

	for _, plan := range plans {
		if plan.DCDeviceID == nil {
			// 无设备的 Ego 计划先保留投影，等待设备接单时由 Keystone 补绑。
			summary.BlockedCount++
			continue
		}
		created, err := p.projectPlan(ctx, plan, now)
		if err != nil {
			summary.BlockedCount++
			logger.Printf(
				"[DC_PLAN] Workstation projection blocked: workspace_id=%d dc_plan_id=%d operator=%s dc_device_id=%v err=%v",
				plan.WorkspaceID,
				plan.ID,
				strings.TrimSpace(plan.Operator),
				nullableInt64ForLog(plan.DCDeviceID),
				err,
			)
			continue
		}
		if created {
			summary.CreatedCount++
		} else {
			summary.ReusedCount++
		}
	}
	return summary
}

func (p *dcPlanWorkstationProjector) projectPlan(
	ctx context.Context,
	plan auth.HilbertDCPlan,
	now time.Time,
) (bool, error) {
	tx, err := p.db.BeginTxx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	collector, err := resolveProjectionCollector(ctx, tx, plan)
	if err != nil {
		return false, err
	}
	robot, err := resolveProjectionRobot(ctx, tx, plan)
	if err != nil {
		return false, err
	}
	created, err := ensureProjectedWorkstation(ctx, tx, plan, collector, robot, now)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit transaction: %w", err)
	}
	return created, nil
}

func resolveProjectionCollector(
	ctx context.Context,
	tx *sqlx.Tx,
	plan auth.HilbertDCPlan,
) (planProjectionCollector, error) {
	var collector planProjectionCollector
	if err := tx.GetContext(ctx, &collector, `
		SELECT id, name, operator_id
		FROM data_collectors
		WHERE operator_id = ? AND deleted_at IS NULL
		LIMIT 1`+projectionForUpdateClause(tx), strings.TrimSpace(plan.Operator)); err != nil {
		if err == sql.ErrNoRows {
			return collector, fmt.Errorf("collector missing")
		}
		return collector, fmt.Errorf("query collector: %w", err)
	}
	allowed, err := OperatorHasWorkspaceAccess(ctx, tx, collector.OperatorID, plan.WorkspaceID)
	if err != nil {
		return collector, fmt.Errorf("query collector workspace access: %w", err)
	}
	if !allowed {
		return collector, fmt.Errorf("collector has no workspace access")
	}
	return collector, nil
}

func nullableInt64ForLog(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func resolveProjectionRobot(
	ctx context.Context,
	tx *sqlx.Tx,
	plan auth.HilbertDCPlan,
) (planProjectionRobot, error) {
	var robot planProjectionRobot
	if err := tx.GetContext(ctx, &robot, `
		SELECT id, device_id, COALESCE(device_name, '') AS device_name, workspace_id
		FROM robots
		WHERE device_id = ? AND deleted_at IS NULL
		LIMIT 1`+projectionForUpdateClause(tx), strconv.FormatInt(*plan.DCDeviceID, 10)); err != nil {
		if err == sql.ErrNoRows {
			return robot, fmt.Errorf("robot missing")
		}
		return robot, fmt.Errorf("query robot: %w", err)
	}
	if robot.WorkspaceID != plan.WorkspaceID {
		return robot, fmt.Errorf("robot belongs to workspace %d", robot.WorkspaceID)
	}
	return robot, nil
}

func ensureProjectedWorkstation(
	ctx context.Context,
	tx *sqlx.Tx,
	plan auth.HilbertDCPlan,
	collector planProjectionCollector,
	robot planProjectionRobot,
	now time.Time,
) (bool, error) {
	var workstation planProjectionWorkstation
	err := tx.GetContext(ctx, &workstation, `
		SELECT id, superseded_at
		FROM workstations
		WHERE robot_id = ? AND data_collector_id = ? AND workspace_id = ? AND deleted_at IS NULL
		ORDER BY (superseded_at IS NULL) DESC, is_current DESC, id DESC
		LIMIT 1`+projectionForUpdateClause(tx), robot.ID, collector.ID, plan.WorkspaceID)
	if err == nil {
		if workstation.SupersededAt.Valid {
			if _, updateErr := tx.ExecContext(ctx, `
				UPDATE workstations
				SET status = 'offline', is_current = FALSE, superseded_at = NULL,
					superseded_by = NULL, updated_at = ?
				WHERE id = ? AND deleted_at IS NULL
			`, now, workstation.ID); updateErr != nil {
				return false, fmt.Errorf("reactivate workstation: %w", updateErr)
			}
		}
		return false, nil
	}
	if err != sql.ErrNoRows {
		return false, fmt.Errorf("query workstation: %w", err)
	}

	metadata, err := marshalMetadata(map[string]any{
		"source":       "hilbert_dc_plan",
		"dc_plan_id":   plan.ID,
		"workspace_id": plan.WorkspaceID,
		"dc_device_id": nullableInt64ForLog(plan.DCDeviceID),
		"operator":     strings.TrimSpace(plan.Operator),
		"last_seen_at": now.UTC().Format(time.RFC3339),
	})
	if err != nil {
		return false, fmt.Errorf("create workstation metadata: %w", err)
	}
	robotName := strings.TrimSpace(robot.DeviceName)
	if robotName == "" {
		robotName = robot.DeviceID
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workstations (
			robot_id, robot_name, robot_serial, data_collector_id, collector_name,
			collector_operator_id, workspace_id, name, status, metadata,
			created_at, updated_at, is_current
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'offline', ?, ?, ?, FALSE)
	`,
		robot.ID,
		robotName,
		robot.DeviceID,
		collector.ID,
		collector.Name,
		collector.OperatorID,
		plan.WorkspaceID,
		fmt.Sprintf("Hilbert Plan %d Workstation", plan.ID),
		metadata,
		now,
		now,
	); err != nil {
		return false, fmt.Errorf("create workstation: %w", err)
	}
	return true, nil
}
