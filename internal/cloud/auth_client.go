// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package cloud provides cloud upload functionality for syncing approved
// episodes from edge MinIO to the data-platform cloud storage via gRPC
// control plane and Aliyun OSS data plane.
package cloud

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	pb "archebase.com/keystone-edge/internal/cloud/cloudpb"
	"archebase.com/keystone-edge/internal/logger"

	"google.golang.org/grpc"
)

// AuthClientConfig defines the runtime configuration for the auth client.
type AuthClientConfig struct {
	// Endpoint is the gRPC address of the AuthService (e.g. "cloud.example.com:50051").
	Endpoint string
	// UseTLS enables TLS for the gRPC connection.
	UseTLS bool
	// TLSCAFile is an optional CA bundle path for TLS verification.
	TLSCAFile string
	// TLSServerName is an optional TLS server name override (SNI / verification).
	TLSServerName string
	// SiteID is the numeric site identifier assigned to this edge deployment.
	SiteID int64
	// APISecret is the raw API key secret for credential exchange.
	APISecret string
	// RefreshBefore is how long before expiry to proactively refresh the token.
	RefreshBefore time.Duration
}

// AuthToken represents a cached JWT access token obtained from the AuthService.
type AuthToken struct {
	AccessToken string
	ExpiresAt   time.Time
}

// AuthClient provides credential exchange, caching and automatic refresh for
// JWT tokens from the data-platform AuthService.
type AuthClient struct {
	cfg    AuthClientConfig
	mu     sync.Mutex
	token  *AuthToken
	connMu sync.Mutex
	conn   *grpc.ClientConn
}

// NewAuthClient creates a new auth client.
func NewAuthClient(cfg AuthClientConfig) *AuthClient {
	return &AuthClient{cfg: cfg}
}

// GetToken returns the current token, refreshing it when necessary.
func (c *AuthClient) GetToken(ctx context.Context) (*AuthToken, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.token != nil && !c.shouldRefresh(c.token) {
		return c.token, nil
	}

	refreshed, err := c.refreshToken(ctx)
	if err != nil {
		return nil, err
	}
	c.token = refreshed
	return refreshed, nil
}

// GetAuthHeader returns the Authorization header value for gRPC/HTTP requests.
func (c *AuthClient) GetAuthHeader(ctx context.Context) (string, error) {
	token, err := c.GetToken(ctx)
	if err != nil {
		return "", err
	}
	return "Bearer " + token.AccessToken, nil
}

func (c *AuthClient) shouldRefresh(token *AuthToken) bool {
	remaining := time.Until(token.ExpiresAt)
	return remaining <= c.cfg.RefreshBefore
}

func (c *AuthClient) refreshToken(ctx context.Context) (*AuthToken, error) {
	credentialBase64 := c.buildCredentialBase64()
	if credentialBase64 == "" {
		return nil, fmt.Errorf("credential_base64 must not be empty")
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		resp, err := c.exchangeCredential(ctx, credentialBase64)
		if err != nil {
			lastErr = err
			logger.Printf("[CLOUD-AUTH] Credential exchange attempt %d failed: %v", attempt+1, err)
			continue
		}

		expiresAt := time.Unix(resp.ExpiresAtUnix, 0)
		return &AuthToken{
			AccessToken: resp.AccessToken,
			ExpiresAt:   expiresAt,
		}, nil
	}
	return nil, fmt.Errorf("auth token refresh failed after 3 attempts: %w", lastErr)
}

func (c *AuthClient) exchangeCredential(ctx context.Context, credentialBase64 string) (*pb.ExchangeCredentialResponse, error) {
	conn, err := c.getConn()
	if err != nil {
		return nil, err
	}

	client := pb.NewAuthServiceClient(conn)
	resp, err := client.ExchangeCredential(ctx, &pb.ExchangeCredentialRequest{
		CredentialBase64: credentialBase64,
	})
	if err != nil {
		logger.Printf("[CLOUD-AUTH] ExchangeCredential RPC failed, resetting gRPC connection: %v", err)
		if closeErr := c.Close(); closeErr != nil {
			logger.Printf("[CLOUD-AUTH] failed to reset gRPC connection after RPC error: %v", closeErr)
		} else {
			logger.Printf("[CLOUD-AUTH] gRPC connection reset after RPC error")
		}
		return nil, fmt.Errorf("exchange credential RPC: %w", err)
	}
	return resp, nil
}

func (c *AuthClient) getConn() (*grpc.ClientConn, error) {
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
func (c *AuthClient) Close() error {
	c.connMu.Lock()
	defer c.connMu.Unlock()

	if c.conn == nil {
		return nil
	}

	err := c.conn.Close()
	if err != nil {
		logger.Printf("[CLOUD-AUTH] gRPC connection close error: %v", err)
	}
	c.conn = nil
	return err
}

// buildCredentialBase64 encodes the credential as base64url(int64_be(site_id) + "." + api_secret).
// This mirrors the Rust SDK encoding in auth-client tests.
func (c *AuthClient) buildCredentialBase64() string {
	if c.cfg.APISecret == "" {
		return ""
	}
	var raw []byte
	buf := make([]byte, 8)
	// SiteID is specified as an int64 and encoded as its big-endian byte representation.
	// We allow negative values (two's complement), matching typical i64-to-bytes behavior.
	//
	//nolint:gosec // G115: intentional bit-preserving cast for wire encoding
	binary.BigEndian.PutUint64(buf, uint64(c.cfg.SiteID))
	raw = append(raw, buf...)
	raw = append(raw, '.')
	raw = append(raw, []byte(c.cfg.APISecret)...)
	return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(raw)
}
