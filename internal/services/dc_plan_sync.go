// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"archebase.com/keystone-edge/internal/auth"
	"archebase.com/keystone-edge/internal/config"
	"archebase.com/keystone-edge/internal/logger"
	"github.com/jmoiron/sqlx"
)

const (
	dcPlanSyncPageSize     int64 = 200
	dcPlanWorkspaceTimeout       = 30 * time.Second
	dcPlanDeadlockRetries        = 3
)

var (
	// ErrDCPlanSyncNotConfigured indicates Hilbert service identity bootstrap is incomplete.
	ErrDCPlanSyncNotConfigured = errors.New("dc plan sync not configured")
	// ErrDCPlanSyncInvalidWorkspace indicates the requested workspace cannot sync Hilbert dc plans.
	ErrDCPlanSyncInvalidWorkspace = errors.New("dc plan sync invalid workspace")
	// ErrDCPlanSyncFailed indicates Hilbert dc plan sync failed.
	ErrDCPlanSyncFailed = errors.New("dc plan sync failed")
)

// HilbertDCPlanClient captures the Hilbert calls dc plan sync needs.
type HilbertDCPlanClient interface {
	Configured() bool
	ServiceAuthConfigured() bool
	QueryDCPlans(ctx context.Context, workspaceID int64, pageNum int64, pageSize int64) (*auth.HilbertDCPlanPage, error)
}

// DCPlanSyncResult summarizes one Hilbert dc plan sync run.
type DCPlanSyncResult struct {
	WorkspaceID           int64
	SyncedCount           int
	PageCount             int
	LastSyncedAt          time.Time
	WorkstationProjection *DCPlanWorkstationProjectionSummary
}

// DCPlanSyncAllResult summarizes a sync run across every Hilbert workspace.
type DCPlanSyncAllResult struct {
	WorkspaceCount int
	SyncedCount    int
	PageCount      int
	FailedCount    int
	Results        []DCPlanSyncResult
	Errors         []DCPlanSyncWorkspaceError
}

// DCPlanSyncWorkspaceError describes one workspace-level dc_plan sync failure.
type DCPlanSyncWorkspaceError struct {
	WorkspaceID int64
	Error       string
}

// DCPlanSyncService syncs Hilbert dc_plan projections into Keystone.
type DCPlanSyncService struct {
	db            *sqlx.DB
	cfg           *config.HilbertConfig
	hilbertClient HilbertDCPlanClient
	projector     *dcPlanWorkstationProjector
	taskSupply    *DCPlanTaskSupplyService
	syncMu        sync.Mutex
}

// NewDCPlanSyncService creates a DCPlanSyncService.
func NewDCPlanSyncService(db *sqlx.DB, cfg *config.HilbertConfig, hilbertClient HilbertDCPlanClient) *DCPlanSyncService {
	if hilbertClient == nil {
		hilbertClient = auth.NewHilbertClient(cfg)
	}
	return &DCPlanSyncService{
		db:            db,
		cfg:           cfg,
		hilbertClient: hilbertClient,
		projector:     newDCPlanWorkstationProjector(db),
		taskSupply:    NewDCPlanTaskSupplyService(db),
	}
}

// Configured reports whether sync has the required Hilbert service identity settings.
func (s *DCPlanSyncService) Configured() bool {
	if s == nil || s.db == nil || s.cfg == nil || s.hilbertClient == nil || !s.hilbertClient.Configured() {
		return false
	}
	return strings.TrimSpace(s.cfg.BaseURL) != "" && s.hilbertClient.ServiceAuthConfigured()
}

// SyncWorkspace logs into Hilbert, fetches one workspace's dc plans, validates every record, and transactionally upserts them.
func (s *DCPlanSyncService) SyncWorkspace(ctx context.Context, workspaceID int64) (*DCPlanSyncResult, error) {
	if !s.syncMu.TryLock() {
		return nil, fmt.Errorf("dc plan sync already in progress")
	}
	defer s.syncMu.Unlock()
	return s.syncWorkspace(ctx, workspaceID)
}

