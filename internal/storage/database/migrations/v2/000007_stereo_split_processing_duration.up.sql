-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

ALTER TABLE episode_derivatives
    ADD COLUMN processing_duration_sec DECIMAL(10, 2) NULL AFTER duration_sec;

-- Before this migration, duration_sec temporarily held processing wall time
-- until automatic QA replaced it with the MCAP timestamp span. Preserve that
-- value for generations that have not passed QA, then restore duration_sec to
-- its single meaning: derived MCAP data duration.
UPDATE episode_derivatives
SET processing_duration_sec = duration_sec,
    duration_sec = NULL
WHERE processing_status = 'succeeded'
  AND qa_status <> 'approved'
  AND duration_sec IS NOT NULL;
