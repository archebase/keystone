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
	EpisodePublicID string
	Context         dpRawTagContext
}

type dpRawTagContext struct {
	DCPlanID                int64
	WorkspaceID             int64
	DataCollectorOperatorID sql.NullString
	DataCollectorName       sql.NullString
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
		"episode_id":   input.EpisodePublicID,
		"sync_channel": "keystone_direct",
	}
	if input.Context.DCPlanID > 0 {
		tags["dc_plan_id"] = strconv.FormatInt(input.Context.DCPlanID, 10)
	}
	if input.Context.WorkspaceID > 0 {
		tags["workspace_id"] = strconv.FormatInt(input.Context.WorkspaceID, 10)
	}
	addNonEmptyTag(tags, "data_collector_operator_id", input.Context.DataCollectorOperatorID)
	addNonEmptyTag(tags, "data_collector_name", input.Context.DataCollectorName)
	return tags
}

func addNonEmptyTag(tags map[string]string, key string, value sql.NullString) {
	if !value.Valid {
		return
	}
	trimmed := strings.TrimSpace(value.String)
	if trimmed == "" {
		return
	}
	tags[key] = trimmed
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