func (s *DCPlanSyncService) syncWorkspace(ctx context.Context, workspaceID int64) (*DCPlanSyncResult, error) {
	workspaceCtx, cancel := context.WithTimeout(ctx, dcPlanWorkspaceTimeout)
	defer cancel()
	ctx = workspaceCtx
	if !s.Configured() {
		return nil, ErrDCPlanSyncNotConfigured
	}
	if err := s.requireHilbertWorkspace(ctx, workspaceID); err != nil {
		return nil, err
	}
	plans, pageCount, err := s.fetchAllPlans(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	if err := validateHilbertDCPlans(workspaceID, plans); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDCPlanSyncFailed, err)
	}

	now := time.Now().UTC()
	changedPlans, err := s.upsertDCPlansWithRetry(ctx, workspaceID, plans, now)
	if err != nil {
		return nil, fmt.Errorf("%w: upsert dc plans: %v", ErrDCPlanSyncFailed, err)
	}
	projectionPlans, err := s.workstationProjectionPlans(ctx, workspaceID, plans, changedPlans)
	if err != nil {
		return nil, fmt.Errorf("%w: select workstation repair plans: %v", ErrDCPlanSyncFailed, err)
	}
	projection := s.projectPlansWithRetry(ctx, projectionPlans, now)
	poolPlans, poolTasks, poolFailures := s.maintainEgoPortalPendingPools(ctx, changedPlans, now)
	logger.Printf("[DC_PLAN] Hilbert dc plan sync committed: workspace_id=%d synced_count=%d page_count=%d", workspaceID, len(plans), pageCount)
	logger.Printf(
		"[DC_PLAN] Hilbert workstation projection completed: workspace_id=%d plans=%d created=%d reused=%d blocked=%d",
		workspaceID,
		projection.TotalPlans,
		projection.CreatedCount,
		projection.ReusedCount,
		projection.BlockedCount,
	)
	logger.Printf(
		"[DC_PLAN] Ego Portal pending pool maintenance completed: workspace_id=%d plans=%d created=%d failed=%d",
		workspaceID,
		poolPlans,
		poolTasks,
		poolFailures,
	)

	return &DCPlanSyncResult{
		WorkspaceID:           workspaceID,
		SyncedCount:           len(plans),
		PageCount:             pageCount,
		LastSyncedAt:          now,
		WorkstationProjection: projection,
	}, nil
}

