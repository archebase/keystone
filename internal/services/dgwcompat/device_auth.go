// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package dgwcompat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	keystoneauth "archebase.com/keystone-edge/internal/auth"
	"archebase.com/keystone-edge/internal/cloud/cloudpb"
	"archebase.com/keystone-edge/internal/config"
	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/services/deviceauth"
)

type hilbertDeviceClient interface {
	GetDCDeviceAPIKey(ctx context.Context, workspaceID, deviceID int64) (string, error)
	GenerateDCDeviceAPIKey(ctx context.Context, workspaceID, deviceID int64) (string, error)
	DeleteDCDeviceAPIKey(ctx context.Context, workspaceID, deviceID int64) error
	ValidateDCDeviceAPIKey(ctx context.Context, workspaceID, deviceID int64, apiKey string) (bool, error)
}

type deviceIdentityService struct {
	db      *sqlx.DB
	hilbert hilbertDeviceClient
	auth    *deviceauth.Authenticator
	now     func() time.Time
}

func newDeviceIdentityService(db *sqlx.DB, cfg Config) *deviceIdentityService {
	return &deviceIdentityService{
		db: db,
		hilbert: keystoneauth.NewHilbertClient(&config.HilbertConfig{
			BaseURL:        cfg.HilbertBaseURL,
			TimeoutSeconds: 5,
			AccessKey:      cfg.HilbertAccessKey,
			SecretKey:      cfg.HilbertSecretKey,
		}),
		auth: deviceauth.New(db, deviceauth.Config{
			JWTSecret: cfg.DeviceJWTSecret,
			JWTTTL:    cfg.DeviceJWTTTL,
		}),
		now: func() time.Time { return time.Now().UTC() },
	}
}

func (s *deviceIdentityService) provisionAPIKey(ctx context.Context, principal devicePrincipal) (string, error) {
	hilbertDeviceID, err := parseHilbertDeviceID(principal.DeviceID)
	if err != nil {
		return "", err
	}
	apiKey, getErr := s.hilbert.GetDCDeviceAPIKey(ctx, principal.WorkspaceID, hilbertDeviceID)
	if getErr == nil && apiKey != "" {
		return apiKey, nil
	}
	apiKey, generateErr := s.hilbert.GenerateDCDeviceAPIKey(ctx, principal.WorkspaceID, hilbertDeviceID)
	if generateErr == nil && apiKey != "" {
		return apiKey, nil
	}
	apiKey, retryErr := s.hilbert.GetDCDeviceAPIKey(ctx, principal.WorkspaceID, hilbertDeviceID)
	if retryErr != nil {
		return "", fmt.Errorf("get/generate Hilbert device API key: get=%v generate=%v retry=%w", getErr, generateErr, retryErr)
	}
	return apiKey, nil
}

func parseHilbertDeviceID(deviceID string) (int64, error) {
	parsed, err := strconv.ParseInt(strings.TrimSpace(deviceID), 10, 64)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("invalid Hilbert device ID")
	}
	return parsed, nil
}

type authService struct {
	cloudpb.UnimplementedAuthServiceServer
	identity *deviceIdentityService
}

