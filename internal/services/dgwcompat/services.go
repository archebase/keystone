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

	keystoneauth "archebase.com/keystone-edge/internal/auth"
	"archebase.com/keystone-edge/internal/cloud/cloudpb"
	"archebase.com/keystone-edge/internal/config"
	"archebase.com/keystone-edge/internal/logger"
	keystoneServices "archebase.com/keystone-edge/internal/services"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	dataGatewayEpisodeIngestionChannel = "data_gateway"
	dataGatewayEpisodeStorageBackend   = "keystone_tos"
)

type uploadSession struct {
	Kind                   uploadKind
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
	AutoAssignedTask       bool
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

type hilbertDCPlanBinder interface {
	PatchDCPlanDCDeviceID(ctx context.Context, workspaceID, planID, deviceID int64) (bool, error)
}

type gatewayService struct {
	cloudpb.UnimplementedDataGatewayServiceServer
	cfg      Config
	db       *sqlx.DB
	sts      stsProvider
	sessions *sessionStore
	qa       episodeQAEnqueuer
	hilbert  hilbertDCPlanBinder
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
		hilbert: keystoneauth.NewHilbertClient(&config.HilbertConfig{
			BaseURL:        cfg.HilbertBaseURL,
			TimeoutSeconds: 5,
			AccessKey:      cfg.HilbertAccessKey,
			SecretKey:      cfg.HilbertSecretKey,
		}),
		now: func() time.Time { return time.Now().UTC() },
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
	autoAssignTask := req.GetAutoAssignTask()
	if autoAssignTask {
		if req.GetDcPlanId() <= 0 {
			return nil, status.Error(codes.InvalidArgument, "dc_plan_id is required for automatic task assignment")
		}
		planID := strconv.FormatInt(req.GetDcPlanId(), 10)
		if hinted := strings.TrimSpace(hints["dc_plan_id"]); hinted != "" && hinted != planID {
			return nil, status.Error(codes.FailedPrecondition, "dc_plan_id hints do not match request")
		}
		if strings.TrimSpace(hints["task_id"]) != "" {
			return nil, status.Error(codes.InvalidArgument, "task_id cannot be provided with automatic task assignment")
		}
		hints["dc_plan_id"] = planID
		hints["auto_assign_task"] = "true"
	}
	if hints["product"] == "ego_portal_lite" &&
		!strings.EqualFold(strings.TrimSpace(hints["upload_kind"]), string(uploadKindCalibrationCapture)) {
		checksumMD5 := strings.ToLower(strings.TrimSpace(hints["checksum_md5"]))
		if !isMD5Hex(checksumMD5) {
			return nil, status.Error(codes.InvalidArgument, "checksum_md5 must be a 32-character hexadecimal MD5 digest")
		}
		checksumSHA256 := strings.ToLower(strings.TrimSpace(hints["checksum_sha256"]))
		if !isSHA256Hex(checksumSHA256) {
			return nil, status.Error(codes.InvalidArgument, "checksum_sha256 must be a 64-character hexadecimal SHA-256 digest")
		}
		hints["checksum_md5"] = checksumMD5
		hints["checksum_sha256"] = checksumSHA256
	}
	if hinted := strings.TrimSpace(hints["device_id"]); hinted != "" && hinted != principal.DeviceID {
		return nil, status.Error(codes.PermissionDenied, "client device hint does not match authenticated device")
	}
	if hinted := strings.TrimSpace(hints["workspace_id"]); hinted != "" && hinted != fmt.Sprintf("%d", principal.WorkspaceID) {
		return nil, status.Error(codes.PermissionDenied, "client workspace hint does not match authenticated device")
	}
	hints["device_id"] = principal.DeviceID
	hints["workspace_id"] = fmt.Sprintf("%d", principal.WorkspaceID)
	intent, err := parseUploadIntent(hints)
	if err != nil {
		return nil, err
	}
	var taskBinding uploadTaskBinding
	if intent.Kind == uploadKindTaskEpisode {
		if autoAssignTask {
			assigned, assignErr := s.assignTaskForDevice(ctx, principal, req.GetDcPlanId())
			if assignErr != nil {
				return nil, assignErr
			}
			hints["task_id"] = assigned.TaskID
		}
		taskBinding, err = s.validateCreateLogicalUpload(ctx, principal, hints)
		if err != nil {
			if autoAssignTask {
				s.releaseAutoAssignedTask(ctx, principal, hints["task_id"], "automatic task assignment validation failed")
			}
			return nil, err
		}
	}
	objectKey := buildObjectKey(s.cfg.TOSKeyPrefix, hints, uploadID)
	session := &uploadSession{
		Kind:             intent.Kind,
		LogicalUploadID:  logicalUploadID,
		UploadID:         uploadID,
		RobotID:          principal.RobotID,
		TaskPK:           taskBinding.TaskPK,
		TaskID:           taskBinding.TaskID,
		WorkstationID:    taskBinding.WorkstationID,
		OrganizationID:   taskBinding.OrganizationID,
		DCPlanID:         taskBinding.DCPlanID.Int64,
		LocalDCPlanID:    taskBinding.LocalDCPlanID,
		DeviceID:         principal.DeviceID,
		WorkspaceID:      principal.WorkspaceID,
		AuthEpoch:        principal.AuthEpoch,
		Bucket:           s.cfg.TOSBucket,
		Endpoint:         s.cfg.TOSEndpoint,
		ObjectKey:        objectKey,
		ClientHints:      hints,
		CreatedAt:        s.now(),
		AutoAssignedTask: autoAssignTask,
	}
	if intent.Kind == uploadKindCalibrationCapture {
		if err := s.persistCalibrationUploadStart(ctx, principal, intent, session); err != nil {
			logger.Printf("[DGW_COMPAT] CreateLogicalUpload calibration persistence failed capture_id=%s session_id=%s attempt_no=%d error=%v",
				intent.CaptureID, intent.CalibrationSessionID, intent.AttemptNo, err)
			return nil, err
		}
	}
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
		if autoAssignTask {
			s.releaseAutoAssignedTask(ctx, principal, hints["task_id"], "failed to create object-store upload credentials")
		}
		logger.Printf("[DGW_COMPAT] CreateLogicalUpload assume_role_failed upload_id=%s logical_upload_id=%s bucket=%s object_key=%s error=%v",
			uploadID, logicalUploadID, s.cfg.TOSBucket, objectKey, err)
		return nil, status.Errorf(codes.Unavailable, "assume role: %v", err)
	}
	session.LastSTSExpireAt = creds.Expiration
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
	response := &cloudpb.CreateLogicalUploadResponse{
		LogicalUploadId: logicalUploadID,
		UploadId:        uploadID,
		Credentials:     s.uploadCredentials(session, creds),
	}
	if autoAssignTask {
		response.ResolvedTaskId = taskBinding.TaskID
		response.ResolvedDcPlanId = taskBinding.DCPlanID.Int64
		response.ResolvedWorkspaceId = taskBinding.PlanWorkspaceID
	}
	return response, nil
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
	if updated.AutoAssignedTask {
		principal, principalErr := principalFromContext(ctx)
		if principalErr == nil {
			s.releaseAutoAssignedTask(ctx, principal, updated.TaskID, "keystone import aborted")
		}
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
	var episodeID string
	var episodePK int64
	var episodeCreated bool
	var err error
	if session.Kind == uploadKindCalibrationCapture {
		if err := s.completeCalibrationUpload(ctx, session, req); err != nil {
			logger.Printf("[DGW_COMPAT] CompleteUpload calibration persistence failed upload_id=%s capture_id=%s session_id=%s error=%v",
				session.UploadID, session.ClientHints["capture_id"], session.ClientHints["calibration_session_id"], err)
			return nil, err
		}
	} else {
		episodeID, episodePK, episodeCreated, err = s.completeBusinessUpload(ctx, session, req)
		if err != nil {
			return nil, err
		}
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
	RobotID         sql.NullInt64 `db:"robot_id"`
	WorkspaceID     sql.NullInt64 `db:"workspace_id"`
	OrganizationID  sql.NullInt64 `db:"organization_id"`
	DCPlanID        sql.NullInt64 `db:"dc_plan_id"`
	LocalDCPlanID   sql.NullInt64 `db:"local_dc_plan_id"`
	PlanWorkspaceID int64         `db:"plan_workspace_id"`
	PlanDeviceID    sql.NullInt64 `db:"plan_device_id"`
	PlanOperator    string        `db:"plan_operator"`
	EpisodePK       sql.NullInt64 `db:"episode_pk"`
}

// assignTaskForDevice obtains and atomically reserves the next task for a
// plan bound to the authenticated device. It deliberately resolves the
// workstation without activating it, so an offline import cannot replace the
// real Ego Portal workstation session.
func (s *gatewayService) assignTaskForDevice(
	ctx context.Context,
	principal devicePrincipal,
	planID int64,
) (keystoneServices.DCPlanSuppliedTask, error) {
	if s == nil || s.db == nil || planID <= 0 {
		return keystoneServices.DCPlanSuppliedTask{}, status.Error(codes.Unavailable, "task assignment unavailable")
	}

	var plan struct {
		WorkspaceID int64          `db:"workspace_id"`
		DeviceID    sql.NullString `db:"device_id"`
		Operator    string         `db:"operator"`
	}
	if err := s.db.GetContext(ctx, &plan, `
		SELECT workspace_id, CAST(dc_device_id AS CHAR) AS device_id, operator
		FROM dc_plan
		WHERE id = ? AND deleted_at IS NULL
		LIMIT 1
	`, planID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return keystoneServices.DCPlanSuppliedTask{}, status.Error(codes.NotFound, "dc plan not found")
		}
		return keystoneServices.DCPlanSuppliedTask{}, status.Error(codes.Unavailable, "dc plan lookup unavailable")
	}
	if plan.WorkspaceID != principal.WorkspaceID {
		return keystoneServices.DCPlanSuppliedTask{}, status.Error(codes.PermissionDenied, "dc plan belongs to another workspace")
	}
	deviceID := strings.TrimSpace(principal.DeviceID)
	if !plan.DeviceID.Valid {
		hilbertDeviceID, parseErr := parseHilbertDeviceID(deviceID)
		if parseErr != nil || s.hilbert == nil {
			return keystoneServices.DCPlanSuppliedTask{}, status.Error(codes.FailedPrecondition, "dc plan device binding unavailable")
		}
		bound, bindErr := s.hilbert.PatchDCPlanDCDeviceID(ctx, plan.WorkspaceID, planID, hilbertDeviceID)
		if bindErr != nil || !bound {
			return keystoneServices.DCPlanSuppliedTask{}, status.Error(codes.Aborted, "dc plan device binding failed")
		}
		now := s.now()
		if _, updateErr := s.db.ExecContext(ctx, `
			UPDATE dc_plan SET dc_device_id = ?, dc_device_name = ?, local_updated_at = ?
			WHERE id = ? AND workspace_id = ? AND deleted_at IS NULL
		`, hilbertDeviceID, deviceID, now, planID, principal.WorkspaceID); updateErr != nil {
			return keystoneServices.DCPlanSuppliedTask{}, status.Error(codes.Unavailable, "dc plan device projection unavailable")
		}
		if projectionErr := keystoneServices.EnsureDCPlanWorkstation(ctx, s.db, keystoneauth.HilbertDCPlan{
			ID:          planID,
			WorkspaceID: plan.WorkspaceID,
			Operator:    plan.Operator,
			DCDeviceID:  &hilbertDeviceID,
		}, now); projectionErr != nil {
			logger.Printf("[DGW_COMPAT] project workstation after dc plan device binding failed: workspace_id=%d dc_plan_id=%d device_id=%s error=%v", plan.WorkspaceID, planID, deviceID, projectionErr)
			return keystoneServices.DCPlanSuppliedTask{}, status.Error(codes.FailedPrecondition, "no workstation is assigned to this device for the dc plan")
		}
		plan.DeviceID = sql.NullString{String: deviceID, Valid: true}
	}
	if strings.TrimSpace(plan.DeviceID.String) != deviceID {
		return keystoneServices.DCPlanSuppliedTask{}, status.Error(codes.PermissionDenied, "dc plan is not assigned to this device")
	}

	var workstationID int64
	if err := s.db.GetContext(ctx, &workstationID, `
		SELECT ws.id
		FROM workstations ws
		INNER JOIN data_collectors dc ON dc.id = ws.data_collector_id AND dc.deleted_at IS NULL
		INNER JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
		WHERE ws.workspace_id = ?
			AND ws.deleted_at IS NULL
			AND dc.operator_id = ?
			AND r.id = ?
			AND r.device_id = ?
		LIMIT 1
	`, plan.WorkspaceID, strings.TrimSpace(plan.Operator), principal.RobotID, strings.TrimSpace(principal.DeviceID)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return keystoneServices.DCPlanSuppliedTask{}, status.Error(codes.PermissionDenied, "no workstation is assigned to this device for the dc plan")
		}
		return keystoneServices.DCPlanSuppliedTask{}, status.Error(codes.Unavailable, "workstation lookup unavailable")
	}

	supply := keystoneServices.NewDCPlanTaskSupplyService(s.db)
	for attempt := 0; attempt < 5; attempt++ {
		result, err := supply.EnsureNextTask(ctx, planID, workstationID, s.now())
		if err != nil {
			switch {
			case errors.Is(err, keystoneServices.ErrDCPlanTaskSupplyNotFound):
				return keystoneServices.DCPlanSuppliedTask{}, status.Error(codes.NotFound, "dc plan not found")
			case errors.Is(err, keystoneServices.ErrDCPlanTaskSupplyWorkstationMismatch):
				return keystoneServices.DCPlanSuppliedTask{}, status.Error(codes.PermissionDenied, "dc plan workstation mismatch")
			case errors.Is(err, keystoneServices.ErrDCPlanTaskSupplyTargetReached):
				return keystoneServices.DCPlanSuppliedTask{}, status.Error(codes.FailedPrecondition, "dc plan target has been reached")
			case errors.Is(err, keystoneServices.ErrDCPlanTaskSupplyActiveTask):
				return keystoneServices.DCPlanSuppliedTask{}, status.Error(codes.Aborted, "dc plan has an active task")
			default:
				return keystoneServices.DCPlanSuppliedTask{}, status.Error(codes.Unavailable, "task assignment unavailable")
			}
		}
		if result == nil || result.Task.ID <= 0 || strings.TrimSpace(result.Task.TaskID) == "" {
			return keystoneServices.DCPlanSuppliedTask{}, status.Error(codes.Unavailable, "task assignment returned an incomplete task")
		}
		updated, err := s.db.ExecContext(ctx, `
			UPDATE tasks
			SET status = 'uploading', error_message = NULL, updated_at = ?
			WHERE id = ? AND status = 'pending' AND deleted_at IS NULL
		`, s.now(), result.Task.ID)
		if err != nil {
			return keystoneServices.DCPlanSuppliedTask{}, status.Error(codes.Unavailable, "reserve task unavailable")
		}
		affected, err := updated.RowsAffected()
		if err != nil {
			return keystoneServices.DCPlanSuppliedTask{}, status.Error(codes.Unavailable, "reserve task result unavailable")
		}
		if affected == 1 {
			result.Task.Status = "uploading"
			return result.Task, nil
		}
	}
	return keystoneServices.DCPlanSuppliedTask{}, status.Error(codes.Aborted, "could not reserve a pending task")
}

