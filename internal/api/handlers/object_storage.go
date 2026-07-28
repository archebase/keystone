// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"strings"

	"archebase.com/keystone-edge/internal/config"
	"archebase.com/keystone-edge/internal/storage/s3"
)

func configuredTOSBucket(storageCfg *config.StorageConfig) string {
	if storageCfg == nil || !strings.EqualFold(strings.TrimSpace(storageCfg.Type), "tos") {
		return ""
	}
	return strings.TrimSpace(storageCfg.Bucket)
}

func defaultObjectStorageBucket(s3Client *s3.Client, bucket string, storageCfg *config.StorageConfig) string {
	bucket = strings.TrimSpace(bucket)
	if s3Client == nil {
		if tosBucket := configuredTOSBucket(storageCfg); tosBucket != "" {
			return tosBucket
		}
	}
	return bucket
}
