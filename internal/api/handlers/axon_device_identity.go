// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

var errAxonDeviceNameNotFound = errors.New("axon device name not found")

func resolveAxonDeviceID(ctx context.Context, db *sqlx.DB, deviceName string) (string, error) {
	if db == nil {
		return "", fmt.Errorf("resolve axon device: database unavailable")
	}

	deviceName = strings.TrimSpace(deviceName)
	if deviceName == "" {
		return "", errAxonDeviceNameNotFound
	}

	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var deviceID string
	if err := db.GetContext(queryCtx, &deviceID, `
		SELECT device_id
		FROM robots
		WHERE device_name = ?
			AND status = 'active'
			AND deleted_at IS NULL
		LIMIT 1
	`, deviceName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", errAxonDeviceNameNotFound
		}
		return "", fmt.Errorf("resolve axon device: %w", err)
	}

	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return "", fmt.Errorf("resolve axon device: database returned empty device_id")
	}
	return deviceID, nil
}
