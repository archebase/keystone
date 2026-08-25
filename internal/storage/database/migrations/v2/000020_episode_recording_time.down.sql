-- SPDX-FileCopyrightText: 2026 ArcheBase
-- SPDX-License-Identifier: MulanPSL-2.0

ALTER TABLE episodes
    DROP INDEX idx_episodes_recording_started_at,
    DROP COLUMN recording_finished_at,
    DROP COLUMN recording_started_at;
