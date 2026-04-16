// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package cloud

import (
	"context"
	"fmt"
	"io"
	"math"
	"time"

	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/storage/s3"

	"github.com/minio/minio-go/v7"
)

// UploaderConfig defines configuration for the high-level uploader.
type UploaderConfig struct {
	// RequestTimeout is the per-RPC/HTTP deadline for gateway calls.
	RequestTimeout time.Duration
	// OSSTimeout is the HTTP timeout for individual OSS part uploads.
	OSSTimeout time.Duration
}

// UploadRequest describes an episode upload from MinIO to cloud.
type UploadRequest struct {
	// EpisodeID is the unique episode identifier used as client hint.
	EpisodeID string
	// McapKey is the MinIO object key for the MCAP file (without bucket prefix).
	McapKey string
	// RawTags are arbitrary key-value tags passed to the data-gateway.
	RawTags map[string]string
	// ClientHints are passed to CreateFileUpload for server-side routing.
	ClientHints map[string]string
}

// UploadResult describes the outcome of a successful cloud upload.
type UploadResult struct {
	UploadID      string
	Bucket        string
	ObjectKey     string
	FileSize      int64
	OSSObjectETag string
}

// Uploader orchestrates the complete upload flow:
//  1. Auth (gRPC) → JWT
//  2. Gateway (gRPC) → STS credentials + object key
//  3. OSS (REST) → multipart upload
//  4. Gateway (gRPC) → complete upload
type Uploader struct {
	gateway     *GatewayClient
	oss         *OSSUploader
	minioClient *s3.Client
	minioBucket string
	cfg         UploaderConfig
}

// NewUploader creates a new uploader that streams data from MinIO to cloud OSS.
func NewUploader(gateway *GatewayClient, minioClient *s3.Client, minioBucket string, cfg UploaderConfig) *Uploader {
	return &Uploader{
		gateway:     gateway,
		oss:         NewOSSUploader(cfg.OSSTimeout),
		minioClient: minioClient,
		minioBucket: minioBucket,
		cfg:         cfg,
	}
}

// abortMultipartUpload attempts to abort a multipart upload with a timeout.
// It uses context.Background() as base to ensure the abort is independent of the
// caller's context, but with a 30s timeout to prevent indefinite hanging.
func (u *Uploader) abortMultipartUpload(session *UploadSession, multipartUploadID string) {
	abortCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	u.oss.AbortMultipartUpload(abortCtx, session, multipartUploadID)
}

