-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

CREATE TABLE IF NOT EXISTS bulk_runs (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    run_id VARCHAR(64) NOT NULL UNIQUE,
    action VARCHAR(32) NOT NULL,
    status VARCHAR(32) NOT NULL,
    total_count BIGINT NOT NULL DEFAULT 0,
    processed_count BIGINT NOT NULL DEFAULT 0,
    passed_count BIGINT NOT NULL DEFAULT 0,
    qa_failed_count BIGINT NOT NULL DEFAULT 0,
    processing_failed_count BIGINT NOT NULL DEFAULT 0,
    skipped_count BIGINT NOT NULL DEFAULT 0,
    error_message TEXT,
    started_at TIMESTAMP NULL,
    finished_at TIMESTAMP NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    INDEX idx_bulk_runs_action_status (action, status),
    INDEX idx_bulk_runs_updated_at (updated_at)
);
