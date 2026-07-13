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
	QueryAccountByCode(ctx context.Context, sessionKey string, code string) (*auth.HilbertAccount, error)
	QueryDCDevices(ctx context.Context, sessionKey string, workspaceID int64) (*auth.HilbertDCDevicePage, error)
	QueryDCDeviceTypeByID(ctx context.Context, sessionKey string, id int64) (*auth.HilbertDCDeviceType, error)
}

// WorkspaceResourceSyncSummary summarizes a workspace resource sync run.
type WorkspaceResourceSyncSummary struct {
	Enabled                bool                          `json:"enabled"`
	CollectorUpsertedCount int                           `json:"collector_upserted_count"`
	CollectorSkippedCount  int                           `json:"collector_skipped_count"`
	RobotUpsertedCount     int                           `json:"robot_upserted_count"`
	RobotTypeUpsertedCount int                           `json:"robot_type_upserted_count"`
	FactoryUpsertedCount   int                           `json:"factory_upserted_count"`
	WorkspaceResults       []WorkspaceResourceSyncResult `json:"workspace_results"`
}

// WorkspaceResourceSyncResult summarizes resource sync for one workspace.
type WorkspaceResourceSyncResult struct {
	WorkspaceID            int64                        `json:"workspace_id"`
	CollectorUpsertedCount int                          `json:"collector_upserted_count"`
	CollectorSkippedCount  int                          `json:"collector_skipped_count"`
	RobotUpsertedCount     int                          `json:"robot_upserted_count"`
	RobotTypeUpsertedCount int                          `json:"robot_type_upserted_count"`
	FactoryUpserted        bool                         `json:"factory_upserted"`
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
	db            *sqlx.DB
	hilbertClient HilbertWorkspaceResourceClient
}

// NewWorkspaceResourceSyncService creates a resource sync service.
func NewWorkspaceResourceSyncService(db *sqlx.DB, hilbertClient HilbertWorkspaceResourceClient) *WorkspaceResourceSyncService {
	return &WorkspaceResourceSyncService{db: db, hilbertClient: hilbertClient}
}

// SyncWorkspaces syncs resources for every Hilbert workspace and isolates failures per workspace.
func (s *WorkspaceResourceSyncService) SyncWorkspaces(ctx context.Context, sessionKey string, workspaces []auth.HilbertWorkspace, syncedAt time.Time) *WorkspaceResourceSyncSummary {
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
		result := s.syncWorkspace(ctx, sessionKey, workspace, syncedAt)
		summary.CollectorUpsertedCount += result.CollectorUpsertedCount
		summary.CollectorSkippedCount += result.CollectorSkippedCount
		summary.RobotUpsertedCount += result.RobotUpsertedCount
		summary.RobotTypeUpsertedCount += result.RobotTypeUpsertedCount
		if result.FactoryUpserted {
			summary.FactoryUpsertedCount++
		}
		summary.WorkspaceResults = append(summary.WorkspaceResults, result)
	}

	logger.Printf(
		"[WORKSPACE] Hilbert resource sync completed: workspaces=%d collectors_upserted=%d collectors_skipped=%d robots_upserted=%d robot_types_upserted=%d factories_upserted=%d errors=%d",
		len(summary.WorkspaceResults),
		summary.CollectorUpsertedCount,
		summary.CollectorSkippedCount,
		summary.RobotUpsertedCount,
		summary.RobotTypeUpsertedCount,
		summary.FactoryUpsertedCount,
		summary.errorCount(),
	)

	return summary
}

