// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package cloud

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	pb "archebase.com/keystone-edge/internal/cloud/cloudpb"
	"archebase.com/keystone-edge/internal/logger"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// ErrLogicalUploadNotFound is returned by GetUploadRecovery when the server reports that the
// logical upload session no longer exists (gRPC NOT_FOUND). Callers should treat this as a
// permanent failure and clean up any local persisted state.
var ErrLogicalUploadNotFound = errors.New("logical upload not found on server")

// GatewayClientConfig defines the runtime configuration for the data-gateway client.
type GatewayClientConfig struct {
	// Endpoint is the gRPC address of the DataGatewayService.
	Endpoint string
	// UseTLS enables TLS for the gRPC connection.
	UseTLS bool
	// TLSCAFile is an optional CA bundle path for TLS verification.
	TLSCAFile string
	// TLSServerName is an optional TLS server name override (SNI / verification).
	TLSServerName string
	// RequestTimeout is the per-RPC deadline.
	RequestTimeout time.Duration
}

// UploadSession tracks the active upload session state returned by the data-gateway service.
type UploadSession struct {
	// LogicalUploadID is the stable logical identifier that persists across restarts.
	LogicalUploadID    string
	UploadID           string
	Bucket             string
	Endpoint           string
	ObjectKey          string
	STSAccessKeyID     string
	STSAccessKeySecret string
	STSSecurityToken   string
	STSExpireAt        time.Time
	PartSizeBytes      int64
}

// UploadRecoveryInfo holds the recovery state returned by GetUploadRecovery.
type UploadRecoveryInfo struct {
	LogicalUploadID    string
	CurrentUploadID    string
	NextAction         pb.UploadRecoveryAction
	OSSObjectETag      string
	CompletedPartCount int32
}

// GatewayClient provides a high-level gRPC client for the DataGatewayService.
type GatewayClient struct {
	cfg        GatewayClientConfig
	authClient *AuthClient
	connMu     sync.Mutex
	conn       *grpc.ClientConn
}

// NewGatewayClient creates a new gateway client.
func NewGatewayClient(cfg GatewayClientConfig, authClient *AuthClient) *GatewayClient {
	return &GatewayClient{cfg: cfg, authClient: authClient}
}

// CreateLogicalUpload calls DataGatewayService.CreateLogicalUpload to create a new logical upload
// session (or restart an existing one) and obtain STS credentials and the target object key.
func (c *GatewayClient) CreateLogicalUpload(ctx context.Context, clientHints map[string]string, restartFromUploadID string) (*UploadSession, error) {
	conn, err := c.connect()
	if err != nil {
		return nil, err
	}

	// Auth and RPC each get their own independent deadline rooted at the caller's ctx.
	// Auth deadline is separate so token acquisition (TCP+TLS dial + ExchangeCredential RPC)
	// does not consume the budget intended for the gateway RPC itself.
	// The auth header string is extracted before authCancel(), then injected directly into
	// rpcCtx — which is derived from the original ctx, not from authCtx — so cancelling
	// authCtx never propagates into the RPC call.
	authCtx, authCancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
	authHeader, err := c.authClient.GetAuthHeader(authCtx)
	authCancel()
	if err != nil {
		return nil, fmt.Errorf("get auth header: %w", err)
	}

	rpcCtx, rpcCancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
	defer rpcCancel()
	rpcCtx = metadata.NewOutgoingContext(rpcCtx, metadata.Pairs("authorization", authHeader))

	client := pb.NewDataGatewayServiceClient(conn)
	resp, err := client.CreateLogicalUpload(rpcCtx, &pb.CreateLogicalUploadRequest{
		ClientHints:         clientHints,
		RestartFromUploadId: restartFromUploadID,
	})
	if err != nil {
		logger.Printf("[CLOUD-GATEWAY] CreateLogicalUpload RPC failed, resetting gRPC connection: %v", err)
		if closeErr := c.Close(); closeErr != nil {
			logger.Printf("[CLOUD-GATEWAY] failed to reset gRPC connection after CreateLogicalUpload error: %v", closeErr)
		} else {
			logger.Printf("[CLOUD-GATEWAY] gRPC connection reset after CreateLogicalUpload error")
		}
		return nil, fmt.Errorf("CreateLogicalUpload RPC: %w", err)
	}

	return c.sessionFromCreateResponse(resp.LogicalUploadId, resp.UploadId, resp.Credentials)
}

