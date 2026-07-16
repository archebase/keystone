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
	"archebase.com/keystone-edge/internal/logger"
	"github.com/jmoiron/sqlx"
)

const (
	hilbertMetadataSource       = "hilbert"
	hilbertResourceActiveStatus = "active"
)

// HilbertWorkspaceResourceClient captures Hilbert resource calls workspace resource sync needs.
type HilbertWorkspaceResourceClient interface {
	QueryAccountByCode(ctx context.Context, code string) (*auth.HilbertAccount, error)
	QueryDCDevices(ctx context.Context, workspaceID int64) (*auth.HilbertDCDevicePage, error)
	QueryDCDeviceTypeByID(ctx context.Context, id int64) (*auth.HilbertDCDeviceType, error)
}

// WorkspaceResourceSyncSummary summarizes a workspace resource sync run.
type WorkspaceResourceSyncSummary struct {
	Enabled                bool                          `json:"enabled"`
	CollectorUpsertedCount int                           `json:"collector_upserted_count"`
	CollectorSkippedCount  int                           `json:"collector_skipped_count"`
	RobotUpsertedCount     int                           `json:"robot_upserted_count"`
	WorkspaceResults       []WorkspaceResourceSyncResult `json:"workspace_results"`
}

// WorkspaceResourceSyncResult summarizes resource sync for one workspace.
type WorkspaceResourceSyncResult struct {
	WorkspaceID            int64                        `json:"workspace_id"`
	CollectorUpsertedCount int                          `json:"collector_upserted_count"`
	CollectorSkippedCount  int                          `json:"collector_skipped_count"`
	RobotUpsertedCount     int                          `json:"robot_upserted_count"`
	Errors                 []WorkspaceResourceSyncError `json:"errors"`
}

// WorkspaceResourceSyncError describes one skipped or failed resource projection.
type WorkspaceResourceSyncError struct {
	Resource   string `json:"resource"`
	ExternalID string `json:"external_id"`
	Code       string `json:"code"`
	Message    string `json:"message"`
}

// WorkspaceResourceSyncService projects Hilbert workspace resources into Keystone.
type WorkspaceResourceSyncService struct {
	db                  *sqlx.DB
	hilbertClient       HilbertWorkspaceResourceClient
	excludedOperatorIDs map[string]struct{}
}

// NewWorkspaceResourceSyncService creates a resource sync service.
func NewWorkspaceResourceSyncService(db *sqlx.DB, hilbertClient HilbertWorkspaceResourceClient) *WorkspaceResourceSyncService {
	return &WorkspaceResourceSyncService{db: db, hilbertClient: hilbertClient}
}

func newWorkspaceResourceSyncServiceWithExclusions(
	db *sqlx.DB,
	hilbertClient HilbertWorkspaceResourceClient,
	excludedOperatorIDs []string,
) *WorkspaceResourceSyncService {
	excluded := make(map[string]struct{}, len(excludedOperatorIDs))
	for _, operatorID := range normalizeWorkspacePeople(excludedOperatorIDs) {
		excluded[operatorID] = struct{}{}
	}
	return &WorkspaceResourceSyncService{
		db:                  db,
		hilbertClient:       hilbertClient,
		excludedOperatorIDs: excluded,
	}
}

// SyncWorkspaces syncs resources for every Hilbert workspace and isolates failures per workspace.
func (s *WorkspaceResourceSyncService) SyncWorkspaces(ctx context.Context, workspaces []auth.HilbertWorkspace, syncedAt time.Time) *WorkspaceResourceSyncSummary {
	summary := &WorkspaceResourceSyncSummary{
		Enabled:          true,
		WorkspaceResults: []WorkspaceResourceSyncResult{},
	}
	if s == nil || s.db == nil || s.hilbertClient == nil {
		return summary
	}

	for _, workspace := range workspaces {
		if workspace.ID <= defaultWorkspaceID {
			continue
		}
		result := s.syncWorkspace(ctx, workspace, syncedAt)
		summary.CollectorUpsertedCount += result.CollectorUpsertedCount
		summary.CollectorSkippedCount += result.CollectorSkippedCount
		summary.RobotUpsertedCount += result.RobotUpsertedCount
		summary.WorkspaceResults = append(summary.WorkspaceResults, result)
	}

	logger.Printf(
		"[WORKSPACE] Hilbert resource sync completed: workspaces=%d collectors_upserted=%d collectors_skipped=%d robots_upserted=%d errors=%d",
		len(summary.WorkspaceResults),
		summary.CollectorUpsertedCount,
		summary.CollectorSkippedCount,
		summary.RobotUpsertedCount,
		summary.errorCount(),
	)

	return summary
}