func (s *authService) ExchangeCredential(ctx context.Context, req *cloudpb.ExchangeCredentialRequest) (*cloudpb.ExchangeCredentialResponse, error) {
	deviceID := strings.TrimSpace(req.GetDeviceId())
	credential := strings.TrimSpace(req.GetCredential())
	if deviceID == "" || credential == "" {
		return nil, status.Error(codes.InvalidArgument, "device_id and credential are required")
	}
	hilbertDeviceID, err := parseHilbertDeviceID(deviceID)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "device_id must be a positive decimal integer")
	}
	principal, err := s.identity.lookupActiveDevice(ctx, deviceID)
	if err != nil {
		return nil, err
	}
	valid, err := s.identity.hilbert.ValidateDCDeviceAPIKey(ctx, principal.WorkspaceID, hilbertDeviceID, credential)
	if err != nil {
		logger.Printf("[DGW_COMPAT] device API key validation failed: device_id=%s workspace_id=%d error=%v", deviceID, principal.WorkspaceID, err)
		return nil, status.Error(codes.Unavailable, "device authentication unavailable")
	}
	if !valid {
		logger.Printf("[DGW_COMPAT] device API key rejected: device_id=%s workspace_id=%d", deviceID, principal.WorkspaceID)
		return nil, status.Error(codes.Unauthenticated, "invalid device credential")
	}
	now := s.identity.now()
	token, expiresAt, err := s.identity.auth.IssueJWT(principal, now)
	if err != nil {
		return nil, status.Error(codes.Internal, "failed to issue device token")
	}
	return &cloudpb.ExchangeCredentialResponse{
		AccessToken:   token,
		ExpiresAtUnix: expiresAt.Unix(),
		TokenType:     "Bearer",
		Principal: &cloudpb.ApiKeyPrincipal{
			OwnerKind: cloudpb.ApiKeyOwnerKind_API_KEY_OWNER_KIND_DEVICE,
			DeviceId:  deviceID,
		},
	}, nil
}

type deviceInitService struct {
	cloudpb.UnimplementedDeviceInitServiceServer
	identity *deviceIdentityService
}

func (s *deviceInitService) InitDevice(ctx context.Context, req *cloudpb.InitDeviceRequest) (*cloudpb.InitDeviceResponse, error) {
	deviceName := strings.TrimSpace(req.GetDeviceName())
	token := strings.TrimSpace(req.GetDeviceAuthToken())
	if deviceName == "" || token == "" {
		return nil, status.Error(codes.InvalidArgument, "device_name and device_auth_token are required")
	}
	principal, tokenID, err := s.identity.authorizeSDKInit(ctx, deviceName, token)
	if err != nil {
		return nil, err
	}
	apiKey, err := s.identity.provisionAPIKey(ctx, principal)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "device initialization unavailable")
	}
	result, err := s.identity.db.ExecContext(ctx, `
		UPDATE ws_client_auth_tokens
		SET sdk_initialized_at = ?
		WHERE id = ? AND revoked_at IS NULL AND sdk_initialized_at IS NULL
	`, s.identity.now(), tokenID)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "device initialization unavailable")
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return nil, status.Error(codes.Aborted, "device initialization permission was already consumed")
	}
	return initDeviceResponse(principal, apiKey, req.GetPlatform()), nil
}

func (s *deviceInitService) ReinitDevice(ctx context.Context, req *cloudpb.ReinitDeviceRequest) (*cloudpb.InitDeviceResponse, error) {
	deviceID := strings.TrimSpace(req.GetDeviceId())
	if deviceID == "" {
		return nil, status.Error(codes.InvalidArgument, "device_id is required")
	}
	if _, err := parseHilbertDeviceID(deviceID); err != nil {
		return nil, status.Error(codes.InvalidArgument, "device_id must be a positive decimal integer")
	}
	if recoveryToken := strings.TrimSpace(req.GetDeviceAuthToken()); recoveryToken != "" {
		return s.recoverDevice(ctx, req, recoveryToken)
	}
	principal, err := authenticateDeviceContext(ctx, s.identity.auth)
	if err != nil {
		return nil, err
	}
	if principal.DeviceID != deviceID {
		return nil, status.Error(codes.PermissionDenied, "device identity mismatch")
	}
	if err := s.identity.incrementAuthEpoch(ctx, principal.RobotID); err != nil {
		return nil, status.Error(codes.Unavailable, "device reset unavailable")
	}
	principal.AuthEpoch++
	apiKey, err := s.identity.resetHilbertAPIKey(ctx, principal)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "device reset incomplete; administrator recovery is required")
	}
	return initDeviceResponse(principal, apiKey, req.GetPlatform()), nil
}

