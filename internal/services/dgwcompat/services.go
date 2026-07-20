// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package dgwcompat

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"archebase.com/keystone-edge/internal/cloud/cloudpb"
	"archebase.com/keystone-edge/internal/logger"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type uploadSession struct {
	LogicalUploadID        string
	UploadID               string
	RobotID                int64
	TaskPK                 int64
	TaskID                 string
	WorkstationID          sql.NullInt64
	OrganizationID         sql.NullInt64
	DCPlanID               int64
	LocalDCPlanID          sql.NullInt64
	DeviceID               string
	WorkspaceID            int64
	AuthEpoch              int64
	Bucket                 string
	Endpoint               string
	ObjectKey              string
	ClientHints            map[string]string
	CreatedAt              time.Time
	CompletedAt            time.Time
	Aborted                bool
	CompletedPartCount     int32
	ObjectETag             string
	FileSize               int64
	CredentialRefreshCount int32
	LastSTSExpireAt        time.Time
}

type sessionStore struct {
	mu        sync.RWMutex
	byUpload  map[string]*uploadSession
	byLogical map[string]*uploadSession
}

func newSessionStore() *sessionStore {
	return &sessionStore{
		byUpload:  make(map[string]*uploadSession),
		byLogical: make(map[string]*uploadSession),
	}
}

func (s *sessionStore) put(session *uploadSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byUpload[session.UploadID] = session
	s.byLogical[session.LogicalUploadID] = session
}

func (s *sessionStore) getByUpload(uploadID string) (*uploadSession, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	session, ok := s.byUpload[uploadID]
	return session, ok
}

func (s *sessionStore) getByLogical(logicalUploadID string) (*uploadSession, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	session, ok := s.byLogical[logicalUploadID]
	return session, ok
}

func (s *sessionStore) update(uploadID string, update func(*uploadSession)) (*uploadSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.byUpload[uploadID]
	if !ok {
		return nil, false
	}
	update(session)
	return session, true
}

type gatewayService struct {
	cloudpb.UnimplementedDataGatewayServiceServer
	cfg      Config
	db       *sqlx.DB
	sts      stsProvider
	sessions *sessionStore
	qa       episodeQAEnqueuer
	now      func() time.Time
}

func newGatewayService(cfg Config, sts stsProvider, sessions *sessionStore, db *sqlx.DB, qa episodeQAEnqueuer) *gatewayService {
	database := db
	return &gatewayService{
		cfg:      cfg,
		db:       database,
		sts:      sts,
		sessions: sessions,
		qa:       qa,
		now:      func() time.Time { return time.Now().UTC() },
	}
}