func (s *WorkspaceResourceSyncService) syncWorkspace(ctx context.Context, workspace auth.HilbertWorkspace, syncedAt time.Time) WorkspaceResourceSyncResult {
	result := WorkspaceResourceSyncResult{WorkspaceID: workspace.ID, Errors: []WorkspaceResourceSyncError{}}

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		result.addError("workspace", strconv.FormatInt(workspace.ID, 10), "transaction_begin_failed", err.Error())
		return result
	}
	defer func() { _ = tx.Rollback() }()

	s.syncCollectors(ctx, tx, workspace, syncedAt, &result)

	devicesPage, err := s.hilbertClient.QueryDCDevices(ctx, workspace.ID)
	if err != nil {
		result.addError("robot", strconv.FormatInt(workspace.ID, 10), "dc_device_query_failed", err.Error())
		result.discardUncommittedCounts()
		return result
	}
	for _, device := range devicesPage.Records {
		deviceType, typeErr := s.resolveDeviceType(ctx, device.DCDeviceTypeID)
		if typeErr != nil {
			result.addError("robot", strconv.FormatInt(device.ID, 10), "dc_device_type_query_failed", typeErr.Error())
			continue
		}
		upserted, robotErr := upsertHilbertRobot(ctx, tx, device, deviceType, syncedAt)
		if robotErr != nil {
			result.addError("robot", strconv.FormatInt(device.ID, 10), "robot_upsert_failed", robotErr.Error())
			continue
		}
		if upserted {
			result.RobotUpsertedCount++
		}
	}

	if err := tx.Commit(); err != nil {
		result.addError("workspace", strconv.FormatInt(workspace.ID, 10), "transaction_commit_failed", err.Error())
		result.discardUncommittedCounts()
	}
	return result
}

func (s *WorkspaceResourceSyncService) resolveDeviceType(ctx context.Context, deviceTypeID int64) (*auth.HilbertDCDeviceType, error) {
	if deviceTypeID <= 0 {
		return nil, nil
	}
	return s.hilbertClient.QueryDCDeviceTypeByID(ctx, deviceTypeID)
}

func (s *WorkspaceResourceSyncService) syncCollectors(
	ctx context.Context,
	tx *sqlx.Tx,
	workspace auth.HilbertWorkspace,
	syncedAt time.Time,
	result *WorkspaceResourceSyncResult,
) {
	for _, code := range normalizeWorkspacePeople(append(workspace.Admins, workspace.Members...)) {
		if _, excluded := s.excludedOperatorIDs[code]; excluded {
			result.CollectorSkippedCount++
			continue
		}
		account, err := s.hilbertClient.QueryAccountByCode(ctx, code)
		if err != nil {
			result.CollectorSkippedCount++
			result.addError("collector", code, "account_query_failed", err.Error())
			continue
		}
		if account == nil {
			result.CollectorSkippedCount++
			result.addError("collector", code, "account_missing", "workspace member account was not returned")
			continue
		}
		upserted, upsertErr := upsertHilbertDataCollector(ctx, tx, *account, syncedAt)
		if upsertErr != nil {
			result.CollectorSkippedCount++
			result.addError("collector", code, "collector_upsert_failed", upsertErr.Error())
			continue
		}
		if upserted {
			result.CollectorUpsertedCount++
		}
	}
}

func upsertHilbertDataCollector(ctx context.Context, tx *sqlx.Tx, account auth.HilbertAccount, syncedAt time.Time) (bool, error) {
	operatorID := strings.TrimSpace(account.Code)
	if operatorID == "" {
		return false, fmt.Errorf("hilbert account code is empty")
	}

	existingSource, err := activeMetadataSource(ctx, tx, "data_collectors", "operator_id", operatorID)
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	if err == nil && existingSource != hilbertMetadataSource {
		return false, fmt.Errorf("active data collector with operator_id %s is not a Hilbert projection", operatorID)
	}
	name := strings.TrimSpace(account.DisplayName)
	if name == "" {
		name = operatorID
	}
	metadata, err := marshalMetadata(map[string]any{
		"source":                     hilbertMetadataSource,
		"hilbert_account_id":         account.ID,
		"hilbert_account_code":       operatorID,
		"hilbert_role":               account.Role,
		"hilbert_external_user_type": account.ExternalUserType,
		"hilbert_status":             account.Status,
		"last_seen_at":               syncedAt.Format(time.RFC3339),
	})
	if err != nil {
		return false, err
	}

	if tx.DriverName() == "sqlite" {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO data_collectors (name, operator_id, status, metadata, created_at, updated_at, deleted_at)
			VALUES (?, ?, ?, ?, ?, ?, NULL)
			ON CONFLICT(operator_id) DO UPDATE SET
				name = excluded.name,
				metadata = excluded.metadata,
				updated_at = excluded.updated_at,
				deleted_at = NULL
		`, name, operatorID, hilbertResourceActiveStatus, metadata, syncedAt, syncedAt)
		return err == nil, err
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO data_collectors (name, operator_id, status, metadata, created_at, updated_at, deleted_at)
		VALUES (?, ?, ?, ?, ?, ?, NULL)
		ON DUPLICATE KEY UPDATE
			name = VALUES(name),
			metadata = VALUES(metadata),
			updated_at = VALUES(updated_at),
			deleted_at = NULL
	`, name, operatorID, hilbertResourceActiveStatus, metadata, syncedAt, syncedAt)
	return err == nil, err
}

