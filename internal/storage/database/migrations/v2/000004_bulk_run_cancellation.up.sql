-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

ALTER TABLE bulk_runs
    ADD COLUMN canceled_count BIGINT NOT NULL DEFAULT 0 AFTER skipped_count,
    ADD COLUMN cancel_requested_at TIMESTAMP NULL AFTER error_message;

ALTER TABLE sync_logs
    MODIFY COLUMN status ENUM('pending', 'in_progress', 'completed', 'failed', 'canceled') DEFAULT 'pending',
    ADD COLUMN bulk_run_id VARCHAR(64) NULL AFTER episode_id,
    ADD INDEX idx_sync_bulk_run_status (bulk_run_id, status);