func (s *gatewayService) CreateLogicalUpload(ctx context.Context, req *cloudpb.CreateLogicalUploadRequest) (*cloudpb.CreateLogicalUploadResponse, error) {
	principal, err := principalFromContext(ctx)
	if err != nil {
		return nil, err
	}
	logicalUploadID := uuid.NewString()
	uploadID := uuid.NewString()
	hints := cloneMap(req.GetClientHints())
	if hints["product"] == "ego_portal_lite" {
		checksumMD5 := strings.ToLower(strings.TrimSpace(hints["checksum_md5"]))
		if !isMD5Hex(checksumMD5) {
			return nil, status.Error(codes.InvalidArgument, "checksum_md5 must be a 32-character hexadecimal MD5 digest")
		}
		hints["checksum_md5"] = checksumMD5
	}
	if hinted := strings.TrimSpace(hints["device_id"]); hinted != "" && hinted != principal.DeviceID {
		return nil, status.Error(codes.PermissionDenied, "client device hint does not match authenticated device")
	}
	if hinted := strings.TrimSpace(hints["workspace_id"]); hinted != "" && hinted != fmt.Sprintf("%d", principal.WorkspaceID) {
		return nil, status.Error(codes.PermissionDenied, "client workspace hint does not match authenticated device")
	}
	hints["device_id"] = principal.DeviceID
	hints["workspace_id"] = fmt.Sprintf("%d", principal.WorkspaceID)
	taskBinding, err := s.validateCreateLogicalUpload(ctx, principal, hints)
	if err != nil {
		return nil, err
	}
	objectKey := buildObjectKey(s.cfg.TOSKeyPrefix, hints, uploadID)
	logger.Printf("[DGW_COMPAT] CreateLogicalUpload assume_role upload_id=%s logical_upload_id=%s bucket=%s object_key=%s endpoint=%s region=%s part_size=%d capture_id=%s task_id=%s device_id=%s workspace_id=%s",
		uploadID,
		logicalUploadID,
		s.cfg.TOSBucket,
		objectKey,
		s.cfg.TOSEndpoint,
		s.cfg.TOSRegion,
		s.cfg.UploadPartSize,
		hints["capture_id"],
		hints["task_id"],
		hints["device_id"],
		hints["workspace_id"],
	)
	creds, err := s.sts.AssumeRole(ctx, stsScope{Bucket: s.cfg.TOSBucket, ObjectKey: objectKey})
	if err != nil {
		logger.Printf("[DGW_COMPAT] CreateLogicalUpload assume_role_failed upload_id=%s logical_upload_id=%s bucket=%s object_key=%s error=%v",
			uploadID, logicalUploadID, s.cfg.TOSBucket, objectKey, err)
		return nil, status.Errorf(codes.Unavailable, "assume role: %v", err)
	}
	session := &uploadSession{
		LogicalUploadID: logicalUploadID,
		UploadID:        uploadID,
		RobotID:         principal.RobotID,
		TaskPK:          taskBinding.TaskPK,
		TaskID:          taskBinding.TaskID,
		WorkstationID:   taskBinding.WorkstationID,
		OrganizationID:  taskBinding.OrganizationID,
		DCPlanID:        taskBinding.DCPlanID.Int64,
		LocalDCPlanID:   taskBinding.LocalDCPlanID,
		DeviceID:        principal.DeviceID,
		WorkspaceID:     principal.WorkspaceID,
		AuthEpoch:       principal.AuthEpoch,
		Bucket:          s.cfg.TOSBucket,
		Endpoint:        s.cfg.TOSEndpoint,
		ObjectKey:       objectKey,
		ClientHints:     hints,
		CreatedAt:       s.now(),
		LastSTSExpireAt: creds.Expiration,
	}
	s.sessions.put(session)
	logger.Printf("[DGW_COMPAT] CreateLogicalUpload issued upload_id=%s logical_upload_id=%s bucket=%s object_key=%s sts_expires_at=%s sts_ttl_seconds=%d capture_id=%s task_id=%s device_id=%s",
		uploadID,
		logicalUploadID,
		s.cfg.TOSBucket,
		objectKey,
		creds.Expiration.Format(time.RFC3339),
		int(time.Until(creds.Expiration).Seconds()),
		hints["capture_id"],
		hints["task_id"],
		hints["device_id"],
	)
	return &cloudpb.CreateLogicalUploadResponse{
		LogicalUploadId: logicalUploadID,
		UploadId:        uploadID,
		Credentials:     s.uploadCredentials(session, creds),
	}, nil
}

