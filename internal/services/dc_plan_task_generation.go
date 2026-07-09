// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"context"
	"database/sql"
	"encoding/json"
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
	dcPlanCompatSOPVersion            = "1.0.0"
	dcPlanCompatSubsceneName          = "Default"
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

	factoryID, err := ensurePlanCompatFactory(ctx, tx, plan, now)
	if err != nil {
		result.Reason = "compat_factory_failed"
		result.Status = dcPlanTaskGenerationStatusBlocked
		return result
	}
	sopID, err := ensurePlanCompatSOP(ctx, tx, plan, now)
	if err != nil {
		result.Reason = "compat_sop_failed"
		result.Status = dcPlanTaskGenerationStatusBlocked
		return result
	}
	scene, err := ensurePlanCompatScene(ctx, tx, plan, factoryID, now)
	if err != nil {
		result.Reason = "compat_scene_failed"
		result.Status = dcPlanTaskGenerationStatusBlocked
		return result
	}
	workstation, err := ensurePlanWorkstation(ctx, tx, plan, collector, robot, factoryID, now)
	if err != nil {
		result.Reason = err.Error()
		result.Status = dcPlanTaskGenerationStatusBlocked
		return result
	}
	orderID, err := ensurePlanCompatOrder(ctx, tx, plan, scene.ID, now)
	if err != nil {
		result.Reason = "compat_order_failed"
		result.Status = dcPlanTaskGenerationStatusBlocked
		return result
	}
	batch, err := ensurePlanCompatBatch(ctx, tx, plan, orderID, workstation, now)
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
				task_id, batch_id, order_id, sop_id, workstation_id,
				scene_id, subscene_id, batch_name, scene_name, subscene_name,
				factory_id, organization_id, dc_plan_id, local_dc_plan_id,
				initial_scene_layout, status, assigned_at, metadata, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?, ?, ?, ?, ?)
		`,
			taskID, batch.ID, orderID, sopID, workstation.ID,
			scene.ID, scene.SubsceneID, batch.Name, scene.Name, scene.SubsceneName,
			factoryID, plan.WorkspaceID, plan.ID, scene.Layout, "pending", now, metadata, now, now,
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
	ID             int64          `db:"id"`
	OrganizationID int64          `db:"organization_id"`
	Name           string         `db:"name"`
	OperatorID     string         `db:"operator_id"`
	Metadata       sql.NullString `db:"metadata"`
}

type planRobotRow struct {
	ID          int64          `db:"id"`
	RobotTypeID int64          `db:"robot_type_id"`
	DeviceID    string         `db:"device_id"`
	FactoryID   int64          `db:"factory_id"`
	Metadata    sql.NullString `db:"metadata"`
}

type planWorkstationRow struct {
	ID                  int64  `db:"id"`
	Name                string `db:"name"`
	RobotName           string `db:"robot_name"`
	RobotSerial         string `db:"robot_serial"`
	CollectorName       string `db:"collector_name"`
	CollectorOperatorID string `db:"collector_operator_id"`
}

type planSceneRow struct {
	ID           int64  `db:"id"`
	Name         string `db:"name"`
	SubsceneID   int64  `db:"subscene_id"`
	SubsceneName string `db:"subscene_name"`
	Layout       string `db:"layout"`
}

type planBatchRow struct {
	ID   int64  `db:"id"`
	Name string `db:"name"`
}

func resolvePlanCollector(ctx context.Context, tx *sqlx.Tx, plan auth.HilbertDCPlan) (planCollectorRow, error) {
	var collector planCollectorRow
	err := tx.GetContext(ctx, &collector, `
		SELECT id, organization_id, name, operator_id, metadata
		FROM data_collectors
		WHERE operator_id = ? AND deleted_at IS NULL
		LIMIT 1`+forUpdateClause(tx), strings.TrimSpace(plan.Operator))
	if err == sql.ErrNoRows {
		return collector, fmt.Errorf("collector_missing")
	}
	if err != nil {
		return collector, fmt.Errorf("collector_query_failed")
	}
	if collector.OrganizationID != plan.WorkspaceID {
		return collector, fmt.Errorf("workspace_mismatch")
	}
	return collector, nil
}

func resolvePlanRobot(ctx context.Context, tx *sqlx.Tx, plan auth.HilbertDCPlan) (planRobotRow, error) {
	var robot planRobotRow
	err := tx.GetContext(ctx, &robot, `
		SELECT id, robot_type_id, device_id, factory_id, metadata
		FROM robots
		WHERE device_id = ? AND deleted_at IS NULL
		LIMIT 1`+forUpdateClause(tx), strconv.FormatInt(plan.DCDeviceID, 10))
	if err == sql.ErrNoRows {
		return robot, fmt.Errorf("robot_missing")
	}
	if err != nil {
		return robot, fmt.Errorf("robot_query_failed")
	}
	workspaceID, ok := int64Metadata(robot.Metadata.String, "hilbert_workspace_id")
	if !ok || workspaceID != plan.WorkspaceID {
		return robot, fmt.Errorf("workspace_mismatch")
	}
	return robot, nil
}

func ensurePlanCompatFactory(ctx context.Context, tx *sqlx.Tx, plan auth.HilbertDCPlan, now time.Time) (int64, error) {
	slug := fmt.Sprintf("hilbert_dc_factory_%d", plan.DCFactoryID)
	name := fmt.Sprintf("Hilbert DC Factory %d", plan.DCFactoryID)
	if tx.DriverName() == "sqlite" {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO factories (name, slug, created_at, updated_at, deleted_at)
			VALUES (?, ?, ?, ?, NULL)
			ON CONFLICT(slug) DO UPDATE SET
				name = excluded.name,
				updated_at = excluded.updated_at,
				deleted_at = NULL
		`, name, slug, now, now); err != nil {
			return 0, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO factories (name, slug, created_at, updated_at, deleted_at)
			VALUES (?, ?, ?, ?, NULL)
			ON DUPLICATE KEY UPDATE
				name = VALUES(name),
				updated_at = VALUES(updated_at),
				deleted_at = NULL
		`, name, slug, now, now); err != nil {
			return 0, err
		}
	}
	var id int64
	if err := tx.GetContext(ctx, &id, "SELECT id FROM factories WHERE slug = ? AND deleted_at IS NULL LIMIT 1", slug); err != nil {
		return 0, err
	}
	return id, nil
}

func ensurePlanCompatSOP(ctx context.Context, tx *sqlx.Tx, plan auth.HilbertDCPlan, now time.Time) (int64, error) {
	slug := "hilbert_dc_plan_" + strings.TrimSpace(plan.DCType)
	description := "Hilbert dc_plan compatibility SOP"
	if tx.DriverName() == "sqlite" {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO sops (slug, description, version, created_at, updated_at, deleted_at)
			VALUES (?, ?, ?, ?, ?, NULL)
			ON CONFLICT(slug, version) DO UPDATE SET
				description = excluded.description,
				updated_at = excluded.updated_at,
				deleted_at = NULL
		`, slug, description, dcPlanCompatSOPVersion, now, now); err != nil {
			return 0, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO sops (slug, description, version, created_at, updated_at, deleted_at)
			VALUES (?, ?, ?, ?, ?, NULL)
			ON DUPLICATE KEY UPDATE
				description = VALUES(description),
				updated_at = VALUES(updated_at),
				deleted_at = NULL
		`, slug, description, dcPlanCompatSOPVersion, now, now); err != nil {
			return 0, err
		}
	}
	var id int64
	if err := tx.GetContext(ctx, &id, "SELECT id FROM sops WHERE slug = ? AND version = ? AND deleted_at IS NULL LIMIT 1", slug, dcPlanCompatSOPVersion); err != nil {
		return 0, err
	}
	return id, nil
}

func ensurePlanCompatScene(ctx context.Context, tx *sqlx.Tx, plan auth.HilbertDCPlan, factoryID int64, now time.Time) (planSceneRow, error) {
	sceneName := "Hilbert " + strings.TrimSpace(plan.DCType)
	layout, err := marshalMetadata(map[string]any{
		"source":       "hilbert_dc_plan",
		"dc_plan_id":   plan.ID,
		"dc_type":      strings.TrimSpace(plan.DCType),
		"dc_device_id": plan.DCDeviceID,
		"workspace_id": plan.WorkspaceID,
		"last_seen_at": now.Format(time.RFC3339),
	})
	if err != nil {
		return planSceneRow{}, err
	}
	if tx.DriverName() == "sqlite" {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO scenes (factory_id, name, description, initial_scene_layout_template, created_at, updated_at, deleted_at)
			VALUES (?, ?, ?, ?, ?, ?, NULL)
			ON CONFLICT(name) DO UPDATE SET
				factory_id = excluded.factory_id,
				description = excluded.description,
				initial_scene_layout_template = excluded.initial_scene_layout_template,
				updated_at = excluded.updated_at,
				deleted_at = NULL
		`, factoryID, sceneName, "Hilbert dc_plan compatibility scene", layout, now, now); err != nil {
			return planSceneRow{}, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO scenes (factory_id, name, description, initial_scene_layout_template, created_at, updated_at, deleted_at)
			VALUES (?, ?, ?, ?, ?, ?, NULL)
			ON DUPLICATE KEY UPDATE
				factory_id = VALUES(factory_id),
				description = VALUES(description),
				initial_scene_layout_template = VALUES(initial_scene_layout_template),
				updated_at = VALUES(updated_at),
				deleted_at = NULL
		`, factoryID, sceneName, "Hilbert dc_plan compatibility scene", layout, now, now); err != nil {
			return planSceneRow{}, err
		}
	}

	var sceneID int64
	if err := tx.GetContext(ctx, &sceneID, "SELECT id FROM scenes WHERE name = ? AND deleted_at IS NULL LIMIT 1", sceneName); err != nil {
		return planSceneRow{}, err
	}
	if tx.DriverName() == "sqlite" {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO subscenes (scene_id, name, description, initial_scene_layout, created_at, updated_at, deleted_at)
			VALUES (?, ?, ?, ?, ?, ?, NULL)
			ON CONFLICT(scene_id, name) DO UPDATE SET
				description = excluded.description,
				initial_scene_layout = excluded.initial_scene_layout,
				updated_at = excluded.updated_at,
				deleted_at = NULL
		`, sceneID, dcPlanCompatSubsceneName, "Hilbert dc_plan compatibility subscene", layout, now, now); err != nil {
			return planSceneRow{}, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO subscenes (scene_id, name, description, initial_scene_layout, created_at, updated_at, deleted_at)
			VALUES (?, ?, ?, ?, ?, ?, NULL)
			ON DUPLICATE KEY UPDATE
				description = VALUES(description),
				initial_scene_layout = VALUES(initial_scene_layout),
				updated_at = VALUES(updated_at),
				deleted_at = NULL
		`, sceneID, dcPlanCompatSubsceneName, "Hilbert dc_plan compatibility subscene", layout, now, now); err != nil {
			return planSceneRow{}, err
		}
	}

	var scene planSceneRow
	if err := tx.GetContext(ctx, &scene, `
		SELECT
			s.id AS id,
			s.name AS name,
			ss.id AS subscene_id,
			ss.name AS subscene_name,
			COALESCE(ss.initial_scene_layout, '') AS layout
		FROM scenes s
		JOIN subscenes ss ON ss.scene_id = s.id AND ss.deleted_at IS NULL
		WHERE s.id = ? AND ss.name = ? AND s.deleted_at IS NULL
		LIMIT 1`, sceneID, dcPlanCompatSubsceneName); err != nil {
		return planSceneRow{}, err
	}
	return scene, nil
}

func ensurePlanWorkstation(
	ctx context.Context,
	tx *sqlx.Tx,
	plan auth.HilbertDCPlan,
	collector planCollectorRow,
	robot planRobotRow,
	factoryID int64,
	now time.Time,
) (planWorkstationRow, error) {
	var ws planWorkstationRow
	err := tx.GetContext(ctx, &ws, `
		SELECT id, COALESCE(name, '') AS name, COALESCE(robot_name, '') AS robot_name,
			COALESCE(robot_serial, '') AS robot_serial, COALESCE(collector_name, '') AS collector_name,
			COALESCE(collector_operator_id, '') AS collector_operator_id
		FROM workstations
		WHERE robot_id = ? AND data_collector_id = ? AND is_current = TRUE AND deleted_at IS NULL
		LIMIT 1`+forUpdateClause(tx), robot.ID, collector.ID)
	if err == nil {
		return ws, nil
	}
	if err != sql.ErrNoRows {
		return ws, fmt.Errorf("workstation_query_failed")
	}
	if hasCurrentBinding(ctx, tx, "robot_id", robot.ID) || hasCurrentBinding(ctx, tx, "data_collector_id", collector.ID) {
		return ws, fmt.Errorf("binding_conflict")
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
			factory_id, organization_id, name, status, metadata, created_at, updated_at, is_current
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, TRUE)
	`, robot.ID, robot.DeviceID, robot.DeviceID, collector.ID, collector.Name, collector.OperatorID,
		factoryID, plan.WorkspaceID, name, "active", metadata, now, now)
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

