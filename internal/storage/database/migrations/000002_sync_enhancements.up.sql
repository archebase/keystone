-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

-- migrations/000002_sync_enhancements.up.sql
-- Add retry tracking and index to sync_logs for cloud sync worker

ALTER TABLE sync_logs
  ADD COLUMN attempt_count INT NOT NULL DEFAULT 0,
  ADD COLUMN next_retry_at TIMESTAMP NULL;

CREATE INDEX idx_sync_retry ON sync_logs (status, next_retry_at);
CREATE INDEX idx_sync_episode_status ON sync_logs (episode_id, status);
