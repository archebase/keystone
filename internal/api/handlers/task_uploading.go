// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

type taskStateExecutor interface {
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
}

func markOwnedTaskUploading(ctx context.Context, exec taskStateExecutor, deviceID, taskID string) (sql.Result, error) {
	if exec == nil {
		return nil, nil
	}
	now := time.Now().UTC()
	return exec.ExecContext(ctx, `
		UPDATE tasks
		SET
			status = 'uploading',
			updated_at = ?,
			error_message = NULL
		WHERE task_id = ?
		  AND status IN ('pending', 'ready', 'in_progress', 'uploading')
		  AND deleted_at IS NULL
		  AND EXISTS (
			SELECT 1
			FROM workstations ws
			JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
			WHERE ws.id = tasks.workstation_id
			  AND ws.deleted_at IS NULL
			  AND r.device_id = ?
		  )
	`, now, strings.TrimSpace(taskID), strings.TrimSpace(deviceID))
}

func failOwnedUploadingTask(ctx context.Context, exec taskStateExecutor, deviceID, taskID, reason string) (sql.Result, error) {
	if exec == nil {
		return nil, nil
	}
	now := time.Now().UTC()
	return exec.ExecContext(ctx, `
		UPDATE tasks
		SET
			status = 'failed',
			completed_at = CASE WHEN completed_at IS NULL THEN ? ELSE completed_at END,
			error_message = ?,
			updated_at = ?
		WHERE task_id = ?
		  AND status IN ('in_progress', 'uploading')
		  AND deleted_at IS NULL
		  AND EXISTS (
			SELECT 1
			FROM workstations ws
			JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
			WHERE ws.id = tasks.workstation_id
			  AND ws.deleted_at IS NULL
			  AND r.device_id = ?
		  )
	`, now, strings.TrimSpace(reason), now, strings.TrimSpace(taskID), strings.TrimSpace(deviceID))
}

func writeOwnedUploadingTaskError(ctx context.Context, exec taskStateExecutor, deviceID, taskID, message string) (sql.Result, error) {
	if exec == nil {
		return nil, nil
	}
	now := time.Now().UTC()
	return exec.ExecContext(ctx, `
		UPDATE tasks
		SET
			error_message = ?,
			updated_at = ?
		WHERE task_id = ?
		  AND status = 'uploading'
		  AND deleted_at IS NULL
		  AND EXISTS (
			SELECT 1
			FROM workstations ws
			JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
			WHERE ws.id = tasks.workstation_id
			  AND ws.deleted_at IS NULL
			  AND r.device_id = ?
		  )
	`, strings.TrimSpace(message), now, strings.TrimSpace(taskID), strings.TrimSpace(deviceID))
}
