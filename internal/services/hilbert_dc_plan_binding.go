// SPDX-FileCopyrightText: 2026 ArcheBase
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"archebase.com/keystone-edge/internal/auth"

	"github.com/jmoiron/sqlx"
)

// HilbertDCPlanBinder captures the Hilbert call needed to bind a dc plan to a device.
type HilbertDCPlanBinder interface {
	PatchDCPlanDCDeviceID(ctx context.Context, workspaceID, planID, deviceID int64) (bool, error)
}

// ResolveDCPlanDeviceName returns the local display name for a Hilbert device.
// The numeric device ID is used as a fallback only when the local name is empty.
func ResolveDCPlanDeviceName(ctx context.Context, db *sqlx.DB, workspaceID, deviceID int64) (string, error) {
	if db == nil || workspaceID <= 0 || deviceID <= 0 {
		return "", fmt.Errorf("invalid dc plan device name lookup")
	}
	var device struct {
		DeviceName sql.NullString `db:"device_name"`
	}
	if err := db.GetContext(ctx, &device, `
		SELECT device_name
		FROM robots
		WHERE workspace_id = ? AND device_id = ? AND deleted_at IS NULL
		LIMIT 1
	`, workspaceID, fmt.Sprintf("%d", deviceID)); err != nil {
		return "", fmt.Errorf("query dc plan device name: %w", err)
	}
	if device.DeviceName.Valid && strings.TrimSpace(device.DeviceName.String) != "" {
		return strings.TrimSpace(device.DeviceName.String), nil
	}
	return fmt.Sprintf("%d", deviceID), nil
}

// operator selects a device for their unbound plans, not only at first upload. Already-bound
// plans are left untouched, so concurrent device selection cannot overwrite the Hilbert record.
func BindDCPlanDevice(
	ctx context.Context,
	db *sqlx.DB,
	hilbert HilbertDCPlanBinder,
	workspaceID int64,
	planID int64,
	deviceID int64,
) error {
	if db == nil || hilbert == nil || workspaceID <= 0 || planID <= 0 || deviceID <= 0 {
		return fmt.Errorf("dc plan device binding unavailable")
	}

	var planRow struct {
		CurrentDeviceID sql.NullInt64 `db:"dc_device_id"`
		Status          string        `db:"status"`
	}
	if err := db.GetContext(ctx, &planRow, `
		SELECT dc_device_id, COALESCE(status, 'pending_collection') AS status
		FROM dc_plan
		WHERE id = ? AND workspace_id = ? AND deleted_at IS NULL
		LIMIT 1
	`, planID, workspaceID); err != nil {
		return fmt.Errorf("query dc plan for device binding: %w", err)
	}
	if strings.EqualFold(strings.TrimSpace(planRow.Status), "collected") {
		return fmt.Errorf("dc plan %d is closed", planID)
	}
	currentDeviceID := planRow.CurrentDeviceID
	if currentDeviceID.Valid && currentDeviceID.Int64 == deviceID {
		return nil
	}
	if currentDeviceID.Valid {
		return fmt.Errorf("dc plan %d is already bound to device %d", planID, currentDeviceID.Int64)
	}
	var operator string
	if err := db.GetContext(ctx, &operator, `
		SELECT operator
		FROM dc_plan
		WHERE id = ? AND workspace_id = ? AND deleted_at IS NULL
		LIMIT 1
	`, planID, workspaceID); err != nil {
		return fmt.Errorf("query dc plan operator for device binding: %w", err)
	}

	bound, err := hilbert.PatchDCPlanDCDeviceID(ctx, workspaceID, planID, deviceID)
	if err != nil || !bound {
		return fmt.Errorf("patch dc plan device: %w", err)
	}
	deviceName, err := ResolveDCPlanDeviceName(ctx, db, workspaceID, deviceID)
	if err != nil {
		return fmt.Errorf("resolve dc plan device name: %w", err)
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE dc_plan SET dc_device_id = ?, dc_device_name = ?, local_updated_at = ?
		WHERE id = ? AND workspace_id = ? AND deleted_at IS NULL
	`, deviceID, deviceName, time.Now().UTC(), planID, workspaceID); err != nil {
		return fmt.Errorf("update dc plan device projection: %w", err)
	}
	return EnsureDCPlanWorkstation(ctx, db, auth.HilbertDCPlan{
		ID:          planID,
		WorkspaceID: workspaceID,
		Operator:    operator,
		DCDeviceID:  &deviceID,
	}, time.Now().UTC())
}
