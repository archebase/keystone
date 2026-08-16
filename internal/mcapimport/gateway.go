// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package mcapimport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"archebase.com/keystone-edge/internal/cloud/cloudpb"
)

// GatewayClient implements the import control plane over gRPC.
type GatewayClient struct {
	conn       *grpc.ClientConn
	rpcTimeout time.Duration
}

// NewGatewayClient creates a Data Gateway client. The returned gRPC connection is lazy.
func NewGatewayClient(cfg Config) (*GatewayClient, error) {
	transportCredentials, err := gatewayTransportCredentials(cfg)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(cfg.Endpoint, grpc.WithTransportCredentials(transportCredentials))
	if err != nil {
		return nil, fmt.Errorf("create Data Gateway client: %w", err)
	}
	return &GatewayClient{conn: conn, rpcTimeout: cfg.RPCTimeout}, nil
}

// Close releases the gRPC connection.
func (c *GatewayClient) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// InitDevice exchanges a one-time device auth token for an upload credential.
func (c *GatewayClient) InitDevice(ctx context.Context, deviceName, authToken string) (DeviceCredential, int64, error) {
	rpcCtx, cancel := c.rpcContext(ctx)
	defer cancel()
	response, err := cloudpb.NewDeviceInitServiceClient(c.conn).InitDevice(rpcCtx, &cloudpb.InitDeviceRequest{
		SdkVersion:      "keystone-import-mcap/1",
		Platform:        "linux",
		DeviceAuthToken: strings.TrimSpace(authToken),
		DeviceName:      strings.TrimSpace(deviceName),
	})
	if err != nil {
		return DeviceCredential{}, 0, fmt.Errorf("DeviceInitService.InitDevice: %w", err)
	}
	deviceID := strings.TrimSpace(response.GetTags()["device_id"])
	workspaceText := strings.TrimSpace(response.GetTags()["workspace_id"])
	workspaceID, err := strconv.ParseInt(workspaceText, 10, 64)
	if err != nil || workspaceID <= 0 {
		return DeviceCredential{}, 0, fmt.Errorf("device initialization returned invalid workspace_id")
	}
	credential := DeviceCredential{
		DeviceID: deviceID,
		Secret:   strings.TrimSpace(response.GetApiKey()),
	}
	if credential.DeviceID == "" || credential.Secret == "" {
		return DeviceCredential{}, 0, fmt.Errorf("device initialization returned incomplete credentials")
	}
	return credential, workspaceID, nil
}

// CreateLogicalUpload obtains a TOS destination and temporary credentials from Keystone.
func (c *GatewayClient) CreateLogicalUpload(ctx context.Context, credential DeviceCredential, clientHints map[string]string) (UploadSession, error) {
	authCtx, cancel, err := c.authenticatedContext(ctx, credential)
	if err != nil {
		return UploadSession{}, err
	}
	defer cancel()
	request := &cloudpb.CreateLogicalUploadRequest{ClientHints: clientHints}
	if strings.TrimSpace(clientHints["task_id"]) == "" && strings.EqualFold(strings.TrimSpace(clientHints["auto_assign_task"]), "true") {
		planID, parseErr := strconv.ParseInt(strings.TrimSpace(clientHints["dc_plan_id"]), 10, 64)
		if parseErr != nil || planID <= 0 {
			return UploadSession{}, fmt.Errorf("auto-assigned upload requires a positive dc_plan_id")
		}
		request.AutoAssignTask = true
		request.DcPlanId = planID
	}
	response, err := cloudpb.NewDataGatewayServiceClient(c.conn).CreateLogicalUpload(authCtx, request)
	if err != nil {
		return UploadSession{}, fmt.Errorf("DataGatewayService.CreateLogicalUpload: %w", err)
	}
	issued := response.GetCredentials()
	if issued == nil {
		return UploadSession{}, fmt.Errorf("data gateway returned no upload credentials")
	}
	if backend := strings.TrimSpace(issued.GetObjectStoreBackend()); backend != "volcengine_tos" {
		return UploadSession{}, fmt.Errorf("unsupported object-store backend %q", backend)
	}
	return UploadSession{
		LogicalUploadID: strings.TrimSpace(response.GetLogicalUploadId()),
		UploadID:        strings.TrimSpace(response.GetUploadId()),
		TaskID:          strings.TrimSpace(response.GetResolvedTaskId()),
		DCPlanID:        response.GetResolvedDcPlanId(),
		WorkspaceID:     response.GetResolvedWorkspaceId(),
		Bucket:          strings.TrimSpace(issued.GetBucket()),
		Endpoint:        strings.TrimSpace(issued.GetEndpoint()),
		Region:          strings.TrimSpace(issued.GetObjectStoreRegion()),
		ObjectKey:       strings.TrimSpace(issued.GetObjectKey()),
		AccessKeyID:     strings.TrimSpace(issued.GetStsAccessKeyId()),
		AccessKeySecret: strings.TrimSpace(issued.GetStsAccessKeySecret()),
		SecurityToken:   strings.TrimSpace(issued.GetStsSecurityToken()),
		PartSizeBytes:   issued.GetPartSizeBytes(),
	}, nil
}

