-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

DROP TABLE local_cleanup_jobs;

ALTER TABLE episodes
    DROP INDEX idx_episodes_local_storage_status,
    DROP COLUMN local_storage_delete_error,
    DROP COLUMN local_storage_deleted_at,
    DROP COLUMN local_storage_status;