func (s *DCPlanSyncService) upsertDCPlansWithRetry(ctx context.Context, workspaceID int64, plans []auth.HilbertDCPlan, syncedAt time.Time) ([]auth.HilbertDCPlan, error) {
	var changed []auth.HilbertDCPlan
	var err error
	for attempt := 0; attempt < dcPlanDeadlockRetries; attempt++ {
		changed, err = s.upsertDCPlans(ctx, workspaceID, plans, syncedAt)
		if err == nil || !isRetryableMySQLLockError(err) {
			return changed, err
		}
		if err := waitDCPlanRetry(ctx, attempt); err != nil {
			return nil, err
		}
	}
	return changed, err
}
func (s *DCPlanSyncService) projectPlansWithRetry(ctx context.Context, plans []auth.HilbertDCPlan, now time.Time) *DCPlanWorkstationProjectionSummary {
	summary := &DCPlanWorkstationProjectionSummary{TotalPlans: len(plans)}
	for _, plan := range plans {
		created, err := s.projectWithRetry(ctx, plan, now)
		if err != nil {
			summary.BlockedCount++
			logger.Printf("[DC_PLAN] Workstation projection blocked: workspace_id=%d dc_plan_id=%d operator=%s dc_device_id=%v err=%v", plan.WorkspaceID, plan.ID, strings.TrimSpace(plan.Operator), nullableInt64ForLog(plan.DCDeviceID), err)
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

func (s *DCPlanSyncService) projectWithRetry(ctx context.Context, plan auth.HilbertDCPlan, now time.Time) (bool, error) {
	var created bool
	var err error
	for attempt := 0; attempt < dcPlanDeadlockRetries; attempt++ {
		created, err = s.projector.projectPlan(ctx, plan, now)
		if err == nil || !isRetryableMySQLLockError(err) {
			return created, err
		}
		if err := waitDCPlanRetry(ctx, attempt); err != nil {
			return created, err
		}
	}
	return created, err
}

func isRetryableMySQLLockError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "deadlock") || strings.Contains(message, "lock wait timeout") || strings.Contains(message, "error 1213")
}
func waitDCPlanRetry(ctx context.Context, attempt int) error {
	timer := time.NewTimer(time.Duration(attempt+1) * 100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *DCPlanSyncService) ensurePendingPoolWithRetry(ctx context.Context, planID int64, now time.Time) (*EgoPortalPendingPoolResult, error) {
	var result *EgoPortalPendingPoolResult
	var err error
	for attempt := 0; attempt < dcPlanDeadlockRetries; attempt++ {
		result, err = s.taskSupply.EnsureEgoPortalPendingPool(ctx, planID, now)
		if err == nil || !isRetryableMySQLLockError(err) {
			return result, err
		}
		if waitErr := waitDCPlanRetry(ctx, attempt); waitErr != nil {
			return nil, waitErr
		}
	}
	return result, err
}
func (s *DCPlanSyncService) maintainEgoPortalPendingPools(
	ctx context.Context,
	plans []auth.HilbertDCPlan,
	now time.Time,
) (int, int, int) {
	enabledCount := 0
	createdCount := 0
	failedCount := 0
	for _, plan := range plans {
		if plan.DCDeviceID == nil || strings.EqualFold(strings.TrimSpace(plan.Status), "collected") {
			continue
		}
		result, err := s.ensurePendingPoolWithRetry(ctx, plan.ID, now)
		if err != nil {
			failedCount++
			logger.Printf(
				"[DC_PLAN] Pending pool maintenance blocked: workspace_id=%d dc_plan_id=%d err=%v",
				plan.WorkspaceID,
				plan.ID,
				err,
			)
			continue
		}
		if result.Enabled {
			enabledCount++
			createdCount += result.CreatedCount
		}
	}
	return enabledCount, createdCount, failedCount
}

func (s *DCPlanSyncService) workstationProjectionPlans(
	ctx context.Context,
	workspaceID int64,
	allPlans []auth.HilbertDCPlan,
	changedPlans []auth.HilbertDCPlan,
) ([]auth.HilbertDCPlan, error) {
	var missingIDs []int64
	if err := s.db.SelectContext(ctx, &missingIDs, `
		SELECT dp.id
		FROM dc_plan dp
		LEFT JOIN robots r
		  ON r.device_id = CAST(dp.dc_device_id AS CHAR)
		 AND r.deleted_at IS NULL
		LEFT JOIN data_collectors dc
		  ON dc.operator_id = dp.operator
		 AND dc.deleted_at IS NULL
		LEFT JOIN workstations ws
		  ON ws.robot_id = r.id
		 AND ws.data_collector_id = dc.id
		 AND ws.workspace_id = dp.workspace_id
		 AND ws.deleted_at IS NULL
		 AND ws.superseded_at IS NULL
		WHERE dp.workspace_id = ?
		  AND dp.deleted_at IS NULL
		  AND dp.dc_device_id IS NOT NULL
		  AND LOWER(COALESCE(dp.status, '')) <> 'collected'
		  AND ws.id IS NULL
	`, workspaceID); err != nil {
		if s.db.DriverName() == "sqlite" && strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return changedPlans, nil
		}
		return nil, err
	}
	missing := make(map[int64]struct{}, len(missingIDs))
	for _, id := range missingIDs {
		missing[id] = struct{}{}
	}
	plansByID := make(map[int64]auth.HilbertDCPlan, len(allPlans))
	for _, plan := range allPlans {
		plansByID[plan.ID] = plan
	}
	selected := make(map[int64]auth.HilbertDCPlan, len(changedPlans)+len(missingIDs))
	for _, plan := range changedPlans {
		selected[plan.ID] = plan
	}
	for id := range missing {
		if plan, ok := plansByID[id]; ok {
			selected[id] = plan
		}
	}
	result := make([]auth.HilbertDCPlan, 0, len(selected))
	for _, plan := range allPlans {
		if _, ok := selected[plan.ID]; ok {
			result = append(result, plan)
		}
	}
	return result, nil
}

// SyncAllWorkspaces syncs dc_plan projections for every local Hilbert workspace.
func (s *DCPlanSyncService) SyncAllWorkspaces(ctx context.Context) (*DCPlanSyncAllResult, error) {
	if !s.Configured() {
		return nil, ErrDCPlanSyncNotConfigured
	}
	if !s.syncMu.TryLock() {
		return nil, fmt.Errorf("dc plan sync already in progress")
	}
	defer s.syncMu.Unlock()

	workspaceIDs, err := s.listHilbertWorkspaceIDs(ctx)
	if err != nil {
		return nil, err
	}

	result := &DCPlanSyncAllResult{
		WorkspaceCount: len(workspaceIDs),
		Results:        []DCPlanSyncResult{},
		Errors:         []DCPlanSyncWorkspaceError{},
	}
	for _, workspaceID := range workspaceIDs {
		workspaceCtx, cancel := context.WithTimeout(ctx, dcPlanWorkspaceTimeout)
		workspaceResult, syncErr := s.syncWorkspace(workspaceCtx, workspaceID)
		cancel()
		if syncErr != nil {
			result.FailedCount++
			result.Errors = append(result.Errors, DCPlanSyncWorkspaceError{
				WorkspaceID: workspaceID,
				Error:       syncErr.Error(),
			})
			logger.Printf("[DC_PLAN] Hilbert dc plan sync failed for workspace_id=%d: %v", workspaceID, syncErr)
			continue
		}
		result.SyncedCount += workspaceResult.SyncedCount
		result.PageCount += workspaceResult.PageCount
		result.Results = append(result.Results, *workspaceResult)
	}

	logger.Printf(
		"[DC_PLAN] Hilbert dc plan sync completed: workspaces=%d failed=%d synced_count=%d page_count=%d",
		result.WorkspaceCount,
		result.FailedCount,
		result.SyncedCount,
		result.PageCount,
	)
	return result, nil
}

func (s *DCPlanSyncService) listHilbertWorkspaceIDs(ctx context.Context) ([]int64, error) {
	workspaceIDs := []int64{}
	if err := s.db.SelectContext(ctx, &workspaceIDs, `
		SELECT id
		FROM workspaces
		WHERE id > ? AND source = ? AND deleted_at IS NULL
		ORDER BY id
	`, defaultWorkspaceID, workspaceSourceHilbert); err != nil {
		return nil, fmt.Errorf("%w: query hilbert workspaces: %v", ErrDCPlanSyncFailed, err)
	}
	return workspaceIDs, nil
}

func (s *DCPlanSyncService) requireHilbertWorkspace(ctx context.Context, workspaceID int64) error {
	if workspaceID <= defaultWorkspaceID {
		return fmt.Errorf("%w: default workspace cannot sync Hilbert dc plans", ErrDCPlanSyncInvalidWorkspace)
	}
	var source string
	if err := s.db.GetContext(ctx, &source, "SELECT source FROM workspaces WHERE id = ? AND deleted_at IS NULL", workspaceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: workspace not found", ErrDCPlanSyncInvalidWorkspace)
		}
		return fmt.Errorf("%w: query workspace: %v", ErrDCPlanSyncFailed, err)
	}
	if source != workspaceSourceHilbert {
		return fmt.Errorf("%w: workspace is not a Hilbert projection", ErrDCPlanSyncInvalidWorkspace)
	}
	return nil
}

