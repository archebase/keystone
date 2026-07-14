// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package dgwcompat

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"archebase.com/keystone-edge/internal/cloud/cloudpb"
	"archebase.com/keystone-edge/internal/logger"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type uploadSession struct {
	LogicalUploadID        string
	UploadID               string
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
	sts      stsProvider
	sessions *sessionStore
	now      func() time.Time
}

func newGatewayService(cfg Config, sts stsProvider, sessions *sessionStore) *gatewayService {
	return &gatewayService{
		cfg:      cfg,
		sts:      sts,
		sessions: sessions,
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
	if hinted := strings.TrimSpace(hints["device_id"]); hinted != "" && hinted != principal.DeviceID {
		return nil, status.Error(codes.PermissionDenied, "client device hint does not match authenticated device")
	}
	if hinted := strings.TrimSpace(hints["workspace_id"]); hinted != "" && hinted != fmt.Sprintf("%d", principal.WorkspaceID) {
		return nil, status.Error(codes.PermissionDenied, "client workspace hint does not match authenticated device")
	}
	hints["device_id"] = principal.DeviceID
	hints["workspace_id"] = fmt.Sprintf("%d", principal.WorkspaceID)
	objectKey := buildObjectKey(s.cfg.TOSKeyPrefix, hints, uploadID)
	creds, err := s.sts.AssumeRole(ctx, stsScope{Bucket: s.cfg.TOSBucket, ObjectKey: objectKey})
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "assume role: %v", err)
	}
	session := &uploadSession{
		LogicalUploadID: logicalUploadID,
		UploadID:        uploadID,
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
	logger.Printf("[DGW_COMPAT] CreateLogicalUpload upload_id=%s logical_upload_id=%s bucket=%s object_key=%s capture_id=%s task_id=%s device_id=%s",
		uploadID, logicalUploadID, s.cfg.TOSBucket, objectKey, hints["capture_id"], hints["task_id"], hints["device_id"])
	return &cloudpb.CreateLogicalUploadResponse{
		LogicalUploadId: logicalUploadID,
		UploadId:        uploadID,
		Credentials:     s.uploadCredentials(session, creds),
	}, nil
}

func (s *gatewayService) GetUploadRecovery(ctx context.Context, req *cloudpb.GetUploadRecoveryRequest) (*cloudpb.GetUploadRecoveryResponse, error) {
	session, ok := s.sessions.getByLogical(req.GetLogicalUploadId())
	if !ok {
		return nil, status.Error(codes.NotFound, "logical upload not found")
	}
	if err := authorizeUploadSession(ctx, session); err != nil {
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
		return nil, err
	}
	creds, err := s.sts.AssumeRole(ctx, stsScope{Bucket: session.Bucket, ObjectKey: session.ObjectKey})
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "assume role: %v", err)
	}
	updated, ok := s.sessions.update(session.UploadID, func(current *uploadSession) {
		current.CredentialRefreshCount++
		current.LastSTSExpireAt = creds.Expiration
	})
	if !ok {
		return nil, status.Error(codes.NotFound, "upload not found")
	}
	logger.Printf("[DGW_COMPAT] ReissueUploadCredentials upload_id=%s logical_upload_id=%s refresh_count=%d",
		updated.UploadID, updated.LogicalUploadID, updated.CredentialRefreshCount)
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
	return &cloudpb.CompleteUploadResponse{}, nil
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