// Upload streams a file from MinIO to cloud OSS via the data-gateway control plane.
// It does NOT buffer the entire file in memory — only one part_size_bytes buffer is held at a time.
func (u *Uploader) Upload(ctx context.Context, req UploadRequest) (*UploadResult, error) {
	// Merge client hints
	hints := map[string]string{
		"episode_id": req.EpisodeID,
		"source":     "keystone-edge",
	}
	for k, v := range req.ClientHints {
		hints[k] = v
	}

	// Step 1: Get file size from MinIO
	objInfo, err := u.minioClient.StatObject(ctx, u.minioBucket, req.McapKey, minio.StatObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("stat minio object %s/%s: %w", u.minioBucket, req.McapKey, err)
	}
	fileSize := objInfo.Size
	if fileSize == 0 {
		return nil, fmt.Errorf("zero-byte file uploads are not allowed: %s", req.McapKey)
	}

	logger.Printf("[CLOUD-UPLOAD] Starting upload: episode=%s mcap=%s size=%d", req.EpisodeID, req.McapKey, fileSize)

	// Step 2: CreateFileUpload → get STS credentials + object_key
	session, err := u.gateway.CreateFileUpload(ctx, hints)
	if err != nil {
		return nil, fmt.Errorf("create file upload: %w", err)
	}

	logger.Printf("[CLOUD-UPLOAD] Session created: upload_id=%s object_key=%s bucket=%s part_size=%d",
		session.UploadID, session.ObjectKey, session.Bucket, session.PartSizeBytes)

	// Step 3: Initiate multipart upload on OSS
	multipartUploadID, err := u.oss.InitiateMultipartUpload(ctx, session)
	if err != nil {
		return nil, fmt.Errorf("initiate multipart upload: %w", err)
	}

	logger.Printf("[CLOUD-UPLOAD] Multipart initiated: multipart_upload_id=%s", multipartUploadID)

	// Step 4: Stream from MinIO → OSS in parts
	mcapStream, err := u.minioClient.GetObject(ctx, u.minioBucket, req.McapKey, minio.GetObjectOptions{})
	if err != nil {
		u.abortMultipartUpload(session, multipartUploadID)
		return nil, fmt.Errorf("get minio object %s: %w", req.McapKey, err)
	}
	defer func() {
		_ = mcapStream.Close()
	}()

	partSizeBytes := session.PartSizeBytes
	if partSizeBytes <= 0 {
		partSizeBytes = 8 * 1024 * 1024 // 8MB default
	}

	buf := make([]byte, partSizeBytes)
	var parts []UploadedPart
	var partMD5s [][16]byte
	var offset int64
	partNumber := 1

	for offset < fileSize {
		if err := ctx.Err(); err != nil {
			u.abortMultipartUpload(session, multipartUploadID)
			return nil, err
		}

		remaining := fileSize - offset
		readSize := partSizeBytes
		if remaining < readSize {
			readSize = remaining
		}

		n, err := io.ReadFull(mcapStream, buf[:readSize])
		if err != nil && err != io.ErrUnexpectedEOF {
			u.abortMultipartUpload(session, multipartUploadID)
			return nil, fmt.Errorf("read part %d from minio: %w", partNumber, err)
		}

		partSlice := buf[:n]
		partMD5s = append(partMD5s, MD5DigestBytes(partSlice))

		etag, err := u.oss.UploadPart(ctx, session, multipartUploadID, partNumber, partSlice)
		if err != nil {
			u.abortMultipartUpload(session, multipartUploadID)
			return nil, fmt.Errorf("upload part %d: %w", partNumber, err)
		}

		parts = append(parts, UploadedPart{
			PartNumber: partNumber,
			ETag:       etag,
		})

		offset += int64(n)
		partNumber++

		if partNumber%10 == 0 {
			logger.Printf("[CLOUD-UPLOAD] Progress: episode=%s parts=%d offset=%d/%d",
				req.EpisodeID, len(parts), offset, fileSize)
		}
	}

	// Step 5: Complete multipart upload on OSS
	_, err = u.oss.CompleteMultipartUpload(ctx, session, multipartUploadID, parts)
	if err != nil {
		u.abortMultipartUpload(session, multipartUploadID)
		return nil, fmt.Errorf("complete multipart upload on OSS: %w", err)
	}

	localETag := BuildMultipartETag(partMD5s)
	logger.Printf("[CLOUD-UPLOAD] OSS upload complete: parts=%d etag=%s", len(parts), localETag)

	// Step 6: Refresh STS credentials if about to expire before CompleteUpload RPC
	if time.Until(session.STSExpireAt) <= u.cfg.RequestTimeout {
		refreshed, err := u.gateway.RefreshUploadCredentials(ctx, session.UploadID)
		if err != nil {
			logger.Printf("[CLOUD-UPLOAD] Warning: refresh credentials failed (proceeding anyway): %v", err)
		} else {
			session = refreshed
		}
	}

	// Step 7: Notify data-gateway that upload is complete
	if len(parts) > math.MaxInt32 {
		return nil, fmt.Errorf("too many upload parts: %d", len(parts))
	}
	//nolint:gosec // G115: len(parts) validated to fit into int32 above
	err = u.gateway.CompleteUpload(ctx, session.UploadID, fileSize, req.RawTags, int32(len(parts)), localETag)
	if err != nil {
		return nil, fmt.Errorf("complete upload on gateway: %w", err)
	}

	logger.Printf("[CLOUD-UPLOAD] Upload complete: episode=%s upload_id=%s object_key=%s",
		req.EpisodeID, session.UploadID, session.ObjectKey)

	return &UploadResult{
		UploadID:      session.UploadID,
		Bucket:        session.Bucket,
		ObjectKey:     session.ObjectKey,
		FileSize:      fileSize,
		OSSObjectETag: localETag,
	}, nil
}
