// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package cloud

import (
	"context"
	"fmt"
	"sync"
	"time"

	pb "archebase.com/keystone-edge/internal/cloud/cloudpb"
	"archebase.com/keystone-edge/internal/logger"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// GatewayClientConfig defines the runtime configuration for the data-gateway client.
type GatewayClientConfig struct {
	// Endpoint is the gRPC address of the DataGatewayService.
	Endpoint string
	// RequestTimeout is the per-RPC deadline.
	RequestTimeout time.Duration
}

// UploadSession tracks the active upload session state returned by the data-gateway service.
type UploadSession struct {
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

// CreateFileUpload calls DataGatewayService.CreateFileUpload to obtain an upload session
// with STS credentials and target object key.
func (c *GatewayClient) CreateFileUpload(ctx context.Context, clientHints map[string]string) (*UploadSession, error) {
	conn, err := c.connect()
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
	defer cancel()

	ctx, err = c.attachAuth(ctx)
	if err != nil {
		return nil, err
	}

	client := pb.NewDataGatewayServiceClient(conn)
	resp, err := client.CreateFileUpload(ctx, &pb.CreateFileUploadRequest{
		ClientHints: clientHints,
	})
	if err != nil {
		return nil, fmt.Errorf("CreateFileUpload RPC: %w", err)
	}

	return c.sessionFromCredentials(resp.UploadId, resp.Credentials)
}

// RefreshUploadCredentials refreshes STS credentials for an existing upload session.
func (c *GatewayClient) RefreshUploadCredentials(ctx context.Context, uploadID string) (*UploadSession, error) {
	conn, err := c.connect()
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
	defer cancel()

	ctx, err = c.attachAuth(ctx)
	if err != nil {
		return nil, err
	}

	client := pb.NewDataGatewayServiceClient(conn)
	resp, err := client.RefreshUploadCredentials(ctx, &pb.RefreshUploadCredentialsRequest{
		UploadId: uploadID,
	})
	if err != nil {
		return nil, fmt.Errorf("RefreshUploadCredentials RPC: %w", err)
	}

	return c.sessionFromCredentials(resp.UploadId, resp.Credentials)
}

// CompleteUpload notifies the data-gateway that all parts have been uploaded to OSS.
func (c *GatewayClient) CompleteUpload(ctx context.Context, uploadID string, fileSize int64, rawTags map[string]string, completedPartCount int32, ossObjectEtag string) error {
	conn, err := c.connect()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
	defer cancel()

	ctx, err = c.attachAuth(ctx)
	if err != nil {
		return err
	}

	client := pb.NewDataGatewayServiceClient(conn)
	_, err = client.CompleteUpload(ctx, &pb.CompleteUploadRequest{
		UploadId:           uploadID,
		FileSize:           fileSize,
		RawTags:            rawTags,
		CompletedPartCount: completedPartCount,
		OssObjectEtag:      ossObjectEtag,
	})
	if err != nil {
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

	conn, err := grpc.NewClient(c.cfg.Endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
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

func (c *GatewayClient) attachAuth(ctx context.Context) (context.Context, error) {
	header, err := c.authClient.GetAuthHeader(ctx)
	if err != nil {
		return ctx, fmt.Errorf("get auth header: %w", err)
	}
	md := metadata.Pairs("authorization", header)
	return metadata.NewOutgoingContext(ctx, md), nil
}

func (c *GatewayClient) sessionFromCredentials(uploadID string, creds *pb.UploadCredentials) (*UploadSession, error) {
	if creds == nil {
		return nil, fmt.Errorf("missing upload credentials in response")
	}
	return &UploadSession{
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
