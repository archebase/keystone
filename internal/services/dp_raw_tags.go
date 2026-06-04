// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"database/sql"
	"fmt"
	"path"
	"strconv"
	"strings"
)

const (
	dpReservedDeviceIDTagKey = "778a6d83c9ec49108537542a570966ee.device_id"
	dpReservedRawFileTagKey  = "a206e337ecdf70a93bb611cf6a30c346.raw_file"
)

type dpRawTagsInput struct {
	Profile         DPDeviceProfile
	McapKey         string
	SidecarTags     map[string]string
	EpisodeID       int64
	EpisodePublicID string
	TaskID          int64
	FactoryID       sql.NullInt64
	OrganizationID  sql.NullInt64
}

func buildDPDirectRawTags(input dpRawTagsInput) (map[string]string, error) {
	mcapKey := stripBucketPrefix(input.McapKey)
	rawFile := path.Base(strings.TrimSpace(mcapKey))
	if rawFile == "" || rawFile == "." || rawFile == "/" {
		return nil, fmt.Errorf("raw_file basename is empty for mcap key %q", input.McapKey)
	}

	merged := make(map[string]string, len(input.Profile.Tags)+len(input.SidecarTags)+8)
	if err := insertAllNonConflictingTags(merged, input.Profile.Tags); err != nil {
		return nil, fmt.Errorf("device profile tags: %w", err)
	}
	if err := insertNonConflictingTag(merged, dpReservedDeviceIDTagKey, input.Profile.DeviceID); err != nil {
		return nil, err
	}
	if err := insertNonConflictingTag(merged, dpReservedRawFileTagKey, rawFile); err != nil {
		return nil, err
	}
	if err := insertAllNonConflictingTags(merged, input.SidecarTags); err != nil {
		return nil, fmt.Errorf("sidecar tags: %w", err)
	}
	if err := insertAllNonConflictingTags(merged, keystoneExtraTags(input)); err != nil {
		return nil, fmt.Errorf("keystone extra tags: %w", err)
	}
	return merged, nil
}

func keystoneExtraTags(input dpRawTagsInput) map[string]string {
	tags := map[string]string{
		"episode_id":          input.EpisodePublicID,
		"keystone_episode_id": strconv.FormatInt(input.EpisodeID, 10),
		"sync_channel":        "keystone_direct",
	}
	if input.TaskID > 0 {
		tags["task_id"] = strconv.FormatInt(input.TaskID, 10)
	}
	if input.FactoryID.Valid {
		tags["factory_id"] = strconv.FormatInt(input.FactoryID.Int64, 10)
	}
	if input.OrganizationID.Valid {
		tags["organization_id"] = strconv.FormatInt(input.OrganizationID.Int64, 10)
	}
	return tags
}

func insertAllNonConflictingTags(dst map[string]string, src map[string]string) error {
	for key, value := range src {
		if err := insertNonConflictingTag(dst, key, value); err != nil {
			return err
		}
	}
	return nil
}

func insertNonConflictingTag(dst map[string]string, key string, value string) error {
	if key == "" {
		return fmt.Errorf("raw tag key must not be empty")
	}
	if existing, ok := dst[key]; ok {
		if existing != value {
			return fmt.Errorf("raw tag conflict for key %q", key)
		}
		return nil
	}
	dst[key] = value
	return nil
}