// CompleteUpload asks Keystone to persist the Episode and enqueue automatic QA.
func (c *GatewayClient) CompleteUpload(ctx context.Context, credential DeviceCredential, req CompleteRequest) error {
	err := c.completeUploadOnce(ctx, credential, req)
	if status.Code(err) != codes.Unavailable && status.Code(err) != codes.DeadlineExceeded {
		return err
	}
	// Completion is idempotent. One retry handles a lost response without creating another Episode.
	return c.completeUploadOnce(ctx, credential, req)
}

func (c *GatewayClient) completeUploadOnce(ctx context.Context, credential DeviceCredential, req CompleteRequest) error {
	authCtx, cancel, err := c.authenticatedContext(ctx, credential)
	if err != nil {
		return err
	}
	defer cancel()
	_, err = cloudpb.NewDataGatewayServiceClient(c.conn).CompleteUpload(authCtx, &cloudpb.CompleteUploadRequest{
		UploadId:           req.UploadID,
		FileSize:           req.FileSize,
		RawTags:            req.RawTags,
		CompletedPartCount: req.CompletedPartCount,
		ObjectEtag:         req.ObjectETag,
		PartSizeBytes:      req.PartSizeBytes,
	})
	if err != nil {
		return fmt.Errorf("DataGatewayService.CompleteUpload: %w", err)
	}
	return nil
}

// GetUploadRecovery reads the server-side completion state after an ambiguous completion response.
func (c *GatewayClient) GetUploadRecovery(ctx context.Context, credential DeviceCredential, logicalUploadID string) (UploadRecovery, error) {
	authCtx, cancel, err := c.authenticatedContext(ctx, credential)
	if err != nil {
		return UploadRecovery{}, err
	}
	defer cancel()
	response, err := cloudpb.NewDataGatewayServiceClient(c.conn).GetUploadRecovery(authCtx, &cloudpb.GetUploadRecoveryRequest{
		LogicalUploadId: logicalUploadID,
	})
	if err != nil {
		return UploadRecovery{}, fmt.Errorf("DataGatewayService.GetUploadRecovery: %w", err)
	}
	return UploadRecovery{
		Completed: response.GetLogicalUploadStatus() == cloudpb.LogicalUploadStatus_LOGICAL_UPLOAD_STATUS_COMPLETED,
		ETag:      strings.TrimSpace(response.GetObjectEtag()),
		PartCount: response.GetCompletedPartCount(),
	}, nil
}

// AbortUpload marks an unsuccessful logical upload as aborted.
func (c *GatewayClient) AbortUpload(ctx context.Context, credential DeviceCredential, logicalUploadID, reason string) error {
	authCtx, cancel, err := c.authenticatedContext(ctx, credential)
	if err != nil {
		return err
	}
	defer cancel()
	_, err = cloudpb.NewDataGatewayServiceClient(c.conn).AbortUpload(authCtx, &cloudpb.AbortUploadRequest{
		LogicalUploadId: logicalUploadID,
		Reason:          reason,
	})
	if err != nil {
		return fmt.Errorf("DataGatewayService.AbortUpload: %w", err)
	}
	return nil
}

func (c *GatewayClient) authenticatedContext(ctx context.Context, credential DeviceCredential) (context.Context, context.CancelFunc, error) {
	tokenCtx, tokenCancel := c.rpcContext(ctx)
	response, err := cloudpb.NewAuthServiceClient(c.conn).ExchangeCredential(tokenCtx, &cloudpb.ExchangeCredentialRequest{
		Credential: credential.Secret,
		DeviceId:   credential.DeviceID,
	})
	tokenCancel()
	if err != nil {
		return nil, nil, fmt.Errorf("AuthService.ExchangeCredential: %w", err)
	}
	token := strings.TrimSpace(response.GetAccessToken())
	if token == "" {
		return nil, nil, fmt.Errorf("AuthService.ExchangeCredential returned no access token")
	}
	rpcCtx, cancel := c.rpcContext(ctx)
	rpcCtx = metadata.NewOutgoingContext(rpcCtx, metadata.Pairs("authorization", "Bearer "+token))
	return rpcCtx, cancel, nil
}

func (c *GatewayClient) rpcContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, c.rpcTimeout)
}

func gatewayTransportCredentials(cfg Config) (credentials.TransportCredentials, error) {
	if !cfg.UseTLS {
		return insecure.NewCredentials(), nil
	}
	var roots *x509.CertPool
	if strings.TrimSpace(cfg.TLSCAFile) == "" {
		pool, err := x509.SystemCertPool()
		if err != nil {
			return nil, fmt.Errorf("load system CA pool: %w", err)
		}
		roots = pool
	} else {
		// #nosec G304 -- CA path is explicitly supplied by the CLI operator.
		pemData, err := os.ReadFile(cfg.TLSCAFile)
		if err != nil {
			return nil, fmt.Errorf("read TLS CA file: %w", err)
		}
		roots = x509.NewCertPool()
		if ok := roots.AppendCertsFromPEM(pemData); !ok {
			return nil, fmt.Errorf("TLS CA file contains no certificates")
		}
	}
	return credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    roots,
		ServerName: strings.TrimSpace(cfg.TLSServerName),
	}), nil
}
