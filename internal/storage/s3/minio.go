// s3/minio.go - MinIO S3 storage wrapper
package s3

import (
	"context"
	"fmt"
	"log"

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
	AccessKey string
	SecretKey string
	Bucket    string
	UseSSL    bool
}

// Connect creates S3 client
func Connect(cfg *Config) (*Client, error) {
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
		log.Printf("[S3] Created bucket: %s", cfg.Bucket)
	} else {
		log.Printf("[S3] Using existing bucket: %s", cfg.Bucket)
	}

	log.Println("[S3] Connected to MinIO successfully")
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