// GetUploadRecovery calls DataGatewayService.GetUploadRecovery to determine the recovery action
// for an existing logical upload (e.g. after a process restart).
func (c *GatewayClient) GetUploadRecovery(ctx context.Context, logicalUploadID string) (*UploadRecoveryInfo, error) {
	conn, err := c.connect()
	if err != nil {
		return nil, err
	}

	authCtx, authCancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
	authHeader, err := c.authClient.GetAuthHeader(authCtx)
	authCancel()
	if err != nil {
		return nil, fmt.Errorf("get auth header: %w", err)
	}

	rpcCtx, rpcCancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
	defer rpcCancel()
	rpcCtx = metadata.NewOutgoingContext(rpcCtx, metadata.Pairs("authorization", authHeader))

	client := pb.NewDataGatewayServiceClient(conn)
	resp, err := client.GetUploadRecovery(rpcCtx, &pb.GetUploadRecoveryRequest{
		LogicalUploadId: logicalUploadID,
	})
	if err != nil {
		// NOT_FOUND means the logical upload session no longer exists on the server.
		// Return the sentinel directly — no connection reset needed, this is not a
		// transient error and the caller must treat it as permanent failure.
		if st, ok := status.FromError(err); ok && st.Code() == codes.NotFound {
			return nil, ErrLogicalUploadNotFound
		}
		logger.Printf("[CLOUD-GATEWAY] GetUploadRecovery RPC failed, resetting gRPC connection: %v", err)
		if closeErr := c.Close(); closeErr != nil {
			logger.Printf("[CLOUD-GATEWAY] failed to reset gRPC connection after GetUploadRecovery error: %v", closeErr)
		} else {
			logger.Printf("[CLOUD-GATEWAY] gRPC connection reset after GetUploadRecovery error")
		}
		return nil, fmt.Errorf("GetUploadRecovery RPC: %w", err)
	}

	return &UploadRecoveryInfo{
		LogicalUploadID:    resp.LogicalUploadId,
		CurrentUploadID:    resp.CurrentUploadId,
		NextAction:         resp.NextAction,
		OSSObjectETag:      resp.OssObjectEtag,
		CompletedPartCount: resp.CompletedPartCount,
	}, nil
}

// ReissueUploadCredentials refreshes STS credentials for an existing upload session.
func (c *GatewayClient) ReissueUploadCredentials(ctx context.Context, uploadID string) (*UploadSession, error) {
	conn, err := c.connect()
	if err != nil {
		return nil, err
	}

	authCtx, authCancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
	authHeader, err := c.authClient.GetAuthHeader(authCtx)
	authCancel()
	if err != nil {
		return nil, fmt.Errorf("get auth header: %w", err)
	}

	rpcCtx, rpcCancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
	defer rpcCancel()
	rpcCtx = metadata.NewOutgoingContext(rpcCtx, metadata.Pairs("authorization", authHeader))

	client := pb.NewDataGatewayServiceClient(conn)
	resp, err := client.ReissueUploadCredentials(rpcCtx, &pb.ReissueUploadCredentialsRequest{
		UploadId: uploadID,
	})
	if err != nil {
		logger.Printf("[CLOUD-GATEWAY] ReissueUploadCredentials RPC failed, resetting gRPC connection: %v", err)
		if closeErr := c.Close(); closeErr != nil {
			logger.Printf("[CLOUD-GATEWAY] failed to reset gRPC connection after ReissueUploadCredentials error: %v", closeErr)
		} else {
			logger.Printf("[CLOUD-GATEWAY] gRPC connection reset after ReissueUploadCredentials error")
		}
		return nil, fmt.Errorf("ReissueUploadCredentials RPC: %w", err)
	}

	return c.sessionFromCreateResponse(resp.LogicalUploadId, resp.UploadId, resp.Credentials)
}

