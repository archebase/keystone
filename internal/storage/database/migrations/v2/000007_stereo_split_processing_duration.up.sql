-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

ALTER TABLE episode_derivatives
    ADD COLUMN processing_duration_sec DECIMAL(10, 2) NULL AFTER duration_sec;

-- Successful generations retain the verified processing manifest. Recover
-- wall time for historical QA-approved rows whose duration_sec already holds
-- the MCAP data duration.
UPDATE episode_derivatives
SET processing_duration_sec = GREATEST(
    0,
    TIMESTAMPDIFF(
        MICROSECOND,
        CAST(
            REPLACE(
                REPLACE(JSON_UNQUOTE(JSON_EXTRACT(processing_result, '$.started_at')), 'T', ' '),
                'Z',
                ''
            ) AS DATETIME(6)
        ),
        CAST(
            REPLACE(
                REPLACE(JSON_UNQUOTE(JSON_EXTRACT(processing_result, '$.finished_at')), 'T', ' '),
                'Z',
                ''
            ) AS DATETIME(6)
        )
    ) / 1000000
)
WHERE processing_status = 'succeeded'
  AND processing_result IS NOT NULL
  AND JSON_EXTRACT(processing_result, '$.started_at') IS NOT NULL
  AND JSON_EXTRACT(processing_result, '$.finished_at') IS NOT NULL;

-- Before this migration, duration_sec temporarily held processing wall time
-- until automatic QA replaced it with the MCAP timestamp span. Preserve that
-- value for generations that have not passed QA, then restore duration_sec to
-- its single meaning: derived MCAP data duration.
UPDATE episode_derivatives
SET processing_duration_sec = COALESCE(processing_duration_sec, duration_sec),
    duration_sec = NULL
WHERE processing_status = 'succeeded'
  AND qa_status <> 'approved'
  AND duration_sec IS NOT NULL;