func (s *DCPlanSyncService) fetchAllPlans(ctx context.Context, workspaceID int64) ([]auth.HilbertDCPlan, int, error) {
	plans := []auth.HilbertDCPlan{}
	pageCount := 0
	for pageNum := int64(1); ; pageNum++ {
		page, err := s.hilbertClient.QueryDCPlans(ctx, workspaceID, pageNum, dcPlanSyncPageSize)
		if err != nil {
			return nil, pageCount, fmt.Errorf("%w: query dc plans: %v", ErrDCPlanSyncFailed, err)
		}
		if page == nil {
			return nil, pageCount, fmt.Errorf("%w: empty dc plan page", ErrDCPlanSyncFailed)
		}
		pageCount++
		plans = append(plans, page.Records...)
		if page.Total <= 0 || int64(len(plans)) >= page.Total || len(page.Records) == 0 {
			break
		}
	}
	return plans, pageCount, nil
}

func validateHilbertDCPlans(workspaceID int64, plans []auth.HilbertDCPlan) error {
	seen := make(map[int64]struct{}, len(plans))
	for _, plan := range plans {
		if plan.ID <= 0 {
			return fmt.Errorf("invalid hilbert dc plan id %d", plan.ID)
		}
		if plan.WorkspaceID != workspaceID {
			return fmt.Errorf("dc plan %d belongs to workspace %d, want %d", plan.ID, plan.WorkspaceID, workspaceID)
		}
		if strings.TrimSpace(plan.Name) == "" ||
			strings.TrimSpace(plan.Operator) == "" ||
			strings.TrimSpace(plan.DCType) == "" ||
			strings.TrimSpace(plan.CreatedBy) == "" {
			return fmt.Errorf("dc plan %d has empty required string field", plan.ID)
		}
		if strings.TrimSpace(plan.DCDate) == "" {
			return fmt.Errorf("dc plan %d has empty dc_date", plan.ID)
		}
		if parsed, err := time.Parse("2006-01-02", strings.TrimSpace(plan.DCDate)); err != nil || parsed.Format("2006-01-02") != strings.TrimSpace(plan.DCDate) {
			return fmt.Errorf("dc plan %d has invalid dc_date", plan.ID)
		}
		if plan.DCFactoryID <= 0 ||
			plan.DCServiceProviderID <= 0 ||
			plan.DCProjectID <= 0 ||
			plan.DCTaskID <= 0 ||
			(plan.DCDeviceID != nil && *plan.DCDeviceID <= 0) ||
			plan.TargetCount <= 0 ||
			plan.CurCount < 0 ||
			plan.TargetDuration <= 0 ||
			plan.CurDuration < 0 {
			return fmt.Errorf("dc plan %d has invalid numeric field", plan.ID)
		}
		if plan.CreatedTime.IsZero() {
			return fmt.Errorf("dc plan %d has empty created_time", plan.ID)
		}
		if _, ok := seen[plan.ID]; ok {
			return fmt.Errorf("duplicate hilbert dc plan id %d", plan.ID)
		}
		seen[plan.ID] = struct{}{}
	}
	return nil
}