func upsertHilbertRobot(ctx context.Context, tx *sqlx.Tx, device auth.HilbertDCDevice, deviceType *auth.HilbertDCDeviceType, syncedAt time.Time) (bool, error) {
	deviceID := strconv.FormatInt(device.ID, 10)
	deviceTypeName := ""
	if deviceType != nil {
		deviceTypeName = strings.TrimSpace(deviceType.Name)
	}
	existingSource, err := activeMetadataSource(ctx, tx, "robots", "device_id", deviceID)
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	if err == nil && existingSource != hilbertMetadataSource {
		return false, fmt.Errorf("active robot with device_id %s is not a Hilbert projection", deviceID)
	}
	if err == nil {
		if err := ensureRobotWorkspaceBindingCompatible(ctx, tx, deviceID, device.WorkspaceID); err != nil {
			return false, err
		}
	}

	metadata, err := marshalMetadata(map[string]any{
		"source":                    hilbertMetadataSource,
		"hilbert_dc_device_id":      device.ID,
		"hilbert_workspace_id":      device.WorkspaceID,
		"hilbert_dc_device_name":    strings.TrimSpace(device.Name),
		"hilbert_dc_device_sn":      strings.TrimSpace(device.SN),
		"hilbert_dc_device_type_id": device.DCDeviceTypeID,
		"hilbert_dc_device_type":    deviceTypeName,
		"last_seen_at":              syncedAt.Format(time.RFC3339),
		"hilbert_raw":               device,
	})
	if err != nil {
		return false, err
	}

	if tx.DriverName() == "sqlite" {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO robots (device_id, workspace_id, device_type_id, device_type, status, metadata, created_at, updated_at, deleted_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL)
			ON CONFLICT(device_id) DO UPDATE SET
				workspace_id = excluded.workspace_id,
				device_type_id = excluded.device_type_id,
				device_type = excluded.device_type,
				metadata = excluded.metadata,
				updated_at = excluded.updated_at,
				deleted_at = NULL
		`, deviceID, device.WorkspaceID, nullablePositiveInt64(device.DCDeviceTypeID), nullableString(deviceTypeName), hilbertResourceActiveStatus, metadata, syncedAt, syncedAt)
		return err == nil, err
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO robots (device_id, workspace_id, device_type_id, device_type, status, metadata, created_at, updated_at, deleted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL)
		ON DUPLICATE KEY UPDATE
			workspace_id = VALUES(workspace_id),
			device_type_id = VALUES(device_type_id),
			device_type = VALUES(device_type),
			metadata = VALUES(metadata),
			updated_at = VALUES(updated_at),
			deleted_at = NULL
	`, deviceID, device.WorkspaceID, nullablePositiveInt64(device.DCDeviceTypeID), nullableString(deviceTypeName), hilbertResourceActiveStatus, metadata, syncedAt, syncedAt)
	return err == nil, err
}

func nullablePositiveInt64(value int64) sql.NullInt64 {
	if value <= 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: value, Valid: true}
}

func nullableString(value string) sql.NullString {
	value = strings.TrimSpace(value)
	if value == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: value, Valid: true}
}

func ensureRobotWorkspaceBindingCompatible(ctx context.Context, tx *sqlx.Tx, deviceID string, workspaceID int64) error {
	var mismatch bool
	if err := tx.GetContext(ctx, &mismatch, `
		SELECT EXISTS(
			SELECT 1
			FROM robots r
			INNER JOIN workstations ws
				ON ws.robot_id = r.id
				AND ws.is_current = TRUE
				AND ws.deleted_at IS NULL
			WHERE r.device_id = ?
				AND r.deleted_at IS NULL
				AND ws.workspace_id <> ?
		)
	`, deviceID, workspaceID); err != nil {
		return fmt.Errorf("check robot workspace binding: %w", err)
	}
	if mismatch {
		return fmt.Errorf("current workstation binding belongs to another workspace")
	}
	return nil
}

func activeMetadataSource(ctx context.Context, tx *sqlx.Tx, table string, column string, value string) (string, error) {
	var metadata sql.NullString
	query := fmt.Sprintf("SELECT metadata FROM %s WHERE %s = ? AND deleted_at IS NULL LIMIT 1", table, column) //nolint:gosec // table and column are hardcoded by callers in this file.
	if err := tx.GetContext(ctx, &metadata, query, value); err != nil {
		return "", err
	}
	return metadataSource(metadata.String), nil
}

func metadataSource(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return ""
	}
	source, _ := payload["source"].(string)
	return strings.TrimSpace(source)
}

func marshalMetadata(payload map[string]any) (string, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (r *WorkspaceResourceSyncResult) addError(resource string, externalID string, code string, message string) {
	r.Errors = append(r.Errors, WorkspaceResourceSyncError{
		Resource:   resource,
		ExternalID: externalID,
		Code:       code,
		Message:    message,
	})
}

func (r *WorkspaceResourceSyncResult) discardUncommittedCounts() {
	r.CollectorUpsertedCount = 0
	r.RobotUpsertedCount = 0
}

func (s *WorkspaceResourceSyncSummary) errorCount() int {
	if s == nil {
		return 0
	}
	total := 0
	for _, result := range s.WorkspaceResults {
		total += len(result.Errors)
	}
	return total
}
