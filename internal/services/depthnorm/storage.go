// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package depthnorm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"github.com/minio/minio-go/v7"
)

func (m *Manager) download(ctx context.Context, objectKey, destination string) error {
	object, err := m.s3.GetObject(ctx, m.bucket, objectKey, minio.GetObjectOptions{})
	if err != nil {
		return fmt.Errorf("open source object %s/%s: %w", m.bucket, objectKey, err)
	}
	defer func() { _ = object.Close() }()
	// #nosec G304 -- destination is an internally generated temporary path.
	file, err := os.Create(destination)
	if err != nil {
		return fmt.Errorf("create source file: %w", err)
	}
	defer func() { _ = file.Close() }()
	if _, err := io.Copy(file, object); err != nil {
		return fmt.Errorf("download source object %s/%s: %w", m.bucket, objectKey, err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync source file: %w", err)
	}
	return nil
}

func (m *Manager) upload(ctx context.Context, objectKey, source string) error {
	// #nosec G304 -- source is an internally generated output path.
	file, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open normalized output: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat normalized output: %w", err)
	}
	_, err = m.s3.PutObject(ctx, m.bucket, objectKey, file, info.Size(),
		minio.PutObjectOptions{ContentType: "application/octet-stream"})
	if err != nil {
		return fmt.Errorf("upload normalized object %s/%s: %w", m.bucket, objectKey, err)
	}
	return nil
}

func fileHash(path string) (string, int64, error) {
	// #nosec G304 -- path is an internally generated output path.
	file, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("open output checksum: %w", err)
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, fmt.Errorf("hash normalized output: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}