func ensurePlanCompatOrder(ctx context.Context, tx *sqlx.Tx, plan auth.HilbertDCPlan, sceneID int64, now time.Time) (int64, error) {
	name := fmt.Sprintf("Hilbert DC Plan %d", plan.ID)
	metadata, err := planShellMetadata(plan, now)
	if err != nil {
		return 0, err
	}
	if tx.DriverName() == "sqlite" {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO orders (organization_id, scene_id, name, target_count, priority, status, metadata, created_at, updated_at, deleted_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)
			ON CONFLICT(organization_id, name) DO UPDATE SET
				scene_id = excluded.scene_id,
				target_count = excluded.target_count,
				metadata = excluded.metadata,
				updated_at = excluded.updated_at,
				deleted_at = NULL
		`, plan.WorkspaceID, sceneID, name, plan.TargetCount, "normal", "created", metadata, now, now); err != nil {
			return 0, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO orders (organization_id, scene_id, name, target_count, priority, status, metadata, created_at, updated_at, deleted_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)
			ON DUPLICATE KEY UPDATE
				scene_id = VALUES(scene_id),
				target_count = VALUES(target_count),
				metadata = VALUES(metadata),
				updated_at = VALUES(updated_at),
				deleted_at = NULL
		`, plan.WorkspaceID, sceneID, name, plan.TargetCount, "normal", "created", metadata, now, now); err != nil {
			return 0, err
		}
	}
	var id int64
	if err := tx.GetContext(ctx, &id, "SELECT id FROM orders WHERE organization_id = ? AND name = ? AND deleted_at IS NULL LIMIT 1"+forUpdateClause(tx), plan.WorkspaceID, name); err != nil {
		return 0, err
	}
	return id, nil
}

