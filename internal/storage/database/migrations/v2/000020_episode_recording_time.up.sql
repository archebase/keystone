-- SPDX-FileCopyrightText: 2026 ArcheBase
-- SPDX-License-Identifier: MulanPSL-2.0

ALTER TABLE episodes
    ADD COLUMN recording_started_at TIMESTAMP(6) NULL
        COMMENT 'Client-reported recording start time'
        AFTER duration_sec,
    ADD COLUMN recording_finished_at TIMESTAMP(6) NULL
        COMMENT 'Client-reported recording finish time'
        AFTER recording_started_at,
    ADD INDEX idx_episodes_recording_started_at (recording_started_at);