func (s *gatewayService) GetUploadRecovery(ctx context.Context, req *cloudpb.GetUploadRecoveryRequest) (*cloudpb.GetUploadRecoveryResponse, error) {
	session, ok := s.sessions.getByLogical(req.GetLogicalUploadId())
	if !ok {
		logger.Printf("[DGW_COMPAT] GetUploadRecovery missing logical_upload_id=%s", req.GetLogicalUploadId())
		return nil, status.Error(codes.NotFound, "logical upload not found")
	}
	if err := authorizeUploadSession(ctx, session); err != nil {
		logger.Printf("[DGW_COMPAT] GetUploadRecovery denied upload_id=%s logical_upload_id=%s device_id=%s workspace_id=%d error=%v",
			session.UploadID, session.LogicalUploadID, session.DeviceID, session.WorkspaceID, err)
		return nil, err
	}
	statusValue := cloudpb.LogicalUploadStatus_LOGICAL_UPLOAD_STATUS_ACTIVE
	nextAction := cloudpb.UploadRecoveryAction_UPLOAD_RECOVERY_ACTION_CONTINUE
	terminalReason := ""
	if session.Aborted {
		statusValue = cloudpb.LogicalUploadStatus_LOGICAL_UPLOAD_STATUS_TERMINAL
		nextAction = cloudpb.UploadRecoveryAction_UPLOAD_RECOVERY_ACTION_ABORT
		terminalReason = "aborted"
	}
	if !session.CompletedAt.IsZero() {
		statusValue = cloudpb.LogicalUploadStatus_LOGICAL_UPLOAD_STATUS_COMPLETED
		nextAction = cloudpb.UploadRecoveryAction_UPLOAD_RECOVERY_ACTION_COMPLETE_ONLY
	}
	logger.Printf("[DGW_COMPAT] GetUploadRecovery upload_id=%s logical_upload_id=%s status=%s next_action=%s aborted=%t completed=%t refresh_count=%d completed_parts=%d object_etag=%s bucket=%s object_key=%s",
		session.UploadID,
		session.LogicalUploadID,
		statusValue.String(),
		nextAction.String(),
		session.Aborted,
		!session.CompletedAt.IsZero(),
		session.CredentialRefreshCount,
		session.CompletedPartCount,
		session.ObjectETag,
		session.Bucket,
		session.ObjectKey,
	)
	return &cloudpb.GetUploadRecoveryResponse{
		LogicalUploadId:        session.LogicalUploadID,
		LogicalUploadStatus:    statusValue,
		CurrentUploadId:        session.UploadID,
		Bucket:                 session.Bucket,
		Endpoint:               session.Endpoint,
		ObjectKey:              session.ObjectKey,
		CanRefreshCredentials:  true,
		RestartAllowed:         true,
		TerminalReason:         terminalReason,
		CredentialRefreshCount: session.CredentialRefreshCount,
		SessionExpireAtUnix:    session.LastSTSExpireAt.Unix(),
		NextAction:             nextAction,
		CompletedPartCount:     session.CompletedPartCount,
		ObjectEtag:             session.ObjectETag,
	}, nil
}

func (s *gatewayService) ReissueUploadCredentials(ctx context.Context, req *cloudpb.ReissueUploadCredentialsRequest) (*cloudpb.ReissueUploadCredentialsResponse, error) {
	session, ok := s.sessions.getByUpload(req.GetUploadId())
	if !ok {
		return nil, status.Error(codes.NotFound, "upload not found")
	}
	if err := authorizeUploadSession(ctx, session); err != nil {
		logger.Printf("[DGW_COMPAT] ReissueUploadCredentials denied upload_id=%s logical_upload_id=%s device_id=%s workspace_id=%d error=%v",
			session.UploadID, session.LogicalUploadID, session.DeviceID, session.WorkspaceID, err)
		return nil, err
	}
	logger.Printf("[DGW_COMPAT] ReissueUploadCredentials assume_role upload_id=%s logical_upload_id=%s bucket=%s object_key=%s refresh_count=%d",
		session.UploadID, session.LogicalUploadID, session.Bucket, session.ObjectKey, session.CredentialRefreshCount)
	creds, err := s.sts.AssumeRole(ctx, stsScope{Bucket: session.Bucket, ObjectKey: session.ObjectKey})
	if err != nil {
		logger.Printf("[DGW_COMPAT] ReissueUploadCredentials assume_role_failed upload_id=%s logical_upload_id=%s bucket=%s object_key=%s error=%v",
			session.UploadID, session.LogicalUploadID, session.Bucket, session.ObjectKey, err)
		return nil, status.Errorf(codes.Unavailable, "assume role: %v", err)
	}
	updated, ok := s.sessions.update(session.UploadID, func(current *uploadSession) {
		current.CredentialRefreshCount++
		current.LastSTSExpireAt = creds.Expiration
	})
	if !ok {
		return nil, status.Error(codes.NotFound, "upload not found")
	}
	logger.Printf("[DGW_COMPAT] ReissueUploadCredentials issued upload_id=%s logical_upload_id=%s refresh_count=%d sts_expires_at=%s sts_ttl_seconds=%d",
		updated.UploadID,
		updated.LogicalUploadID,
		updated.CredentialRefreshCount,
		creds.Expiration.Format(time.RFC3339),
		int(time.Until(creds.Expiration).Seconds()),
	)
	return &cloudpb.ReissueUploadCredentialsResponse{
		LogicalUploadId: updated.LogicalUploadID,
		UploadId:        updated.UploadID,
		Credentials:     s.uploadCredentials(updated, creds),
	}, nil
}