func (s *WorkspaceResourceSyncService) syncWorkspace(ctx context.Context, sessionKey string, workspace auth.HilbertWorkspace, syncedAt time.Time) WorkspaceResourceSyncResult {
	result := WorkspaceResourceSyncResult{WorkspaceID: workspace.ID, Errors: []WorkspaceResourceSyncError{}}

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		result.addError("workspace", strconv.FormatInt(workspace.ID, 10), "transaction_begin_failed", err.Error())
		return result
	}
	defer func() { _ = tx.Rollback() }()

	factoryID, err := upsertWorkspaceCompatFactory(ctx, tx, workspace, syncedAt)
	if err != nil {
		result.addError("factory", strconv.FormatInt(workspace.ID, 10), "factory_upsert_failed", err.Error())
		return result
	}
	result.FactoryUpserted = true

	s.syncCollectors(ctx, tx, sessionKey, workspace, syncedAt, &result)

	devicesPage, err := s.hilbertClient.QueryDCDevices(ctx, sessionKey, workspace.ID)
	if err != nil {
		result.addError("robot", strconv.FormatInt(workspace.ID, 10), "dc_device_query_failed", err.Error())
		result.discardUncommittedCounts()
		return result
	}
	deviceTypes := map[int64]auth.HilbertDCDeviceType{}
	for _, device := range devicesPage.Records {
		if _, ok := deviceTypes[device.DCDeviceTypeID]; ok {
			continue
		}
		deviceType, typeErr := s.hilbertClient.QueryDCDeviceTypeByID(ctx, sessionKey, device.DCDeviceTypeID)
		if typeErr != nil {
			result.addError("robot_type", strconv.FormatInt(device.DCDeviceTypeID, 10), "dc_device_type_query_failed", typeErr.Error())
			continue
		}
		if deviceType == nil {
			result.addError("robot_type", strconv.FormatInt(device.DCDeviceTypeID, 10), "dc_device_type_missing", "Hilbert dc device type was not returned")
			continue
		}
		deviceTypes[device.DCDeviceTypeID] = *deviceType
		if upserted, upsertErr := upsertHilbertRobotType(ctx, tx, *deviceType, syncedAt); upsertErr != nil {
			result.addError("robot_type", strconv.FormatInt(device.DCDeviceTypeID, 10), "robot_type_upsert_failed", upsertErr.Error())
			delete(deviceTypes, device.DCDeviceTypeID)
		} else if upserted {
			result.RobotTypeUpsertedCount++
		}
	}

	for _, device := range devicesPage.Records {
		if _, ok := deviceTypes[device.DCDeviceTypeID]; !ok {
			result.addError("robot", strconv.FormatInt(device.ID, 10), "robot_type_unavailable", "Hilbert dc device type could not be projected")
			continue
		}
		upserted, robotErr := upsertHilbertRobot(ctx, tx, device, factoryID, syncedAt)
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

func (s *WorkspaceResourceSyncService) syncCollectors(
	ctx context.Context,
	tx *sqlx.Tx,
	sessionKey string,
	workspace auth.HilbertWorkspace,
	syncedAt time.Time,
	result *WorkspaceResourceSyncResult,
) {
	for _, code := range normalizeWorkspacePeople(append(workspace.Admins, workspace.Members...)) {
		account, err := s.hilbertClient.QueryAccountByCode(ctx, sessionKey, code)
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
		upserted, upsertErr := upsertHilbertDataCollector(ctx, tx, workspace.ID, *account, syncedAt)
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

func upsertWorkspaceCompatFactory(ctx context.Context, tx *sqlx.Tx, workspace auth.HilbertWorkspace, syncedAt time.Time) (int64, error) {
	slug := fmt.Sprintf("hilbert_workspace_%d", workspace.ID)
	name := fmt.Sprintf("%s / Hilbert Workspace %d", strings.TrimSpace(workspace.Name), workspace.ID)
	if strings.TrimSpace(workspace.Name) == "" {
		name = fmt.Sprintf("Hilbert Workspace %d", workspace.ID)
	}

	if tx.DriverName() == "sqlite" {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO factories (name, slug, updated_at, created_at, deleted_at)
			VALUES (?, ?, ?, ?, NULL)
			ON CONFLICT(slug) DO UPDATE SET
				name = excluded.name,
				updated_at = excluded.updated_at,
				deleted_at = NULL
		`, name, slug, syncedAt, syncedAt); err != nil {
			return 0, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO factories (name, slug, updated_at, created_at, deleted_at)
			VALUES (?, ?, ?, ?, NULL)
			ON DUPLICATE KEY UPDATE
				name = VALUES(name),
				updated_at = VALUES(updated_at),
				deleted_at = NULL
		`, name, slug, syncedAt, syncedAt); err != nil {
			return 0, err
		}
	}

	var id int64
	if err := tx.GetContext(ctx, &id, "SELECT id FROM factories WHERE slug = ? AND deleted_at IS NULL LIMIT 1", slug); err != nil {
		return 0, err
	}
	return id, nil
}

func upsertHilbertDataCollector(ctx context.Context, tx *sqlx.Tx, workspaceID int64, account auth.HilbertAccount, syncedAt time.Time) (bool, error) {
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
	if err == nil {
		if err := ensureCollectorWorkspaceBindingCompatible(ctx, tx, operatorID, workspaceID); err != nil {
			return false, err
		}
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
		"hilbert_workspace_id":       workspaceID,
		"last_seen_at":               syncedAt.Format(time.RFC3339),
	})
	if err != nil {
		return false, err
	}

	if tx.DriverName() == "sqlite" {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO data_collectors (organization_id, name, operator_id, status, metadata, created_at, updated_at, deleted_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, NULL)
			ON CONFLICT(operator_id) DO UPDATE SET
				organization_id = excluded.organization_id,
				name = excluded.name,
				metadata = excluded.metadata,
				updated_at = excluded.updated_at,
				deleted_at = NULL
		`, workspaceID, name, operatorID, hilbertResourceActiveStatus, metadata, syncedAt, syncedAt)
		return err == nil, err
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO data_collectors (organization_id, name, operator_id, status, metadata, created_at, updated_at, deleted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, NULL)
		ON DUPLICATE KEY UPDATE
			organization_id = VALUES(organization_id),
			name = VALUES(name),
			metadata = VALUES(metadata),
			updated_at = VALUES(updated_at),
			deleted_at = NULL
	`, workspaceID, name, operatorID, hilbertResourceActiveStatus, metadata, syncedAt, syncedAt)
	return err == nil, err
}

func upsertHilbertRobotType(ctx context.Context, tx *sqlx.Tx, deviceType auth.HilbertDCDeviceType, syncedAt time.Time) (bool, error) {
	model := fmt.Sprintf("hilbert_dc_device_type_%d", deviceType.ID)
	var row struct {
		Model        string         `db:"model"`
		Capabilities sql.NullString `db:"capabilities"`
	}
	err := tx.GetContext(ctx, &row, "SELECT model, capabilities FROM robot_types WHERE id = ? AND deleted_at IS NULL LIMIT 1", deviceType.ID)
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	if err == nil && row.Model != model && metadataSource(row.Capabilities.String) != hilbertMetadataSource {
		return false, fmt.Errorf("active robot type id %d is not a Hilbert projection", deviceType.ID)
	}

	name := strings.TrimSpace(deviceType.Name)
	if name == "" {
		name = fmt.Sprintf("Hilbert DC Device Type %d", deviceType.ID)
	}
	capabilities, err := marshalMetadata(map[string]any{
		"source":                    hilbertMetadataSource,
		"hilbert_dc_device_type_id": deviceType.ID,
		"description":               optionalString(deviceType.Description),
		"last_seen_at":              syncedAt.Format(time.RFC3339),
	})
	if err != nil {
		return false, err
	}

	if tx.DriverName() == "sqlite" {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO robot_types (id, name, model, manufacturer, ros_topics, capabilities, created_at, updated_at, deleted_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL)
			ON CONFLICT(id) DO UPDATE SET
				name = excluded.name,
				manufacturer = excluded.manufacturer,
				ros_topics = excluded.ros_topics,
				capabilities = excluded.capabilities,
				updated_at = excluded.updated_at,
				deleted_at = NULL
		`, deviceType.ID, name, model, "Hilbert", "{}", capabilities, syncedAt, syncedAt)
		return err == nil, err
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO robot_types (id, name, model, manufacturer, ros_topics, capabilities, created_at, updated_at, deleted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL)
		ON DUPLICATE KEY UPDATE
			name = VALUES(name),
			manufacturer = VALUES(manufacturer),
			ros_topics = VALUES(ros_topics),
			capabilities = VALUES(capabilities),
			updated_at = VALUES(updated_at),
			deleted_at = NULL
	`, deviceType.ID, name, model, "Hilbert", "{}", capabilities, syncedAt, syncedAt)
	return err == nil, err
}

func upsertHilbertRobot(ctx context.Context, tx *sqlx.Tx, device auth.HilbertDCDevice, factoryID int64, syncedAt time.Time) (bool, error) {
	deviceID := strconv.FormatInt(device.ID, 10)
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
		"last_seen_at":              syncedAt.Format(time.RFC3339),
		"hilbert_raw":               device,
	})
	if err != nil {
		return false, err
	}

	if tx.DriverName() == "sqlite" {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO robots (robot_type_id, device_id, workspace_id, factory_id, status, metadata, created_at, updated_at, deleted_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL)
			ON CONFLICT(device_id) DO UPDATE SET
				robot_type_id = excluded.robot_type_id,
				workspace_id = excluded.workspace_id,
				factory_id = excluded.factory_id,
				metadata = excluded.metadata,
				updated_at = excluded.updated_at,
				deleted_at = NULL
		`, device.DCDeviceTypeID, deviceID, device.WorkspaceID, factoryID, hilbertResourceActiveStatus, metadata, syncedAt, syncedAt)
		return err == nil, err
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO robots (robot_type_id, device_id, workspace_id, factory_id, status, metadata, created_at, updated_at, deleted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL)
		ON DUPLICATE KEY UPDATE
			robot_type_id = VALUES(robot_type_id),
			workspace_id = VALUES(workspace_id),
			factory_id = VALUES(factory_id),
			metadata = VALUES(metadata),
			updated_at = VALUES(updated_at),
			deleted_at = NULL
	`, device.DCDeviceTypeID, deviceID, device.WorkspaceID, factoryID, hilbertResourceActiveStatus, metadata, syncedAt, syncedAt)
	return err == nil, err
}

