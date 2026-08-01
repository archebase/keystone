-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

ALTER TABLE bulk_runs
    DROP COLUMN cancel_requested_at,
    DROP COLUMN canceled_count;

UPDATE sync_logs
SET status = 'failed',
    error_message = COALESCE(error_message, 'bulk sync canceled')
WHERE status = 'canceled';

ALTER TABLE sync_logs
    DROP INDEX idx_sync_bulk_run_status,
    DROP COLUMN bulk_run_id,
    MODIFY COLUMN status ENUM('pending', 'in_progress', 'completed', 'failed') DEFAULT 'pending';
