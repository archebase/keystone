// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"database/sql"
	"fmt"
	"path"
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
	SOPSlug                 sql.NullString
	SOPVersion              sql.NullString
	SOPDescription          sql.NullString
	Scene                   sql.NullString
	Subscene                sql.NullString
	RobotType               sql.NullString
	DataCollectorOperatorID sql.NullString
	DataCollectorName       sql.NullString
	OrderName               sql.NullString
	BatchID                 sql.NullString
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
	addNonEmptyTag(tags, "sop_slug", input.Context.SOPSlug)
	addNonEmptyTag(tags, "sop_version", input.Context.SOPVersion)
	addNonEmptyTag(tags, "sop_description", input.Context.SOPDescription)
	addNonEmptyTag(tags, "scene", input.Context.Scene)
	addNonEmptyTag(tags, "subscene", input.Context.Subscene)
	addNonEmptyTag(tags, "robot_type", input.Context.RobotType)
	addNonEmptyTag(tags, "data_collector_operator_id", input.Context.DataCollectorOperatorID)
	addNonEmptyTag(tags, "data_collector_name", input.Context.DataCollectorName)
	addNonEmptyTag(tags, "order_name", input.Context.OrderName)
	addNonEmptyTag(tags, "batch_id", input.Context.BatchID)
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