func (s *gatewayService) releaseAutoAssignedTask(
	ctx context.Context,
	principal devicePrincipal,
	taskID string,
	reason string,
) {
	if s == nil || s.db == nil || strings.TrimSpace(taskID) == "" {
		return
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE tasks
		SET status = 'failed',
			completed_at = COALESCE(completed_at, ?),
			error_message = ?,
			updated_at = ?
		WHERE task_id = ? AND status = 'uploading' AND episode_id IS NULL AND deleted_at IS NULL
			AND EXISTS (
				SELECT 1
				FROM workstations ws
				INNER JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
				WHERE ws.id = tasks.workstation_id
					AND ws.deleted_at IS NULL
					AND r.id = ? AND r.device_id = ?
			)
	`, s.now(), strings.TrimSpace(reason), s.now(), strings.TrimSpace(taskID), principal.RobotID, strings.TrimSpace(principal.DeviceID)); err != nil {
		logger.Printf("[DGW_COMPAT] failed to release auto-assigned task task_id=%s device_id=%s error=%v", taskID, principal.DeviceID, err)
	}
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
			ws.robot_id,
			COALESCE(t.organization_id, ws.workspace_id) AS workspace_id,
			t.organization_id,
			t.dc_plan_id,
			t.local_dc_plan_id,
			dp.workspace_id AS plan_workspace_id,
			dp.dc_device_id AS plan_device_id,
			dp.operator AS plan_operator,
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
	if !row.WorkstationID.Valid || !row.RobotID.Valid || row.RobotID.Int64 != principal.RobotID {
		return uploadTaskBinding{}, status.Error(codes.PermissionDenied, "task workstation does not belong to authenticated device")
	}
	switch row.Status {
	case "pending", "ready", "in_progress", "uploading":
	default:
		return uploadTaskBinding{}, status.Error(codes.FailedPrecondition, "task is not uploadable")
	}
	if err := s.bindDCPlanDeviceOnFirstUpload(ctx, principal, row); err != nil {
		return uploadTaskBinding{}, err
	}
	return row, nil
}

// bindDCPlanDeviceOnFirstUpload binds a Hilbert dc plan to the authenticated device when the
// plan has not been bound yet. It is invoked on the first real upload path, which is the common
// point shared by ego-portal and ego-portal-lite after an operator selects a task.
func (s *gatewayService) bindDCPlanDeviceOnFirstUpload(
	ctx context.Context,
	principal devicePrincipal,
	row uploadTaskBinding,
) error {
	if s == nil || s.hilbert == nil || !row.DCPlanID.Valid {
		return nil
	}
	deviceID, err := parseHilbertDeviceID(strings.TrimSpace(principal.DeviceID))
	if err != nil {
		return status.Error(codes.FailedPrecondition, "dc plan device binding unavailable")
	}
	if row.PlanDeviceID.Valid && row.PlanDeviceID.Int64 == deviceID {
		return nil
	}
	if row.PlanDeviceID.Valid {
		return status.Error(codes.PermissionDenied, "dc plan is not assigned to this device")
	}
	if err := keystoneServices.BindDCPlanDevice(ctx, s.db, s.hilbert, row.PlanWorkspaceID, row.DCPlanID.Int64, deviceID); err != nil {
		return status.Error(codes.Aborted, "dc plan device binding failed")
	}
	return nil
}

type completedUploadEpisode struct {
	ID                      int64           `db:"id"`
	EpisodeID               string          `db:"episode_id"`
	IngestionChannel        string          `db:"ingestion_channel"`
	StorageBackend          string          `db:"storage_backend"`
	MCAPPath                string          `db:"mcap_path"`
	FileSize                sql.NullInt64   `db:"file_size_bytes"`
	Duration                sql.NullFloat64 `db:"duration_sec"`
	Checksum                sql.NullString  `db:"checksum"`
	Metadata                sql.NullString  `db:"metadata"`
	CameraSerial            sql.NullString  `db:"camera_serial"`
	CalibrationCaptureID    sql.NullString  `db:"calibration_capture_id"`
	CalibrationResultSHA256 sql.NullString  `db:"calibration_result_sha256"`
}

type episodeCalibrationSelection struct {
	CaptureID    string `db:"capture_id"`
	ResultSHA256 string `db:"result_checksum_sha256"`
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
	if err := requireOptionalMatchingRawTag(rawTags, "camera_serial", session.ClientHints["camera_serial"]); err != nil {
		return "", 0, false, err
	}
	var episodeChecksumSHA256 sql.NullString
	if session.ClientHints["product"] == "ego_portal_lite" {
		if err := requireMatchingDigestRawTag(rawTags, "checksum_md5", session.ClientHints["checksum_md5"]); err != nil {
			return "", 0, false, err
		}
		checksumMD5 := strings.ToLower(strings.TrimSpace(rawTags["checksum_md5"]))
		if !isMD5Hex(checksumMD5) {
			return "", 0, false, status.Error(codes.InvalidArgument, "checksum_md5 must be a 32-character hexadecimal MD5 digest")
		}
		if err := requireMatchingDigestRawTag(rawTags, "checksum_sha256", session.ClientHints["checksum_sha256"]); err != nil {
			return "", 0, false, err
		}
		checksumSHA256 := strings.ToLower(strings.TrimSpace(rawTags["checksum_sha256"]))
		if !isSHA256Hex(checksumSHA256) {
			return "", 0, false, status.Error(codes.InvalidArgument, "checksum_sha256 must be a 64-character hexadecimal SHA-256 digest")
		}
		episodeChecksumSHA256 = sql.NullString{String: checksumSHA256, Valid: true}
	} else if isEgoPortalUpload(session) {
		checksumSHA256 := strings.ToLower(strings.TrimSpace(rawTags["recording.checksum_sha256"]))
		if checksumSHA256 != "" {
			if !isSHA256Hex(checksumSHA256) {
				return "", 0, false, status.Error(codes.InvalidArgument, "recording.checksum_sha256 must be a 64-character hexadecimal SHA-256 digest")
			}
			episodeChecksumSHA256 = sql.NullString{String: checksumSHA256, Valid: true}
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
		SELECT id, episode_id, ingestion_channel, storage_backend,
			mcap_path, file_size_bytes, duration_sec, checksum, metadata,
			camera_serial, calibration_capture_id, calibration_result_sha256
		FROM episodes
		WHERE task_id = ? AND deleted_at IS NULL
		LIMIT 1
	`, task.TaskPK)
	if err == nil {
		if existing.IngestionChannel != dataGatewayEpisodeIngestionChannel ||
			existing.StorageBackend != dataGatewayEpisodeStorageBackend {
			return "", 0, false, status.Errorf(
				codes.FailedPrecondition,
				"episode provenance differs from upload path: existing=%s/%s expected=%s/%s",
				existing.IngestionChannel,
				existing.StorageBackend,
				dataGatewayEpisodeIngestionChannel,
				dataGatewayEpisodeStorageBackend,
			)
		}
		if err := validateIdempotentComplete(existing, session, req, episodeChecksumSHA256); err != nil {
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
	selection, matched, err := s.resolveSuccessfulCalibration(ctx, tx, session.ClientHints["camera_serial"])
	if err != nil {
		logger.Printf("[DGW_COMPAT] resolve camera calibration failed camera_serial=%q: %v",
			session.ClientHints["camera_serial"], err)
		return "", 0, false, status.Error(codes.Unavailable, "camera calibration lookup unavailable")
	}
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
			ingestion_channel,
			storage_backend,
			mcap_path,
			sidecar_path,
			file_size_bytes,
			duration_sec,
			checksum,
			qa_status,
			metadata,
			camera_serial,
			calibration_capture_id,
			calibration_result_sha256,
			created_at,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending_qa', ?, NULLIF(?, ''), ?, ?, ?, ?)
	`, episodeID, task.TaskPK, task.WorkstationID, task.OrganizationID, task.DCPlanID, task.LocalDCPlanID,
		dataGatewayEpisodeIngestionChannel, dataGatewayEpisodeStorageBackend,
		session.ObjectKey, "", req.GetFileSize(), durationSec, episodeChecksumSHA256, metadata,
		session.ClientHints["camera_serial"], nullableCalibrationValue(matched, selection.CaptureID),
		nullableCalibrationValue(matched, selection.ResultSHA256), now, now)
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

func (s *gatewayService) resolveSuccessfulCalibration(
	ctx context.Context,
	tx *sqlx.Tx,
	cameraSerial string,
) (episodeCalibrationSelection, bool, error) {
	cameraSerial = strings.TrimSpace(cameraSerial)
	if cameraSerial == "" {
		return episodeCalibrationSelection{}, false, nil
	}
	comparison := "s.camera_serial = ?"
	if s.db.DriverName() != "sqlite" {
		comparison = "BINARY s.camera_serial = BINARY ?"
	}
	var selection episodeCalibrationSelection
	err := tx.GetContext(ctx, &selection, `
		SELECT c.capture_id, c.result_checksum_sha256
		FROM calibration_sessions s
		INNER JOIN calibration_captures c
			ON c.capture_id = s.successful_capture_id
			AND c.calibration_session_id = s.session_id
		WHERE `+comparison+`
		  AND s.status = 'succeeded'
		  AND c.status = 'succeeded'
		  AND c.result_object_key IS NOT NULL
		  AND c.result_size_bytes > 0
		  AND c.result_checksum_sha256 IS NOT NULL
		ORDER BY c.processing_finished_at DESC, c.id DESC
		LIMIT 1
	`, cameraSerial)
	if errors.Is(err, sql.ErrNoRows) {
		return episodeCalibrationSelection{}, false, nil
	}
	if err != nil {
		return episodeCalibrationSelection{}, false, fmt.Errorf("query successful calibration: %w", err)
	}
	if !isSHA256Hex(strings.ToLower(selection.ResultSHA256)) {
		return episodeCalibrationSelection{}, false, fmt.Errorf("matched calibration result has invalid SHA-256")
	}
	selection.ResultSHA256 = strings.ToLower(selection.ResultSHA256)
	return selection, true, nil
}

func nullableCalibrationValue(matched bool, value string) any {
	if !matched {
		return nil
	}
	return value
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

func requireOptionalMatchingRawTag(tags map[string]string, key, expected string) error {
	actual := strings.TrimSpace(tags[key])
	expected = strings.TrimSpace(expected)
	if actual == "" && expected == "" {
		return nil
	}
	if actual == "" || actual != expected {
		return status.Errorf(codes.FailedPrecondition, "%s does not match upload session", key)
	}
	return nil
}

func requireMatchingDigestRawTag(tags map[string]string, key, expected string) error {
	actual := strings.ToLower(strings.TrimSpace(tags[key]))
	expected = strings.ToLower(strings.TrimSpace(expected))
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

func isSHA256Hex(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func isEgoPortalUpload(session *uploadSession) bool {
	return session.ClientHints["product"] != "ego_portal_lite" &&
		strings.EqualFold(strings.TrimSpace(session.ClientHints["source"]), "ego-portal")
}

func validateIdempotentComplete(
	episode completedUploadEpisode,
	session *uploadSession,
	req *cloudpb.CompleteUploadRequest,
	episodeChecksumSHA256 sql.NullString,
) error {
	expectedCameraSerial := strings.TrimSpace(session.ClientHints["camera_serial"])
	storedCameraSerial := strings.TrimSpace(episode.CameraSerial.String)
	if expectedCameraSerial == "" {
		if episode.CameraSerial.Valid && storedCameraSerial != "" {
			return status.Error(codes.FailedPrecondition, "camera_serial differs from completed upload")
		}
	} else if !episode.CameraSerial.Valid || storedCameraSerial != expectedCameraSerial {
		return status.Error(codes.FailedPrecondition, "camera_serial differs from completed upload")
	}
	storedCaptureID := strings.TrimSpace(episode.CalibrationCaptureID.String)
	storedResultSHA256 := strings.ToLower(strings.TrimSpace(episode.CalibrationResultSHA256.String))
	if (episode.CalibrationCaptureID.Valid && storedCaptureID == "") ||
		(episode.CalibrationResultSHA256.Valid && !isSHA256Hex(storedResultSHA256)) ||
		(episode.CalibrationCaptureID.Valid != episode.CalibrationResultSHA256.Valid) ||
		(episode.CalibrationCaptureID.Valid && expectedCameraSerial == "") {
		return status.Error(codes.FailedPrecondition, "completed upload calibration selection is inconsistent")
	}
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
	if episode.Checksum.Valid {
		checksumTag := "checksum_sha256"
		requestChecksum := strings.ToLower(strings.TrimSpace(req.GetRawTags()[checksumTag]))
		compareChecksum := true
		if isEgoPortalUpload(session) {
			checksumTag = "recording.checksum_sha256"
			requestChecksum = episodeChecksumSHA256.String
			compareChecksum = episodeChecksumSHA256.Valid
		}
		if compareChecksum && episode.Checksum.String != requestChecksum {
			return status.Errorf(codes.FailedPrecondition, "%s differs from completed upload", checksumTag)
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
		payload["checksum_sha256"] = strings.ToLower(strings.TrimSpace(req.GetRawTags()["checksum_sha256"]))
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