// AbortUpload cancels a logical upload session on the data-gateway.
func (c *GatewayClient) AbortUpload(ctx context.Context, logicalUploadID string, reason string) error {
	conn, err := c.connect()
	if err != nil {
		return err
	}

	authCtx, authCancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
	authHeader, err := c.authClient.GetAuthHeader(authCtx)
	authCancel()
	if err != nil {
		return err
	}

	rpcCtx, rpcCancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
	defer rpcCancel()
	rpcCtx = metadata.NewOutgoingContext(rpcCtx, metadata.Pairs("authorization", authHeader))

	client := pb.NewDataGatewayServiceClient(conn)
	_, err = client.AbortUpload(rpcCtx, &pb.AbortUploadRequest{
		LogicalUploadId: logicalUploadID,
		Reason:          reason,
	})
	if err != nil {
		// AbortUpload is best-effort: do not reset the shared gRPC connection on failure,
		// as that would disrupt subsequent normal uploads. Only log the error.
		logger.Printf("[CLOUD-GATEWAY] AbortUpload RPC failed: %v", err)
		return fmt.Errorf("AbortUpload RPC: %w", err)
	}
	return nil
}

// CompleteUpload notifies the data-gateway that all parts have been uploaded to OSS.
func (c *GatewayClient) CompleteUpload(ctx context.Context, uploadID string, fileSize int64, rawTags map[string]string, completedPartCount int32, ossObjectEtag string) error {
	conn, err := c.connect()
	if err != nil {
		return err
	}

	authCtx, authCancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
	authHeader, err := c.authClient.GetAuthHeader(authCtx)
	authCancel()
	if err != nil {
		return err
	}

	rpcCtx, rpcCancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
	defer rpcCancel()
	rpcCtx = metadata.NewOutgoingContext(rpcCtx, metadata.Pairs("authorization", authHeader))

	client := pb.NewDataGatewayServiceClient(conn)
	_, err = client.CompleteUpload(rpcCtx, &pb.CompleteUploadRequest{
		UploadId:           uploadID,
		FileSize:           fileSize,
		RawTags:            rawTags,
		CompletedPartCount: completedPartCount,
		OssObjectEtag:      ossObjectEtag,
	})
	if err != nil {
		logger.Printf("[CLOUD-GATEWAY] CompleteUpload RPC failed, resetting gRPC connection: %v", err)
		if closeErr := c.Close(); closeErr != nil {
			logger.Printf("[CLOUD-GATEWAY] failed to reset gRPC connection after CompleteUpload error: %v", closeErr)
		} else {
			logger.Printf("[CLOUD-GATEWAY] gRPC connection reset after CompleteUpload error")
		}
		return fmt.Errorf("CompleteUpload RPC: %w", err)
	}
	return nil
}

func (c *GatewayClient) connect() (*grpc.ClientConn, error) {
	c.connMu.Lock()
	defer c.connMu.Unlock()

	if c.conn != nil {
		return c.conn, nil
	}

	creds, err := newCloudTransportCredentials(c.cfg.UseTLS, c.cfg.TLSCAFile, c.cfg.TLSServerName)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(c.cfg.Endpoint, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("grpc dial %s: %w", c.cfg.Endpoint, err)
	}
	c.conn = conn
	return conn, nil
}

// Close releases the shared gRPC connection.
func (c *GatewayClient) Close() error {
	c.connMu.Lock()
	defer c.connMu.Unlock()

	if c.conn == nil {
		return nil
	}

	err := c.conn.Close()
	if err != nil {
		logger.Printf("[CLOUD-GATEWAY] gRPC close error: %v", err)
	}
	c.conn = nil
	return err
}

func (c *GatewayClient) sessionFromCreateResponse(logicalUploadID, uploadID string, creds *pb.UploadCredentials) (*UploadSession, error) {
	if creds == nil {
		return nil, fmt.Errorf("missing upload credentials in response")
	}
	return &UploadSession{
		LogicalUploadID:    logicalUploadID,
		UploadID:           uploadID,
		Bucket:             creds.Bucket,
		Endpoint:           creds.Endpoint,
		ObjectKey:          creds.ObjectKey,
		STSAccessKeyID:     creds.StsAccessKeyId,
		STSAccessKeySecret: creds.StsAccessKeySecret,
		STSSecurityToken:   creds.StsSecurityToken,
		STSExpireAt:        time.Unix(creds.StsExpireAtUnix, 0),
		PartSizeBytes:      creds.PartSizeBytes,
	}, nil
}
