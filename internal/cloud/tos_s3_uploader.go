// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package cloud

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// TOSS3UploadTarget describes a single Hilbert-issued TOS S3-compatible upload target.
type TOSS3UploadTarget struct {
	Endpoint        string
	Region          string
	Bucket          string
	Key             string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// TOSS3Uploader uploads objects to Volcengine TOS through its S3-compatible endpoint.
type TOSS3Uploader struct {
	timeout time.Duration
}

// NewTOSS3Uploader creates a TOS S3-compatible uploader.
func NewTOSS3Uploader(timeout time.Duration) *TOSS3Uploader {
	return &TOSS3Uploader{timeout: timeout}
}

// PutObject streams one object to the Hilbert-issued target and returns the object ETag.
func (u *TOSS3Uploader) PutObject(ctx context.Context, target TOSS3UploadTarget, reader io.Reader, size int64, progress UploadProgressFunc) (string, error) {
	if size <= 0 {
		return "", fmt.Errorf("tos upload size must be positive")
	}
	endpoint, secure, err := normalizeTOSS3Endpoint(target.Endpoint)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(target.Bucket) == "" || strings.TrimSpace(target.Key) == "" {
		return "", fmt.Errorf("tos upload target missing bucket or key")
	}
	if strings.TrimSpace(target.AccessKeyID) == "" || strings.TrimSpace(target.SecretAccessKey) == "" {
		return "", fmt.Errorf("tos upload target missing temporary credentials")
	}

	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(target.AccessKeyID, target.SecretAccessKey, target.SessionToken),
		Secure: secure,
		Region: strings.TrimSpace(target.Region),
	})
	if err != nil {
		return "", fmt.Errorf("create tos s3 client: %w", err)
	}

	counting := &progressReadCloser{reader: reader, total: size, progress: progress}
	info, err := client.PutObject(ctx, target.Bucket, target.Key, counting, size, minio.PutObjectOptions{
		ContentType:    "application/octet-stream",
		SendContentMd5: false,
	})
	if err != nil {
		return "", fmt.Errorf("put tos object: %w", err)
	}
	if progress != nil {
		progress(size, size)
	}
	return info.ETag, nil
}

func normalizeTOSS3Endpoint(raw string) (endpoint string, secure bool, err error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", false, fmt.Errorf("tos endpoint is empty")
	}
	secure = true
	if strings.Contains(value, "://") {
		parsed, parseErr := url.Parse(value)
		if parseErr != nil {
			return "", false, fmt.Errorf("parse tos endpoint: %w", parseErr)
		}
		if parsed.Host == "" {
			return "", false, fmt.Errorf("tos endpoint missing host")
		}
		secure = parsed.Scheme != "http"
		value = parsed.Host
	}
	return value, secure, nil
}

type progressReadCloser struct {
	reader   io.Reader
	total    int64
	read     int64
	progress UploadProgressFunc
}

func (r *progressReadCloser) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.read += int64(n)
		if r.progress != nil {
			if r.read > r.total {
				r.read = r.total
			}
			r.progress(r.read, r.total)
		}
	}
	return n, err
}
