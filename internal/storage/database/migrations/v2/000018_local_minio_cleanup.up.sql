-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

ALTER TABLE episodes
    ADD COLUMN local_storage_status ENUM('available', 'deleting', 'deleted', 'delete_failed') NOT NULL DEFAULT 'available' AFTER cloud_synced_at,
    ADD COLUMN local_storage_deleted_at TIMESTAMP NULL AFTER local_storage_status,
    ADD COLUMN local_storage_delete_error TEXT NULL AFTER local_storage_deleted_at,
    ADD INDEX idx_episodes_local_storage_status (local_storage_status);

CREATE TABLE local_cleanup_jobs (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    episode_id BIGINT NOT NULL,
    bucket VARCHAR(255) NOT NULL,
    object_key VARCHAR(1024) NOT NULL,
    status ENUM('pending', 'in_progress', 'completed', 'failed') NOT NULL DEFAULT 'pending',
    requested_by VARCHAR(128) NULL,
    requested_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    started_at TIMESTAMP NULL,
    completed_at TIMESTAMP NULL,
    retry_count INT NOT NULL DEFAULT 0,
    error_message TEXT NULL,
    UNIQUE INDEX idx_local_cleanup_episode (episode_id),
    INDEX idx_local_cleanup_status (status),
    INDEX idx_local_cleanup_requested_at (requested_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
