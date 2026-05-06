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
	authHeader, err := c.getAuthHeader(ctx)
	if err != nil {
		return nil, err
	}

	var resp *pb.CreateLogicalUploadResponse
	err = c.doRPC(ctx, "CreateLogicalUpload", func(conn *grpc.ClientConn) error {
		rpcCtx, rpcCancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
		defer rpcCancel()
		rpcCtx = metadata.NewOutgoingContext(rpcCtx, metadata.Pairs("authorization", authHeader))

		var rpcErr error
		resp, rpcErr = pb.NewDataGatewayServiceClient(conn).CreateLogicalUpload(rpcCtx, &pb.CreateLogicalUploadRequest{
			ClientHints:         clientHints,
			RestartFromUploadId: restartFromUploadID,
		})
		return rpcErr
	})
	if err != nil {
		return nil, fmt.Errorf("CreateLogicalUpload RPC: %w", err)
	}

	return c.sessionFromCreateResponse(resp.LogicalUploadId, resp.UploadId, resp.Credentials)
}

// GetUploadRecovery calls DataGatewayService.GetUploadRecovery to determine the recovery action
// for an existing logical upload (e.g. after a process restart).
func (c *GatewayClient) GetUploadRecovery(ctx context.Context, logicalUploadID string) (*UploadRecoveryInfo, error) {
	authHeader, err := c.getAuthHeader(ctx)
	if err != nil {
		return nil, err
	}

	var resp *pb.GetUploadRecoveryResponse
	err = c.doRPC(ctx, "GetUploadRecovery", func(conn *grpc.ClientConn) error {
		rpcCtx, rpcCancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
		defer rpcCancel()
		rpcCtx = metadata.NewOutgoingContext(rpcCtx, metadata.Pairs("authorization", authHeader))

		var rpcErr error
		resp, rpcErr = pb.NewDataGatewayServiceClient(conn).GetUploadRecovery(rpcCtx, &pb.GetUploadRecoveryRequest{
			LogicalUploadId: logicalUploadID,
		})
		// NOT_FOUND means the logical upload session no longer exists on the server.
		// Wrap as sentinel so doRPC does not reset the connection — this is not a
		// transient transport error but a permanent business-logic failure.
		if rpcErr != nil {
			if st, ok := status.FromError(rpcErr); ok && st.Code() == codes.NotFound {
				return ErrLogicalUploadNotFound
			}
		}
		return rpcErr
	})
	if err != nil {
		if errors.Is(err, ErrLogicalUploadNotFound) {
			return nil, ErrLogicalUploadNotFound
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
	authHeader, err := c.getAuthHeader(ctx)
	if err != nil {
		return nil, err
	}

	var resp *pb.ReissueUploadCredentialsResponse
	err = c.doRPC(ctx, "ReissueUploadCredentials", func(conn *grpc.ClientConn) error {
		rpcCtx, rpcCancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
		defer rpcCancel()
		rpcCtx = metadata.NewOutgoingContext(rpcCtx, metadata.Pairs("authorization", authHeader))

		var rpcErr error
		resp, rpcErr = pb.NewDataGatewayServiceClient(conn).ReissueUploadCredentials(rpcCtx, &pb.ReissueUploadCredentialsRequest{
			UploadId: uploadID,
		})
		return rpcErr
	})
	if err != nil {
		return nil, fmt.Errorf("ReissueUploadCredentials RPC: %w", err)
	}

	return c.sessionFromCreateResponse(resp.LogicalUploadId, resp.UploadId, resp.Credentials)
}

// AbortUpload cancels a logical upload session on the data-gateway.
// AbortUpload is best-effort: it does not reset the shared connection on failure.
func (c *GatewayClient) AbortUpload(ctx context.Context, logicalUploadID string, reason string) error {
	authHeader, err := c.getAuthHeader(ctx)
	if err != nil {
		return err
	}

	conn, err := c.connect()
	if err != nil {
		return err
	}

	rpcCtx, rpcCancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
	defer rpcCancel()
	rpcCtx = metadata.NewOutgoingContext(rpcCtx, metadata.Pairs("authorization", authHeader))

	_, err = pb.NewDataGatewayServiceClient(conn).AbortUpload(rpcCtx, &pb.AbortUploadRequest{
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
	authHeader, err := c.getAuthHeader(ctx)
	if err != nil {
		return err
	}

	return c.doRPC(ctx, "CompleteUpload", func(conn *grpc.ClientConn) error {
		rpcCtx, rpcCancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
		defer rpcCancel()
		rpcCtx = metadata.NewOutgoingContext(rpcCtx, metadata.Pairs("authorization", authHeader))

		_, rpcErr := pb.NewDataGatewayServiceClient(conn).CompleteUpload(rpcCtx, &pb.CompleteUploadRequest{
			UploadId:           uploadID,
			FileSize:           fileSize,
			RawTags:            rawTags,
			CompletedPartCount: completedPartCount,
			OssObjectEtag:      ossObjectEtag,
		})
		return rpcErr
	})
}

// doRPC executes fn with the current shared connection. If fn returns an error, the
// connection is reset and fn is retried once with a fresh connection. This transparently
// recovers from stale connections cut by NLB idle timeout without surfacing a transient
// failure to the caller.
//
// Sentinel errors (e.g. ErrLogicalUploadNotFound) are returned immediately without a
// retry or connection reset, because they represent permanent server-side failures rather
// than transport issues.
func (c *GatewayClient) doRPC(ctx context.Context, rpcName string, fn func(*grpc.ClientConn) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	conn, err := c.connect()
	if err != nil {
		return err
	}

	if err = fn(conn); err == nil {
		return nil
	}

	// Sentinel errors are permanent business failures — do not reset the connection.
	if errors.Is(err, ErrLogicalUploadNotFound) {
		return err
	}

	// First attempt failed. Reset the stale connection and retry once with a fresh one.
	logger.Printf("[CLOUD-GATEWAY] %s RPC failed, resetting connection and retrying: %v", rpcName, err)
	if closeErr := c.Close(); closeErr != nil {
		logger.Printf("[CLOUD-GATEWAY] failed to close stale connection: %v", closeErr)
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	conn, err = c.connect()
	if err != nil {
		return err
	}

	if err = fn(conn); err != nil {
		// Retry also failed — this is a genuine server-side error, not a stale connection.
		logger.Printf("[CLOUD-GATEWAY] %s RPC failed after reconnect: %v", rpcName, err)
		if closeErr := c.Close(); closeErr != nil {
			logger.Printf("[CLOUD-GATEWAY] failed to close connection after retry failure: %v", closeErr)
		}
		return err
	}

	logger.Printf("[CLOUD-GATEWAY] %s RPC succeeded after reconnect", rpcName)
	return nil
}

// getAuthHeader obtains a JWT auth header with its own deadline so token acquisition
// does not consume the budget of the gateway RPC itself.
func (c *GatewayClient) getAuthHeader(ctx context.Context) (string, error) {
	authCtx, authCancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
	defer authCancel()
	header, err := c.authClient.GetAuthHeader(authCtx)
	if err != nil {
		return "", fmt.Errorf("get auth header: %w", err)
	}
	return header, nil
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

	logger.Printf("[CLOUD-GATEWAY] connecting to %s", c.cfg.Endpoint)

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
