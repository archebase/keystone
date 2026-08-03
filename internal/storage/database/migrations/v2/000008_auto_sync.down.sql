-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

ALTER TABLE episodes
    DROP INDEX idx_episodes_auto_sync_capture_recovery,
    DROP INDEX idx_episodes_auto_sync_reconcile,
    DROP COLUMN auto_sync_observed_at,
    DROP COLUMN auto_sync_requested_at,
    DROP COLUMN auto_sync_device_type,
    DROP COLUMN auto_sync_requested;

DROP TABLE auto_sync_configs;
