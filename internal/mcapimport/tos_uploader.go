// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package mcapimport

import (
	"context"
	"fmt"
	"math"
	"os"
	"strings"
	"sync/atomic"

	"github.com/volcengine/ve-tos-golang-sdk/v2/tos"
	"github.com/volcengine/ve-tos-golang-sdk/v2/tos/enum"
)

// TOSUploader uploads files with the official Volcengine TOS SDK.
type TOSUploader struct {
	// Progress may be called concurrently by the TOS SDK and must be concurrency-safe.
	Progress func(format string, args ...any)
}

// Upload performs a multipart upload using the temporary STS credentials issued by Keystone.
func (u TOSUploader) Upload(ctx context.Context, filePath string, session UploadSession, parallel int) (ObjectUploadResult, error) {
	info, err := os.Stat(filePath)
	if err != nil {
		return ObjectUploadResult{}, fmt.Errorf("stat upload file: %w", err)
	}
	partCount := int64(1) + (info.Size()-1)/session.PartSizeBytes
	if partCount > math.MaxInt32 {
		return ObjectUploadResult{}, fmt.Errorf("file requires too many upload parts: %d", partCount)
	}

	credentials := tos.NewStaticCredentials(session.AccessKeyID, session.AccessKeySecret)
	credentials.WithSecurityToken(session.SecurityToken)
	client, err := tos.NewClientV2(
		session.Endpoint,
		tos.WithCredentials(credentials),
		tos.WithRegion(session.Region),
		tos.WithMaxRetryCount(5),
		tos.WithLogger(discardTOSLogger{}),
	)
	if err != nil {
		return ObjectUploadResult{}, fmt.Errorf("create TOS client: %w", err)
	}

	listener := &uploadEventListener{total: partCount, progress: u.Progress}
	output, err := client.UploadFile(ctx, &tos.UploadFileInput{
		CreateMultipartUploadV2Input: tos.CreateMultipartUploadV2Input{
			Bucket:          session.Bucket,
			Key:             session.ObjectKey,
			ContentType:     "application/octet-stream",
			ForbidOverwrite: true,
		},
		FilePath:            filePath,
		PartSize:            session.PartSizeBytes,
		TaskNum:             parallel,
		EnableCheckpoint:    false,
		UploadEventListener: listener,
	})
	if err != nil {
		return ObjectUploadResult{}, err
	}
	etag := strings.TrimSpace(output.ETag)
	if etag == "" {
		return ObjectUploadResult{}, fmt.Errorf("TOS returned an empty object ETag")
	}
	//nolint:gosec // G115: partCount was validated against math.MaxInt32 above.
	return ObjectUploadResult{ETag: etag, PartCount: int32(partCount)}, nil
}

// discardTOSLogger prevents the TOS SDK's default logger from writing slow-request
// diagnostics to stdout, which is reserved for the CLI's final JSON result.
type discardTOSLogger struct{}

func (discardTOSLogger) Debug(...any) {}
func (discardTOSLogger) Info(...any)  {}
func (discardTOSLogger) Warn(...any)  {}
func (discardTOSLogger) Error(...any) {}
func (discardTOSLogger) Fatal(...any) {}

var _ tos.Logger = discardTOSLogger{}

type uploadEventListener struct {
	total    int64
	complete atomic.Int64
	progress func(format string, args ...any)
}

func (l *uploadEventListener) EventChange(event *tos.UploadEvent) {
	if l == nil || l.progress == nil || event == nil || event.Type != enum.UploadEventUploadPartSucceed {
		return
	}
	completed := l.complete.Add(1)
	l.progress("Uploaded TOS part %d/%d", completed, l.total)
}
