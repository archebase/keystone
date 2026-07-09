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
	"time"

	"archebase.com/keystone-edge/internal/auth"
	"archebase.com/keystone-edge/internal/config"
	"archebase.com/keystone-edge/internal/logger"
	"github.com/jmoiron/sqlx"
)

const dcPlanSyncPageSize int64 = 200

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
	Login(ctx context.Context, code string, password string) (*auth.HilbertLoginResult, error)
	QueryDCPlans(ctx context.Context, sessionKey string, workspaceID int64, pageNum int64, pageSize int64) (*auth.HilbertDCPlanPage, error)
}

// DCPlanSyncResult summarizes one Hilbert dc plan sync run.
type DCPlanSyncResult struct {
	WorkspaceID    int64
	SyncedCount    int
	PageCount      int
	LastSyncedAt   time.Time
	TaskGeneration *DCPlanTaskGenerationSummary
}

// DCPlanSyncService syncs Hilbert dc_plan projections into Keystone.
type DCPlanSyncService struct {
	db            *sqlx.DB
	cfg           *config.HilbertConfig
	hilbertClient HilbertDCPlanClient
	taskGenerator *DCPlanTaskGenerationService
}

// NewDCPlanSyncService creates a DCPlanSyncService.
func NewDCPlanSyncService(db *sqlx.DB, cfg *config.HilbertConfig, hilbertClient HilbertDCPlanClient) *DCPlanSyncService {
	if hilbertClient == nil {
		hilbertClient = auth.NewHilbertClient(cfg)
	}
	return &DCPlanSyncService{db: db, cfg: cfg, hilbertClient: hilbertClient, taskGenerator: NewDCPlanTaskGenerationService(db)}
}

// Configured reports whether sync has the required Hilbert service identity settings.
func (s *DCPlanSyncService) Configured() bool {
	if s == nil || s.db == nil || s.cfg == nil || s.hilbertClient == nil || !s.hilbertClient.Configured() {
		return false
	}
	return strings.TrimSpace(s.cfg.BaseURL) != "" &&
		strings.TrimSpace(s.cfg.ServiceAccountCode) != "" &&
		strings.TrimSpace(s.cfg.ServiceAccountPassword) != ""
}

// SyncWorkspace logs into Hilbert, fetches one workspace's dc plans, validates every record, and transactionally upserts them.
func (s *DCPlanSyncService) SyncWorkspace(ctx context.Context, workspaceID int64) (*DCPlanSyncResult, error) {
	if !s.Configured() {
		return nil, ErrDCPlanSyncNotConfigured
	}
	if err := s.requireHilbertWorkspace(ctx, workspaceID); err != nil {
		return nil, err
	}

	loginResult, err := s.hilbertClient.Login(ctx, s.cfg.ServiceAccountCode, s.cfg.ServiceAccountPassword)
	if err != nil {
		return nil, fmt.Errorf("%w: login hilbert: %v", ErrDCPlanSyncFailed, err)
	}
	sessionKey := loginResult.SessionKey()
	if strings.TrimSpace(sessionKey) == "" {
		return nil, fmt.Errorf("%w: login hilbert: missing session key", ErrDCPlanSyncFailed)
	}

	plans, pageCount, err := s.fetchAllPlans(ctx, sessionKey, workspaceID)
	if err != nil {
		return nil, err
	}
	if err := validateHilbertDCPlans(workspaceID, plans); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDCPlanSyncFailed, err)
	}

	now := time.Now().UTC()
	if err := s.upsertDCPlans(ctx, workspaceID, plans, now); err != nil {
		return nil, fmt.Errorf("%w: upsert dc plans: %v", ErrDCPlanSyncFailed, err)
	}
	taskGeneration := s.taskGenerator.GenerateForPlans(ctx, plans, now)
	logger.Printf("[DC_PLAN] Hilbert dc plan sync committed: workspace_id=%d synced_count=%d page_count=%d", workspaceID, len(plans), pageCount)

	return &DCPlanSyncResult{
		WorkspaceID:    workspaceID,
		SyncedCount:    len(plans),
		PageCount:      pageCount,
		LastSyncedAt:   now,
		TaskGeneration: taskGeneration,
	}, nil
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

func (s *DCPlanSyncService) fetchAllPlans(ctx context.Context, sessionKey string, workspaceID int64) ([]auth.HilbertDCPlan, int, error) {
	plans := []auth.HilbertDCPlan{}
	pageCount := 0
	for pageNum := int64(1); ; pageNum++ {
		page, err := s.hilbertClient.QueryDCPlans(ctx, sessionKey, workspaceID, pageNum, dcPlanSyncPageSize)
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
			plan.DCDeviceID <= 0 ||
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

func (s *DCPlanSyncService) upsertDCPlans(ctx context.Context, workspaceID int64, plans []auth.HilbertDCPlan, syncedAt time.Time) error {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for _, plan := range plans {
		var existingWorkspaceID int64
		err := tx.GetContext(ctx, &existingWorkspaceID, "SELECT workspace_id FROM dc_plan WHERE id = ? AND deleted_at IS NULL", plan.ID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && existingWorkspaceID != workspaceID {
			return fmt.Errorf("dc plan %d already belongs to workspace %d", plan.ID, existingWorkspaceID)
		}
	}

	for _, plan := range plans {
		if err := upsertDCPlan(ctx, tx, plan, syncedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
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
		description,
		plan.DCFactoryID,
		plan.DCServiceProviderID,
		strings.TrimSpace(plan.Operator),
		plan.DCProjectID,
		plan.DCTaskID,
		plan.DCDeviceID,
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
				id, workspace_id, name, description, dc_factory_id, dc_service_provider_id,
				operator, dc_project_id, dc_task_id, dc_device_id, dc_type, dc_date,
				target_count, cur_count, target_duration, cur_duration, created_by, created_time,
				updated_by, updated_time, raw_payload, last_synced_at, sync_error,
				local_created_at, local_updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				workspace_id = excluded.workspace_id,
				name = excluded.name,
				description = excluded.description,
				dc_factory_id = excluded.dc_factory_id,
				dc_service_provider_id = excluded.dc_service_provider_id,
				operator = excluded.operator,
				dc_project_id = excluded.dc_project_id,
				dc_task_id = excluded.dc_task_id,
				dc_device_id = excluded.dc_device_id,
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
			id, workspace_id, name, description, dc_factory_id, dc_service_provider_id,
			operator, dc_project_id, dc_task_id, dc_device_id, dc_type, dc_date,
			target_count, cur_count, target_duration, cur_duration, created_by, created_time,
			updated_by, updated_time, raw_payload, last_synced_at, sync_error,
			local_created_at, local_updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			workspace_id = VALUES(workspace_id),
			name = VALUES(name),
			description = VALUES(description),
			dc_factory_id = VALUES(dc_factory_id),
			dc_service_provider_id = VALUES(dc_service_provider_id),
			operator = VALUES(operator),
			dc_project_id = VALUES(dc_project_id),
			dc_task_id = VALUES(dc_task_id),
			dc_device_id = VALUES(dc_device_id),
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
