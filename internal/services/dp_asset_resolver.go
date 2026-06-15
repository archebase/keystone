// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jmoiron/sqlx"
)

func assetIDFromEpisodeMetadata(metadata sql.NullString) string {
	if !metadata.Valid || strings.TrimSpace(metadata.String) == "" {
		return ""
	}
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(metadata.String), &raw); err != nil {
		return ""
	}
	value, _ := raw["asset_id"].(string)
	return strings.TrimSpace(value)
}

func resolveAssetIDForEpisode(ctx context.Context, db *sqlx.DB, episodeID int64, metadata sql.NullString, workstationID sql.NullInt64) (string, error) {
	metadataAssetID := assetIDFromEpisodeMetadata(metadata)
	if db != nil && workstationID.Valid && workstationID.Int64 > 0 {
		var row struct {
			RobotID         sql.NullInt64  `db:"robot_id"`
			ResolvedRobotID sql.NullInt64  `db:"resolved_robot_id"`
			AssetID         sql.NullString `db:"asset_id"`
		}
		err := db.GetContext(ctx, &row, `
			SELECT ws.robot_id, r.id AS resolved_robot_id, r.asset_id
			FROM workstations ws
			LEFT JOIN robots r ON r.id = ws.robot_id
			WHERE ws.id = ?
			LIMIT 1
		`, workstationID.Int64)
		if err != nil && err != sql.ErrNoRows {
			return "", fmt.Errorf("resolve asset_id for episode %d workstation %d: %w", episodeID, workstationID.Int64, err)
		}
		if err == nil && row.RobotID.Valid && row.RobotID.Int64 > 0 {
			if !row.ResolvedRobotID.Valid || row.ResolvedRobotID.Int64 <= 0 {
				if metadataAssetID != "" {
					return metadataAssetID, nil
				}
				return "", fmt.Errorf("episode %d workstation %d robot %d not found while resolving asset_id", episodeID, workstationID.Int64, row.RobotID.Int64)
			}
			assetID := strings.TrimSpace(row.AssetID.String)
			if row.AssetID.Valid && assetID != "" {
				return assetID, nil
			}
			return "", fmt.Errorf("episode %d workstation %d has no robot asset_id", episodeID, workstationID.Int64)
		}
	}
	if metadataAssetID != "" {
		return metadataAssetID, nil
	}
	if db == nil {
		return "", fmt.Errorf("database is not available")
	}
	if !workstationID.Valid || workstationID.Int64 <= 0 {
		return "", fmt.Errorf("episode %d has no asset_id metadata and no workstation_id", episodeID)
	}
	return "", fmt.Errorf("episode %d workstation %d not found while resolving asset_id", episodeID, workstationID.Int64)
}
