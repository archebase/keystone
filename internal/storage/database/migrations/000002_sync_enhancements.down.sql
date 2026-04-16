-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

-- migrations/000002_sync_enhancements.down.sql
-- Revert sync_logs enhancements

DROP INDEX idx_sync_episode_status ON sync_logs;
DROP INDEX idx_sync_retry ON sync_logs;
DROP INDEX idx_sync_episode_latest ON sync_logs;

ALTER TABLE sync_logs
  DROP COLUMN next_retry_at,
  DROP COLUMN attempt_count;