func (s *DCPlanSyncService) upsertDCPlans(ctx context.Context, workspaceID int64, plans []auth.HilbertDCPlan, syncedAt time.Time) ([]auth.HilbertDCPlan, error) {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	for _, plan := range plans {
		var existingWorkspaceID int64
		err := tx.GetContext(ctx, &existingWorkspaceID, "SELECT workspace_id FROM dc_plan WHERE id = ? AND deleted_at IS NULL", plan.ID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err == nil && existingWorkspaceID != workspaceID {
			return nil, fmt.Errorf("dc plan %d already belongs to workspace %d", plan.ID, existingWorkspaceID)
		}
	}

	type existingDCPlan struct {
		ID         int64          `db:"id"`
		RawPayload sql.NullString `db:"raw_payload"`
		DeletedAt  sql.NullTime   `db:"deleted_at"`
	}
	var existing []existingDCPlan
	if err := tx.SelectContext(ctx, &existing, `
		SELECT id, raw_payload, deleted_at
		FROM dc_plan
		WHERE workspace_id = ?`, workspaceID); err != nil {
		return nil, err
	}
	existingByID := make(map[int64]existingDCPlan, len(existing))
	for _, row := range existing {
		existingByID[row.ID] = row
	}

	incomingIDSet := make(map[int64]struct{}, len(plans))
	changedPlans := make([]auth.HilbertDCPlan, 0, len(plans))
	for _, plan := range plans {
		incomingIDSet[plan.ID] = struct{}{}
		rawPayload, err := json.Marshal(plan)
		if err != nil {
			return nil, err
		}
		row, found := existingByID[plan.ID]
		if !found || row.DeletedAt.Valid || !row.RawPayload.Valid || row.RawPayload.String != string(rawPayload) {
			changedPlans = append(changedPlans, plan)
		}
	}

	// Only deactivate active local plans that disappeared from the remote snapshot.
	for _, row := range existing {
		if row.DeletedAt.Valid {
			continue
		}
		if _, present := incomingIDSet[row.ID]; present {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE dc_plan
			SET deleted_at = ?, local_updated_at = ?
			WHERE id = ? AND workspace_id = ? AND deleted_at IS NULL
		`, syncedAt, syncedAt, row.ID, workspaceID); err != nil {
			return nil, err
		}
	}

	for _, plan := range changedPlans {
		if err := upsertDCPlan(ctx, tx, plan, syncedAt); err != nil {
			return nil, err
		}
		if strings.EqualFold(strings.TrimSpace(plan.Status), "collected") {
			if _, err := tx.ExecContext(ctx, `
				UPDATE tasks
				SET status = 'cancelled', updated_at = ?
				WHERE dc_plan_id = ? AND status = 'pending' AND deleted_at IS NULL
			`, syncedAt, plan.ID); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return changedPlans, nil
}

func upsertDCPlan(ctx context.Context, tx *sqlx.Tx, plan auth.HilbertDCPlan, syncedAt time.Time) error {
	rawPayload, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	description := sql.NullString{}
	if plan.Description != nil && strings.TrimSpace(*plan.Description) != "" {
		description = sql.NullString{String: strings.TrimSpace(*plan.Description), Valid: true}
	}
	updatedBy := sql.NullString{}
	if plan.UpdatedBy != nil && strings.TrimSpace(*plan.UpdatedBy) != "" {
		updatedBy = sql.NullString{String: strings.TrimSpace(*plan.UpdatedBy), Valid: true}
	}
	updatedTime := sql.NullTime{}
	if plan.UpdatedTime != nil && !plan.UpdatedTime.IsZero() {
		updatedTime = sql.NullTime{Time: plan.UpdatedTime.UTC(), Valid: true}
	}

	args := []any{
		plan.ID,
		plan.WorkspaceID,
		strings.TrimSpace(plan.Name),
		strings.TrimSpace(plan.Status),
		description,
		plan.DCFactoryID,
		plan.DCServiceProviderID,
		strings.TrimSpace(plan.Operator),
		nullableString(plan.OperatorDisplayName),
		plan.DCProjectID,
		nullableString(plan.DCProjectName),
		nullableString(plan.DCProjectDescription),
		plan.DCTaskID,
		nullableString(plan.DCTaskName),
		nullableString(plan.DCTaskDescription),
		plan.DCDeviceID,
		nullableString(plan.DCDeviceName),
		strings.TrimSpace(plan.DCType),
		strings.TrimSpace(plan.DCDate),
		plan.TargetCount,
		plan.CurCount,
		plan.TargetDuration,
		plan.CurDuration,
		strings.TrimSpace(plan.CreatedBy),
		plan.CreatedTime.UTC(),
		updatedBy,
		updatedTime,
		string(rawPayload),
		syncedAt,
		sql.NullString{},
		syncedAt,
		syncedAt,
	}

	if tx.DriverName() == "sqlite" {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO dc_plan (
				id, workspace_id, name, status, description, dc_factory_id, dc_service_provider_id,
				operator, operator_display_name, dc_project_id, dc_project_name, dc_project_description, dc_task_id, dc_task_name, dc_task_description, dc_device_id, dc_device_name, dc_type, dc_date,
				target_count, cur_count, target_duration, cur_duration, created_by, created_time,
				updated_by, updated_time, raw_payload, last_synced_at, sync_error,
				local_created_at, local_updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				workspace_id = excluded.workspace_id,
				name = excluded.name,
				status = excluded.status,
				description = excluded.description,
				dc_factory_id = excluded.dc_factory_id,
				dc_service_provider_id = excluded.dc_service_provider_id,
				operator = excluded.operator,
				operator_display_name = excluded.operator_display_name,
				dc_project_id = excluded.dc_project_id,
				dc_project_name = excluded.dc_project_name,
				dc_project_description = excluded.dc_project_description,
				dc_task_id = excluded.dc_task_id,
				dc_task_name = excluded.dc_task_name,
				dc_task_description = excluded.dc_task_description,
				dc_device_id = excluded.dc_device_id,
				dc_device_name = excluded.dc_device_name,
				dc_type = excluded.dc_type,
				dc_date = excluded.dc_date,
				target_count = excluded.target_count,
				cur_count = excluded.cur_count,
				target_duration = excluded.target_duration,
				cur_duration = excluded.cur_duration,
				created_by = excluded.created_by,
				created_time = excluded.created_time,
				updated_by = excluded.updated_by,
				updated_time = excluded.updated_time,
				raw_payload = excluded.raw_payload,
				last_synced_at = excluded.last_synced_at,
				sync_error = NULL,
				local_updated_at = excluded.local_updated_at,
				deleted_at = NULL
		`, args...)
		return err
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO dc_plan (
			id, workspace_id, name, status, description, dc_factory_id, dc_service_provider_id,
			operator, operator_display_name, dc_project_id, dc_project_name, dc_project_description, dc_task_id, dc_task_name, dc_task_description, dc_device_id, dc_device_name, dc_type, dc_date,
			target_count, cur_count, target_duration, cur_duration, created_by, created_time,
			updated_by, updated_time, raw_payload, last_synced_at, sync_error,
			local_created_at, local_updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			workspace_id = VALUES(workspace_id),
			name = VALUES(name),
			status = VALUES(status),
			description = VALUES(description),
			dc_factory_id = VALUES(dc_factory_id),
			dc_service_provider_id = VALUES(dc_service_provider_id),
			operator = VALUES(operator),
			operator_display_name = VALUES(operator_display_name),
			dc_project_id = VALUES(dc_project_id),
			dc_project_name = VALUES(dc_project_name),
			dc_project_description = VALUES(dc_project_description),
			dc_task_id = VALUES(dc_task_id),
			dc_task_name = VALUES(dc_task_name),
			dc_task_description = VALUES(dc_task_description),
			dc_device_id = VALUES(dc_device_id),
			dc_device_name = VALUES(dc_device_name),
			dc_type = VALUES(dc_type),
			dc_date = VALUES(dc_date),
			target_count = VALUES(target_count),
			cur_count = VALUES(cur_count),
			target_duration = VALUES(target_duration),
			cur_duration = VALUES(cur_duration),
			created_by = VALUES(created_by),
			created_time = VALUES(created_time),
			updated_by = VALUES(updated_by),
			updated_time = VALUES(updated_time),
			raw_payload = VALUES(raw_payload),
			last_synced_at = VALUES(last_synced_at),
			sync_error = NULL,
			local_updated_at = VALUES(local_updated_at),
			deleted_at = NULL
	`, args...)
	return err
}