func (s *gatewayService) AbortUpload(ctx context.Context, req *cloudpb.AbortUploadRequest) (*cloudpb.AbortUploadResponse, error) {
	session, ok := s.sessions.getByLogical(req.GetLogicalUploadId())
	if !ok {
		return nil, status.Error(codes.NotFound, "logical upload not found")
	}
	if err := authorizeUploadSession(ctx, session); err != nil {
		logger.Printf("[DGW_COMPAT] AbortUpload denied upload_id=%s logical_upload_id=%s device_id=%s workspace_id=%d reason=%q error=%v",
			session.UploadID, session.LogicalUploadID, session.DeviceID, session.WorkspaceID, req.GetReason(), err)
		return nil, err
	}
	updated, ok := s.sessions.update(session.UploadID, func(current *uploadSession) {
		current.Aborted = true
	})
	if !ok {
		return nil, status.Error(codes.NotFound, "upload not found")
	}
	logger.Printf("[DGW_COMPAT] AbortUpload upload_id=%s logical_upload_id=%s reason=%q",
		updated.UploadID, updated.LogicalUploadID, req.GetReason())
	return &cloudpb.AbortUploadResponse{
		LogicalUploadId: updated.LogicalUploadID,
		UploadId:        updated.UploadID,
	}, nil
}

func (s *gatewayService) CompleteUpload(ctx context.Context, req *cloudpb.CompleteUploadRequest) (*cloudpb.CompleteUploadResponse, error) {
	session, ok := s.sessions.getByUpload(req.GetUploadId())
	if !ok {
		return nil, status.Error(codes.NotFound, "upload not found")
	}
	if err := authorizeUploadSession(ctx, session); err != nil {
		logger.Printf("[DGW_COMPAT] CompleteUpload denied upload_id=%s logical_upload_id=%s device_id=%s workspace_id=%d error=%v",
			session.UploadID, session.LogicalUploadID, session.DeviceID, session.WorkspaceID, err)
		return nil, err
	}
	episodeID, episodePK, episodeCreated, err := s.completeBusinessUpload(ctx, session, req)
	if err != nil {
		return nil, err
	}
	updated, ok := s.sessions.update(req.GetUploadId(), func(current *uploadSession) {
		current.CompletedAt = s.now()
		current.FileSize = req.GetFileSize()
		current.CompletedPartCount = req.GetCompletedPartCount()
		current.ObjectETag = req.GetObjectEtag()
	})
	if !ok {
		return nil, status.Error(codes.NotFound, "upload not found")
	}
	rawTags := req.GetRawTags()
	logger.Printf("[DGW_COMPAT] CompleteUpload upload_id=%s logical_upload_id=%s bucket=%s object_key=%s file_size=%d completed_parts=%d etag=%s capture_id=%s task_id=%s device_id=%s tag_capture_id=%s tag_task_id=%s tag_device_id=%s",
		updated.UploadID,
		updated.LogicalUploadID,
		updated.Bucket,
		updated.ObjectKey,
		req.GetFileSize(),
		req.GetCompletedPartCount(),
		req.GetObjectEtag(),
		updated.ClientHints["capture_id"],
		updated.ClientHints["task_id"],
		updated.ClientHints["device_id"],
		rawTags["capture_id"],
		rawTags["task_id"],
		rawTags["device_id"],
	)
	if episodeID != "" {
		logger.Printf("[DGW_COMPAT] CompleteUpload persisted episode_id=%s episode_pk=%d logical_upload_id=%s task_id=%s",
			episodeID, episodePK, updated.LogicalUploadID, updated.ClientHints["task_id"])
	}
	if episodeCreated && episodePK > 0 && s.qa != nil {
		s.qa.EnqueueEpisode(episodePK)
	}
	return &cloudpb.CompleteUploadResponse{}, nil
}