func ensurePlanCompatBatch(ctx context.Context, tx *sqlx.Tx, plan auth.HilbertDCPlan, orderID int64, ws planWorkstationRow, now time.Time) (planBatchRow, error) {
	publicID := fmt.Sprintf("dcplan_%d", plan.ID)
	var existing struct {
		ID     int64          `db:"id"`
		Name   sql.NullString `db:"name"`
		Status string         `db:"status"`
	}
	err := tx.GetContext(ctx, &existing, `
		SELECT id, name, status
		FROM batches
		WHERE order_id = ? AND batch_id = ? AND deleted_at IS NULL
		LIMIT 1`+forUpdateClause(tx), orderID, publicID)
	if err != nil && err != sql.ErrNoRows {
		return planBatchRow{}, fmt.Errorf("compat_batch_query_failed")
	}
	if err == nil {
		if existing.Status == "completed" || existing.Status == "cancelled" || existing.Status == "recalled" {
			return planBatchRow{}, fmt.Errorf("compat_batch_closed")
		}
		name := batchName(existing.Name, plan)
		metadata, metaErr := planShellMetadata(plan, now)
		if metaErr != nil {
			return planBatchRow{}, fmt.Errorf("compat_batch_metadata_failed")
		}
		if _, updateErr := tx.ExecContext(ctx, `
			UPDATE batches
			SET workstation_id = ?, organization_id = ?, name = ?, episode_count = ?, metadata = ?, updated_at = ?
			WHERE id = ? AND deleted_at IS NULL
		`, ws.ID, plan.WorkspaceID, name, plan.TargetCount, metadata, now, existing.ID); updateErr != nil {
			return planBatchRow{}, fmt.Errorf("compat_batch_update_failed")
		}
		return planBatchRow{ID: existing.ID, Name: name}, nil
	}

	name := fmt.Sprintf("Hilbert DC Plan %d", plan.ID)
	metadata, metaErr := planShellMetadata(plan, now)
	if metaErr != nil {
		return planBatchRow{}, fmt.Errorf("compat_batch_metadata_failed")
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO batches (
			batch_id, order_id, workstation_id, organization_id, name, status, episode_count, metadata, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, publicID, orderID, ws.ID, plan.WorkspaceID, name, "pending", plan.TargetCount, metadata, now, now)
	if err != nil {
		return planBatchRow{}, fmt.Errorf("compat_batch_create_failed")
	}
	id, err := res.LastInsertId()
	if err != nil {
		return planBatchRow{}, fmt.Errorf("compat_batch_id_failed")
	}
	return planBatchRow{ID: id, Name: name}, nil
}

func hasCurrentBinding(ctx context.Context, tx *sqlx.Tx, column string, id int64) bool {
	var count int
	query := fmt.Sprintf("SELECT COUNT(*) FROM workstations WHERE %s = ? AND is_current = TRUE AND deleted_at IS NULL", column) //nolint:gosec // column is hardcoded by caller.
	if err := tx.GetContext(ctx, &count, query, id); err != nil {
		return true
	}
	return count > 0
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

func int64Metadata(raw string, key string) (int64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return 0, false
	}
	value, ok := payload[key]
	if !ok {
		return 0, false
	}
	switch typed := value.(type) {
	case float64:
		return int64(typed), true
	case int64:
		return typed, true
	case int:
		return int64(typed), true
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		return parsed, err == nil
	default:
		return 0, false
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
	})
}

func planShellMetadata(plan auth.HilbertDCPlan, now time.Time) (string, error) {
	return marshalMetadata(map[string]any{
		"source":          "hilbert_dc_plan",
		"workspace_id":    plan.WorkspaceID,
		"dc_plan_id":      plan.ID,
		"dc_plan_name":    strings.TrimSpace(plan.Name),
		"dc_type":         strings.TrimSpace(plan.DCType),
		"dc_device_id":    plan.DCDeviceID,
		"target_count":    plan.TargetCount,
		"target_duration": plan.TargetDuration,
		"last_seen_at":    now.Format(time.RFC3339),
	})
}

func batchName(name sql.NullString, plan auth.HilbertDCPlan) string {
	if name.Valid && strings.TrimSpace(name.String) != "" {
		return strings.TrimSpace(name.String)
	}
	return fmt.Sprintf("Hilbert DC Plan %d", plan.ID)
}
