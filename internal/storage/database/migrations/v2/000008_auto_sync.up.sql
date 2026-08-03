-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

CREATE TABLE auto_sync_configs (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    enabled BOOLEAN NOT NULL DEFAULT FALSE,
    previous_enabled BOOLEAN NULL,
    created_by VARCHAR(191) NULL,
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    INDEX idx_auto_sync_configs_created_at (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- The bootstrap revision keeps automatic downstream processing disabled until
-- an administrator explicitly opts in.
INSERT INTO auto_sync_configs (enabled, previous_enabled, created_by)
VALUES (FALSE, NULL, 'migration-bootstrap');

ALTER TABLE episodes
    ADD COLUMN auto_sync_requested BOOLEAN NOT NULL DEFAULT FALSE AFTER cloud_publish_claimed_at,
    ADD COLUMN auto_sync_device_type VARCHAR(255) NULL AFTER auto_sync_requested,
    ADD COLUMN auto_sync_requested_at TIMESTAMP NULL AFTER auto_sync_device_type,
    ADD COLUMN auto_sync_observed_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) AFTER auto_sync_requested_at,
    ADD INDEX idx_episodes_auto_sync_reconcile (
        auto_sync_requested, qa_status, cloud_synced, auto_sync_requested_at
    ),
    ADD INDEX idx_episodes_auto_sync_capture_recovery (
        auto_sync_requested, auto_sync_observed_at, id
    );
