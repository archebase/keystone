-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

CREATE TABLE IF NOT EXISTS cli_sync_runs (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    episode_id BIGINT NOT NULL,
    status ENUM('pending', 'in_progress', 'completed', 'failed') NOT NULL DEFAULT 'pending',
    source_path VARCHAR(1024),
    temp_path VARCHAR(1024),
    dp_config_path VARCHAR(1024),
    file_id VARCHAR(255),
    logical_upload_id VARCHAR(255),
    upload_id VARCHAR(255),
    bucket VARCHAR(255),
    object_key VARCHAR(1024),
    file_size BIGINT,
    oss_object_etag VARCHAR(255),
    duration_sec INT,
    error_message TEXT,
    stdout_json JSON DEFAULT NULL,
    started_at TIMESTAMP NULL,
    completed_at TIMESTAMP NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    INDEX idx_cli_sync_episode (episode_id),
    INDEX idx_cli_sync_status (status),
    INDEX idx_cli_sync_created (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