type uploadTaskBinding struct {
	TaskPK          int64         `db:"id"`
	TaskID          string        `db:"task_id"`
	Status          string        `db:"status"`
	WorkstationID   sql.NullInt64 `db:"workstation_id"`
	WorkspaceID     sql.NullInt64 `db:"workspace_id"`
	OrganizationID  sql.NullInt64 `db:"organization_id"`
	DCPlanID        sql.NullInt64 `db:"dc_plan_id"`
	LocalDCPlanID   sql.NullInt64 `db:"local_dc_plan_id"`
	PlanWorkspaceID int64         `db:"plan_workspace_id"`
	EpisodePK       sql.NullInt64 `db:"episode_pk"`
}

func (s *gatewayService) validateCreateLogicalUpload(ctx context.Context, principal devicePrincipal, hints map[string]string) (uploadTaskBinding, error) {
	if s.db == nil {
		return uploadTaskBinding{}, nil
	}
	taskID := strings.TrimSpace(hints["task_id"])
	captureID := strings.TrimSpace(hints["capture_id"])
	if taskID == "" || captureID == "" {
		return uploadTaskBinding{}, status.Error(codes.InvalidArgument, "capture_id and task_id are required")
	}
	dcPlanID, err := parsePositiveInt64Hint(hints, "dc_plan_id")
	if err != nil {
		return uploadTaskBinding{}, err
	}
	workspaceID, err := parsePositiveInt64Hint(hints, "workspace_id")
	if err != nil {
		return uploadTaskBinding{}, err
	}
	if workspaceID != principal.WorkspaceID {
		return uploadTaskBinding{}, status.Error(codes.PermissionDenied, "workspace_id does not match authenticated device")
	}

	var row uploadTaskBinding
	err = s.db.GetContext(ctx, &row, `
		SELECT
			t.id,
			t.task_id,
			t.status,
			t.workstation_id,
			COALESCE(t.organization_id, ws.workspace_id) AS workspace_id,
			t.organization_id,
			t.dc_plan_id,
			t.local_dc_plan_id,
			dp.workspace_id AS plan_workspace_id,
			e.id AS episode_pk
		FROM tasks t
		INNER JOIN dc_plan dp ON dp.id = t.dc_plan_id AND dp.deleted_at IS NULL
		LEFT JOIN workstations ws ON ws.id = t.workstation_id AND ws.deleted_at IS NULL
		LEFT JOIN episodes e ON e.task_id = t.id AND e.deleted_at IS NULL
		WHERE t.task_id = ? AND t.deleted_at IS NULL
		LIMIT 1
	`, taskID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return uploadTaskBinding{}, status.Error(codes.NotFound, "task not found")
		}
		return uploadTaskBinding{}, status.Error(codes.Unavailable, "task lookup unavailable")
	}
	if row.EpisodePK.Valid {
		return uploadTaskBinding{}, status.Error(codes.FailedPrecondition, "task is already bound to an episode")
	}
	if !row.DCPlanID.Valid || row.DCPlanID.Int64 != dcPlanID {
		return uploadTaskBinding{}, status.Error(codes.FailedPrecondition, "dc_plan_id does not match task")
	}
	if row.PlanWorkspaceID != principal.WorkspaceID {
		return uploadTaskBinding{}, status.Error(codes.PermissionDenied, "task plan belongs to another workspace")
	}
	if row.WorkspaceID.Valid && row.WorkspaceID.Int64 != principal.WorkspaceID {
		return uploadTaskBinding{}, status.Error(codes.PermissionDenied, "task workspace belongs to another workspace")
	}
	switch row.Status {
	case "pending", "ready", "in_progress", "uploading":
	default:
		return uploadTaskBinding{}, status.Error(codes.FailedPrecondition, "task is not uploadable")
	}
	return row, nil
}

type completedUploadEpisode struct {
	ID        int64           `db:"id"`
	EpisodeID string          `db:"episode_id"`
	MCAPPath  string          `db:"mcap_path"`
	FileSize  sql.NullInt64   `db:"file_size_bytes"`
	Duration  sql.NullFloat64 `db:"duration_sec"`
	Metadata  sql.NullString  `db:"metadata"`
}