func (s *deviceInitService) recoverDevice(ctx context.Context, req *cloudpb.ReinitDeviceRequest, token string) (*cloudpb.InitDeviceResponse, error) {
	principal, tokenID, stage, err := s.identity.authorizeRecovery(ctx, req.GetDeviceId(), token)
	if err != nil {
		return nil, err
	}
	hilbertDeviceID, err := parseHilbertDeviceID(principal.DeviceID)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "invalid projected device identity")
	}
	if stage == "authorized" {
		if err := s.identity.incrementAuthEpoch(ctx, principal.RobotID); err != nil {
			return nil, status.Error(codes.Unavailable, "device recovery unavailable")
		}
		principal.AuthEpoch++
		if err := s.identity.setRecoveryStage(ctx, tokenID, "epoch_incremented", false); err != nil {
			return nil, status.Error(codes.Unavailable, "device recovery unavailable")
		}
		stage = "epoch_incremented"
	}
	if stage == "epoch_incremented" {
		if err := s.identity.hilbert.DeleteDCDeviceAPIKey(ctx, principal.WorkspaceID, hilbertDeviceID); err != nil {
			return nil, status.Error(codes.Unavailable, "device recovery delete incomplete")
		}
		if err := s.identity.setRecoveryStage(ctx, tokenID, "deleted", false); err != nil {
			return nil, status.Error(codes.Unavailable, "device recovery unavailable")
		}
		stage = "deleted"
	}
	var apiKey string
	if stage == "deleted" {
		apiKey, err = s.identity.hilbert.GenerateDCDeviceAPIKey(ctx, principal.WorkspaceID, hilbertDeviceID)
		if err != nil {
			apiKey, err = s.identity.hilbert.GetDCDeviceAPIKey(ctx, principal.WorkspaceID, hilbertDeviceID)
		}
		if err != nil {
			return nil, status.Error(codes.Unavailable, "device recovery generate incomplete")
		}
		if err := s.identity.setRecoveryStage(ctx, tokenID, "generated", false); err != nil {
			return nil, status.Error(codes.Unavailable, "device recovery unavailable")
		}
		stage = "generated"
	}
	if stage == "generated" || stage == "completed" {
		if apiKey == "" {
			apiKey, err = s.identity.hilbert.GetDCDeviceAPIKey(ctx, principal.WorkspaceID, hilbertDeviceID)
			if err != nil {
				return nil, status.Error(codes.Unavailable, "device recovery lookup incomplete")
			}
		}
		if stage != "completed" {
			if err := s.identity.setRecoveryStage(ctx, tokenID, "completed", true); err != nil {
				return nil, status.Error(codes.Unavailable, "device recovery unavailable")
			}
		}
		return initDeviceResponse(principal, apiKey, req.GetPlatform()), nil
	}
	return nil, status.Error(codes.FailedPrecondition, "invalid device recovery stage")
}

func (s *deviceIdentityService) lookupActiveDevice(ctx context.Context, deviceID string) (devicePrincipal, error) {
	var principal devicePrincipal
	if err := s.db.GetContext(ctx, &principal, `
		SELECT id AS robot_id, device_id, workspace_id, auth_epoch
		FROM robots
		WHERE device_id = ? AND status = 'active' AND deleted_at IS NULL
		LIMIT 1
	`, deviceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return devicePrincipal{}, status.Error(codes.NotFound, "device not found")
		}
		return devicePrincipal{}, status.Error(codes.Unavailable, "device lookup unavailable")
	}
	return principal, nil
}

func (s *deviceIdentityService) authorizeSDKInit(ctx context.Context, deviceName, token string) (devicePrincipal, int64, error) {
	var row struct {
		TokenID  int64          `db:"token_id"`
		Metadata sql.NullString `db:"metadata"`
		devicePrincipal
	}
	if err := s.db.GetContext(ctx, &row, `
		SELECT t.id AS token_id, r.id AS robot_id, r.device_id, r.workspace_id, r.auth_epoch, r.metadata
		FROM ws_client_auth_tokens t
		INNER JOIN robots r ON r.id = t.robot_id
		WHERE t.token_hash = ? AND t.revoked_at IS NULL
			AND t.sdk_initialized_at IS NULL AND r.status = 'active' AND r.deleted_at IS NULL
		LIMIT 1
	`, deviceauth.HashPersistentToken(token)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return devicePrincipal{}, 0, status.Error(codes.Unauthenticated, "invalid or consumed device auth token")
		}
		return devicePrincipal{}, 0, status.Error(codes.Unavailable, "device initialization unavailable")
	}
	if deviceNameFromRobotMetadata(row.Metadata) != strings.TrimSpace(deviceName) {
		return devicePrincipal{}, 0, status.Error(codes.Unauthenticated, "device name does not match device auth token")
	}
	return row.devicePrincipal, row.TokenID, nil
}

