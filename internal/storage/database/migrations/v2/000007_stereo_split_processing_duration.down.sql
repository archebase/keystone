-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

UPDATE episode_derivatives
SET duration_sec = COALESCE(duration_sec, processing_duration_sec)
WHERE processing_duration_sec IS NOT NULL;

ALTER TABLE episode_derivatives
    DROP COLUMN processing_duration_sec;
