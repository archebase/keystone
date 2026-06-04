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
	if assetID := assetIDFromEpisodeMetadata(metadata); assetID != "" {
		return assetID, nil
	}
	if db == nil {
		return "", fmt.Errorf("database is not available")
	}
	if !workstationID.Valid || workstationID.Int64 <= 0 {
		return "", fmt.Errorf("episode %d has no asset_id metadata and no workstation_id", episodeID)
	}

	var row struct {
		AssetID sql.NullString `db:"asset_id"`
	}
	err := db.GetContext(ctx, &row, `
		SELECT r.asset_id
		FROM workstations ws
		LEFT JOIN robots r ON r.id = ws.robot_id
		WHERE ws.id = ?
		LIMIT 1
	`, workstationID.Int64)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("episode %d workstation %d not found while resolving asset_id", episodeID, workstationID.Int64)
	}
	if err != nil {
		return "", fmt.Errorf("resolve asset_id for episode %d workstation %d: %w", episodeID, workstationID.Int64, err)
	}
	assetID := strings.TrimSpace(row.AssetID.String)
	if !row.AssetID.Valid || assetID == "" {
		return "", fmt.Errorf("episode %d workstation %d has no robot asset_id", episodeID, workstationID.Int64)
	}
	return assetID, nil
}