func ensureCollectorWorkspaceBindingCompatible(ctx context.Context, tx *sqlx.Tx, operatorID string, workspaceID int64) error {
	var mismatch bool
	if err := tx.GetContext(ctx, &mismatch, `
		SELECT EXISTS(
			SELECT 1
			FROM data_collectors dc
			INNER JOIN workstations ws
				ON ws.data_collector_id = dc.id
				AND ws.is_current = TRUE
				AND ws.deleted_at IS NULL
			INNER JOIN robots r
				ON r.id = ws.robot_id
				AND r.deleted_at IS NULL
			WHERE dc.operator_id = ?
				AND dc.deleted_at IS NULL
				AND r.workspace_id <> ?
		)
	`, operatorID, workspaceID); err != nil {
		return fmt.Errorf("check collector workspace binding: %w", err)
	}
	if mismatch {
		return fmt.Errorf("current workstation binding belongs to another workspace")
	}
	return nil
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
			INNER JOIN data_collectors dc
				ON dc.id = ws.data_collector_id
				AND dc.deleted_at IS NULL
			WHERE r.device_id = ?
				AND r.deleted_at IS NULL
				AND dc.organization_id <> ?
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

func optionalString(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
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
	r.RobotTypeUpsertedCount = 0
	r.FactoryUpserted = false
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