func (s *gatewayService) completeBusinessUpload(ctx context.Context, session *uploadSession, req *cloudpb.CompleteUploadRequest) (string, int64, bool, error) {
	if s.db == nil {
		return "", 0, false, nil
	}
	principal, err := principalFromContext(ctx)
	if err != nil {
		return "", 0, false, err
	}
	rawTags := req.GetRawTags()
	if err := requireMatchingRawTag(rawTags, "capture_id", session.ClientHints["capture_id"]); err != nil {
		return "", 0, false, err
	}
	if err := requireMatchingRawTag(rawTags, "task_id", session.ClientHints["task_id"]); err != nil {
		return "", 0, false, err
	}
	if err := requireMatchingRawTag(rawTags, "dc_plan_id", session.ClientHints["dc_plan_id"]); err != nil {
		return "", 0, false, err
	}
	if err := requireMatchingRawTag(rawTags, "workspace_id", fmt.Sprintf("%d", principal.WorkspaceID)); err != nil {
		return "", 0, false, err
	}
	if session.ClientHints["product"] == "ego_portal_lite" {
		if err := requireMatchingRawTag(rawTags, "checksum_md5", session.ClientHints["checksum_md5"]); err != nil {
			return "", 0, false, err
		}
		checksumMD5 := strings.ToLower(strings.TrimSpace(rawTags["checksum_md5"]))
		if !isMD5Hex(checksumMD5) {
			return "", 0, false, status.Error(codes.InvalidArgument, "checksum_md5 must be a 32-character hexadecimal MD5 digest")
		}
	}

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return "", 0, false, status.Error(codes.Unavailable, "begin upload transaction failed")
	}
	defer tx.Rollback() //nolint:errcheck // Safe after successful Commit.

	var task uploadTaskBinding
	taskQuery := `
		SELECT
			t.id,
			t.task_id,
			t.status,
			t.workstation_id,
			COALESCE(t.organization_id, ws.workspace_id) AS workspace_id,
			t.organization_id,
			t.dc_plan_id,
			t.local_dc_plan_id,
			dp.workspace_id AS plan_workspace_id,
			NULL AS episode_pk
		FROM tasks t
		INNER JOIN dc_plan dp ON dp.id = t.dc_plan_id AND dp.deleted_at IS NULL
		LEFT JOIN workstations ws ON ws.id = t.workstation_id AND ws.deleted_at IS NULL
		WHERE t.id = ? AND t.deleted_at IS NULL
		LIMIT 1
	`
	if tx.DriverName() != "sqlite" {
		taskQuery += " FOR UPDATE"
	}
	if err := tx.GetContext(ctx, &task, taskQuery, session.TaskPK); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", 0, false, status.Error(codes.NotFound, "task not found")
		}
		return "", 0, false, status.Error(codes.Unavailable, "task lookup unavailable")
	}
	if task.TaskID != session.TaskID || !task.DCPlanID.Valid || task.DCPlanID.Int64 != session.DCPlanID || task.PlanWorkspaceID != session.WorkspaceID {
		return "", 0, false, status.Error(codes.FailedPrecondition, "task plan changed")
	}

	var existing completedUploadEpisode
	err = tx.GetContext(ctx, &existing, `
		SELECT id, episode_id, mcap_path, file_size_bytes, duration_sec, metadata
		FROM episodes
		WHERE task_id = ? AND deleted_at IS NULL
		LIMIT 1
	`, task.TaskPK)
	if err == nil {
		if err := validateIdempotentComplete(existing, session, req); err != nil {
			return "", 0, false, err
		}
		now := s.now()
		if _, err := tx.ExecContext(ctx, `
			UPDATE tasks
			SET status = 'completed', completed_at = COALESCE(completed_at, ?),
				episode_id = COALESCE(episode_id, ?), error_message = NULL, updated_at = ?
			WHERE id = ? AND deleted_at IS NULL
		`, now, existing.ID, now, task.TaskPK); err != nil {
			return "", 0, false, status.Error(codes.Unavailable, "task completion failed")
		}
		if err := tx.Commit(); err != nil {
			return "", 0, false, status.Error(codes.Unavailable, "commit upload transaction failed")
		}
		return existing.EpisodeID, existing.ID, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, status.Error(codes.Unavailable, "episode lookup unavailable")
	}
	switch task.Status {
	case "pending", "ready", "in_progress", "uploading", "completed":
	default:
		return "", 0, false, status.Error(codes.FailedPrecondition, "task is not completable")
	}

	now := s.now()
	episodeID := uuid.NewString()
	metadata, err := uploadEpisodeMetadata(session, req)
	if err != nil {
		return "", 0, false, status.Error(codes.InvalidArgument, "invalid upload metadata")
	}
	durationSec, err := uploadDurationSec(req.GetRawTags())
	if err != nil {
		return "", 0, false, err
	}
	insertRes, err := tx.ExecContext(ctx, `
		INSERT INTO episodes (
			episode_id,
			task_id,
			workstation_id,
			organization_id,
			dc_plan_id,
			local_dc_plan_id,
			mcap_path,
			sidecar_path,
			file_size_bytes,
			duration_sec,
			qa_status,
			metadata,
			created_at,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending_qa', ?, ?, ?)
	`, episodeID, task.TaskPK, task.WorkstationID, task.OrganizationID, task.DCPlanID, task.LocalDCPlanID,
		session.ObjectKey, "", req.GetFileSize(), durationSec, metadata, now, now)
	if err != nil {
		return "", 0, false, status.Error(codes.Unavailable, "episode creation failed")
	}
	episodePK, err := insertRes.LastInsertId()
	if err != nil {
		return "", 0, false, status.Error(codes.Unavailable, "episode id read failed")
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE tasks
		SET status = 'completed',
			completed_at = CASE WHEN completed_at IS NULL THEN ? ELSE completed_at END,
			episode_id = COALESCE(episode_id, ?),
			error_message = NULL,
			updated_at = ?
		WHERE id = ? AND deleted_at IS NULL
	`, now, episodePK, now, task.TaskPK); err != nil {
		return "", 0, false, status.Error(codes.Unavailable, "task completion failed")
	}
	if err := tx.Commit(); err != nil {
		return "", 0, false, status.Error(codes.Unavailable, "commit upload transaction failed")
	}
	return episodeID, episodePK, true, nil
}

func parsePositiveInt64Hint(hints map[string]string, key string) (int64, error) {
	value := strings.TrimSpace(hints[key])
	if value == "" {
		return 0, status.Errorf(codes.InvalidArgument, "%s is required", key)
	}
	parsed, err := parseInt64(value)
	if err != nil || parsed <= 0 {
		return 0, status.Errorf(codes.InvalidArgument, "%s must be a positive integer", key)
	}
	return parsed, nil
}

func parseInt64(value string) (int64, error) {
	var parsed int64
	_, err := fmt.Sscan(strings.TrimSpace(value), &parsed)
	return parsed, err
}

func requireMatchingRawTag(tags map[string]string, key, expected string) error {
	actual := strings.TrimSpace(tags[key])
	expected = strings.TrimSpace(expected)
	if actual == "" || actual != expected {
		return status.Errorf(codes.FailedPrecondition, "%s does not match upload session", key)
	}
	return nil
}

func isMD5Hex(value string) bool {
	if len(value) != 32 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validateIdempotentComplete(episode completedUploadEpisode, session *uploadSession, req *cloudpb.CompleteUploadRequest) error {
	if episode.MCAPPath != session.ObjectKey {
		return status.Error(codes.FailedPrecondition, "object key differs from completed upload")
	}
	if episode.FileSize.Valid && episode.FileSize.Int64 != req.GetFileSize() {
		return status.Error(codes.FailedPrecondition, "file_size differs from completed upload")
	}
	if episode.Duration.Valid {
		durationSec, err := uploadDurationSec(req.GetRawTags())
		if err != nil {
			return err
		}
		if !durationSec.Valid || math.Abs(episode.Duration.Float64-durationSec.Float64) > 0.000001 {
			return status.Error(codes.FailedPrecondition, "duration_sec differs from completed upload")
		}
	}
	if !episode.Metadata.Valid {
		return status.Error(codes.FailedPrecondition, "completed upload metadata is missing")
	}
	var metadata struct {
		UploadID           string `json:"upload_id"`
		CaptureID          string `json:"capture_id"`
		ObjectETag         string `json:"object_etag"`
		CompletedPartCount int32  `json:"completed_part_count"`
	}
	if err := json.Unmarshal([]byte(episode.Metadata.String), &metadata); err != nil {
		return status.Error(codes.FailedPrecondition, "completed upload metadata is invalid")
	}
	if metadata.UploadID != session.UploadID || metadata.CaptureID != session.ClientHints["capture_id"] {
		return status.Error(codes.FailedPrecondition, "upload identity differs from completed upload")
	}
	if strings.TrimSpace(metadata.ObjectETag) != strings.TrimSpace(req.GetObjectEtag()) {
		return status.Error(codes.FailedPrecondition, "object_etag differs from completed upload")
	}
	if metadata.CompletedPartCount != req.GetCompletedPartCount() {
		return status.Error(codes.FailedPrecondition, "completed_part_count differs from completed upload")
	}
	return nil
}

func uploadDurationSec(tags map[string]string) (sql.NullFloat64, error) {
	value := strings.TrimSpace(tags["duration_sec"])
	if value == "" {
		return sql.NullFloat64{}, nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || parsed <= 0 {
		return sql.NullFloat64{}, status.Error(codes.InvalidArgument, "duration_sec must be a positive number")
	}
	return sql.NullFloat64{Float64: parsed, Valid: true}, nil
}

func uploadEpisodeMetadata(session *uploadSession, req *cloudpb.CompleteUploadRequest) (string, error) {
	payload := map[string]any{
		"source":               "dgwcompat",
		"logical_upload_id":    session.LogicalUploadID,
		"upload_id":            session.UploadID,
		"bucket":               session.Bucket,
		"endpoint":             session.Endpoint,
		"object_key":           session.ObjectKey,
		"object_etag":          strings.TrimSpace(req.GetObjectEtag()),
		"completed_part_count": req.GetCompletedPartCount(),
		"capture_id":           session.ClientHints["capture_id"],
		"client_hints":         session.ClientHints,
		"raw_tags":             req.GetRawTags(),
	}
	if session.ClientHints["product"] == "ego_portal_lite" {
		payload["product"] = "ego_portal_lite"
		payload["checksum_md5"] = strings.ToLower(strings.TrimSpace(req.GetRawTags()["checksum_md5"]))
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func authorizeUploadSession(ctx context.Context, session *uploadSession) error {
	principal, err := principalFromContext(ctx)
	if err != nil {
		return err
	}
	if session.DeviceID != principal.DeviceID || session.WorkspaceID != principal.WorkspaceID || session.AuthEpoch != principal.AuthEpoch {
		return status.Error(codes.PermissionDenied, "upload session belongs to another device principal")
	}
	return nil
}

func (s *gatewayService) uploadCredentials(session *uploadSession, creds stsCredentials) *cloudpb.UploadCredentials {
	return &cloudpb.UploadCredentials{
		Bucket:             session.Bucket,
		Endpoint:           session.Endpoint,
		ObjectKey:          session.ObjectKey,
		StsAccessKeyId:     creds.AccessKeyID,
		StsAccessKeySecret: creds.AccessKeySecret,
		StsSecurityToken:   creds.SecurityToken,
		StsExpireAtUnix:    creds.Expiration.Unix(),
		PartSizeBytes:      s.cfg.UploadPartSize,
		ObjectStoreBackend: "volcengine_tos",
		ObjectStoreRegion:  s.cfg.TOSRegion,
	}
}

func cloneMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (s *gatewayService) String() string {
	return fmt.Sprintf("dgwcompat gateway backend=volcengine_tos bucket=%s endpoint=%s region=%s", s.cfg.TOSBucket, s.cfg.TOSEndpoint, s.cfg.TOSRegion)
}
