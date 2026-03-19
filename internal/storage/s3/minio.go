// Package s3 provides MinIO S3 storage wrapper
package s3

import (
	"context"
	"fmt"

	"archebase.com/keystone-edge/internal/logger"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Client S3 client
type Client struct {
	*minio.Client
	bucket   string
	endpoint string
	useSSL   bool
}

// Config S3 configuration
type Config struct {
	Endpoint  string
	AccessKey string `json:"-"`
	SecretKey string `json:"-"`
	Bucket    string
	UseSSL    bool
}

// Connect creates S3 client
func Connect(cfg *Config) (*Client, error) {
	logger.Printf("[S3] Connecting to MinIO: endpoint=%s, bucket=%s, useSSL=%v", cfg.Endpoint, cfg.Bucket, cfg.UseSSL)

	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create minio client: %w", err)
	}

	// Ensure bucket exists
	ctx := context.Background()
	exists, err := client.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("failed to check bucket: %w", err)
	}

	if !exists {
		err = client.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to create bucket: %w", err)
		}
		logger.Printf("[S3] Created bucket: %s", cfg.Bucket)
	} else {
		logger.Printf("[S3] Using existing bucket: %s", cfg.Bucket)
	}

	logger.Println("[S3] Connected to MinIO successfully")
	return &Client{
		Client:   client,
		bucket:   cfg.Bucket,
		endpoint: cfg.Endpoint,
		useSSL:   cfg.UseSSL,
	}, nil
}

// GetObjectURL returns object URL
func (c *Client) GetObjectURL(objectName string) string {
	return fmt.Sprintf("%s/%s/%s", c.EndpointURL(), c.bucket, objectName)
}

// EndpointURL returns endpoint URL
func (c *Client) EndpointURL() string {
	protocol := "http"
	if c.IsSecure() {
		protocol = "https"
	}
	return fmt.Sprintf("%s://%s", protocol, c.endpoint)
}

// IsSecure checks if SSL is enabled
func (c *Client) IsSecure() bool {
	return c.useSSL
}

// HeadObject checks whether an object exists in the bucket.
// Returns true if the object exists, false if it does not (404), or an error for other failures.
func (c *Client) HeadObject(ctx context.Context, objectName string) (bool, error) {
	_, err := c.StatObject(ctx, c.bucket, objectName, minio.StatObjectOptions{})
	if err != nil {
		errResp := minio.ToErrorResponse(err)
		logger.Printf("[S3] HeadObject error: key=%s, err=%v, code=%s, status=%d", objectName, err, errResp.Code, errResp.StatusCode)
		if errResp.Code == "NoSuchKey" || errResp.StatusCode == 404 {
			return false, nil
		}
		return false, fmt.Errorf("HeadObject %s: %w", objectName, err)
	}
	return true, nil
}

// Bucket returns the configured bucket name
func (c *Client) Bucket() string {
	return c.bucket
}