func deviceNameFromRobotMetadata(ns sql.NullString) string {
	if !ns.Valid || strings.TrimSpace(ns.String) == "" {
		return ""
	}
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(ns.String), &payload); err != nil {
		return ""
	}
	name, ok := payload["hilbert_dc_device_name"].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(name)
}

func (s *deviceIdentityService) authorizeRecovery(ctx context.Context, deviceID, token string) (devicePrincipal, int64, string, error) {
	var row struct {
		TokenID       int64  `db:"token_id"`
		RecoveryStage string `db:"recovery_stage"`
		devicePrincipal
	}
	if err := s.db.GetContext(ctx, &row, `
		SELECT t.id AS token_id, t.recovery_stage, r.id AS robot_id, r.device_id, r.workspace_id, r.auth_epoch
		FROM ws_client_auth_tokens t
		INNER JOIN robots r ON r.id = t.robot_id
		WHERE r.device_id = ? AND t.token_hash = ? AND t.revoked_at IS NULL
			AND t.recovery_requested_at IS NOT NULL AND r.status = 'active' AND r.deleted_at IS NULL
		LIMIT 1
	`, strings.TrimSpace(deviceID), deviceauth.HashPersistentToken(token)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return devicePrincipal{}, 0, "", status.Error(codes.Unauthenticated, "device recovery is not authorized")
		}
		return devicePrincipal{}, 0, "", status.Error(codes.Unavailable, "device recovery unavailable")
	}
	return row.devicePrincipal, row.TokenID, row.RecoveryStage, nil
}

func (s *deviceIdentityService) incrementAuthEpoch(ctx context.Context, robotID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE robots SET auth_epoch = auth_epoch + 1, updated_at = ? WHERE id = ? AND deleted_at IS NULL`, s.now(), robotID)
	return err
}

func (s *deviceIdentityService) setRecoveryStage(ctx context.Context, tokenID int64, stage string, completed bool) error {
	var completedAt sql.NullTime
	if completed {
		completedAt = sql.NullTime{Time: s.now(), Valid: true}
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE ws_client_auth_tokens
		SET recovery_stage = ?, recovery_completed_at = COALESCE(?, recovery_completed_at)
		WHERE id = ? AND revoked_at IS NULL
	`, stage, completedAt, tokenID)
	return err
}

func (s *deviceIdentityService) resetHilbertAPIKey(ctx context.Context, principal devicePrincipal) (string, error) {
	hilbertDeviceID, err := parseHilbertDeviceID(principal.DeviceID)
	if err != nil {
		return "", err
	}
	if err := s.hilbert.DeleteDCDeviceAPIKey(ctx, principal.WorkspaceID, hilbertDeviceID); err != nil {
		return "", err
	}
	apiKey, err := s.hilbert.GenerateDCDeviceAPIKey(ctx, principal.WorkspaceID, hilbertDeviceID)
	if err == nil {
		return apiKey, nil
	}
	return s.hilbert.GetDCDeviceAPIKey(ctx, principal.WorkspaceID, hilbertDeviceID)
}

func initDeviceResponse(principal devicePrincipal, apiKey, platform string) *cloudpb.InitDeviceResponse {
	tags := map[string]string{
		"device_id":    principal.DeviceID,
		"workspace_id": fmt.Sprintf("%d", principal.WorkspaceID),
		"source":       "hilbert",
	}
	if platform = strings.TrimSpace(platform); platform != "" {
		tags["platform"] = platform
	}
	return &cloudpb.InitDeviceResponse{ApiKey: apiKey, Tags: tags}
}
