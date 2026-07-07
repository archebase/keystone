// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package dgwcompat

import (
	"context"
	"fmt"
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
	Bucket                 string
	Endpoint               string
	ObjectKey              string
	ClientHints            map[string]string
	CreatedAt              time.Time
	CompletedAt            time.Time
	Aborted                bool
	CompletedPartCount     int32
	OSSObjectETag          string
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

type authService struct {
	cloudpb.UnimplementedAuthServiceServer
}

func (s *authService) ExchangeCredential(context.Context, *cloudpb.ExchangeCredentialRequest) (*cloudpb.ExchangeCredentialResponse, error) {
	expiresAt := time.Now().UTC().Add(time.Hour).Unix()
	return &cloudpb.ExchangeCredentialResponse{
		AccessToken:   "keystone-poc-access-token",
		ExpiresAtUnix: expiresAt,
		TokenType:     "Bearer",
		KeyId:         "keystone-poc",
		KeyPrefix:     "keystone",
	}, nil
}

type deviceInitService struct {
	cloudpb.UnimplementedDeviceInitServiceServer
	apiKey string
}

func (s *deviceInitService) InitDevice(_ context.Context, req *cloudpb.InitDeviceRequest) (*cloudpb.InitDeviceResponse, error) {
	return s.response(req.GetDeviceId(), req.GetPlatform()), nil
}

func (s *deviceInitService) ReinitDevice(_ context.Context, req *cloudpb.ReinitDeviceRequest) (*cloudpb.InitDeviceResponse, error) {
	return s.response(req.GetDeviceId(), req.GetPlatform()), nil
}

func (s *deviceInitService) response(deviceID, platform string) *cloudpb.InitDeviceResponse {
	tags := map[string]string{
		"source": "ego_portal_lite",
	}
	if deviceID != "" {
		tags["device_id"] = deviceID
	}
	if platform != "" {
		tags["platform"] = platform
	}
	return &cloudpb.InitDeviceResponse{
		ApiKey: s.apiKey,
		Tags:   tags,
	}
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
	logicalUploadID := uuid.NewString()
	uploadID := uuid.NewString()
	hints := cloneMap(req.GetClientHints())
	objectKey := buildObjectKey(s.cfg.OSSKeyPrefix, hints, uploadID)
	creds, err := s.sts.AssumeRole(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "assume role: %v", err)
	}
	session := &uploadSession{
		LogicalUploadID: logicalUploadID,
		UploadID:        uploadID,
		Bucket:          s.cfg.OSSBucket,
		Endpoint:        s.cfg.OSSPublicEndpoint,
		ObjectKey:       objectKey,
		ClientHints:     hints,
		CreatedAt:       s.now(),
		LastSTSExpireAt: creds.Expiration,
	}
	s.sessions.put(session)
	logger.Printf("[DGW_COMPAT] CreateLogicalUpload upload_id=%s logical_upload_id=%s bucket=%s object_key=%s capture_id=%s task_id=%s device_id=%s",
		uploadID, logicalUploadID, s.cfg.OSSBucket, objectKey, hints["capture_id"], hints["task_id"], hints["device_id"])
	return &cloudpb.CreateLogicalUploadResponse{
		LogicalUploadId: logicalUploadID,
		UploadId:        uploadID,
		Credentials:     s.uploadCredentials(session, creds),
	}, nil
}

func (s *gatewayService) GetUploadRecovery(_ context.Context, req *cloudpb.GetUploadRecoveryRequest) (*cloudpb.GetUploadRecoveryResponse, error) {
	session, ok := s.sessions.getByLogical(req.GetLogicalUploadId())
	if !ok {
		return nil, status.Error(codes.NotFound, "logical upload not found")
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
		OssObjectEtag:          session.OSSObjectETag,
	}, nil
}

func (s *gatewayService) ReissueUploadCredentials(ctx context.Context, req *cloudpb.ReissueUploadCredentialsRequest) (*cloudpb.ReissueUploadCredentialsResponse, error) {
	session, ok := s.sessions.getByUpload(req.GetUploadId())
	if !ok {
		return nil, status.Error(codes.NotFound, "upload not found")
	}
	creds, err := s.sts.AssumeRole(ctx)
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

func (s *gatewayService) AbortUpload(_ context.Context, req *cloudpb.AbortUploadRequest) (*cloudpb.AbortUploadResponse, error) {
	session, ok := s.sessions.getByLogical(req.GetLogicalUploadId())
	if !ok {
		return nil, status.Error(codes.NotFound, "logical upload not found")
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

func (s *gatewayService) CompleteUpload(_ context.Context, req *cloudpb.CompleteUploadRequest) (*cloudpb.CompleteUploadResponse, error) {
	updated, ok := s.sessions.update(req.GetUploadId(), func(current *uploadSession) {
		current.CompletedAt = s.now()
		current.FileSize = req.GetFileSize()
		current.CompletedPartCount = req.GetCompletedPartCount()
		current.OSSObjectETag = req.GetOssObjectEtag()
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
		req.GetOssObjectEtag(),
		updated.ClientHints["capture_id"],
		updated.ClientHints["task_id"],
		updated.ClientHints["device_id"],
		rawTags["capture_id"],
		rawTags["task_id"],
		rawTags["device_id"],
	)
	return &cloudpb.CompleteUploadResponse{}, nil
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
	return fmt.Sprintf("dgwcompat gateway bucket=%s endpoint=%s", s.cfg.OSSBucket, s.cfg.OSSPublicEndpoint)
}
