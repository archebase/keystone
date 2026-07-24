-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

-- Add the Hilbert plan descriptions used by task generation.
ALTER TABLE dc_plan
    ADD COLUMN dc_project_description TEXT AFTER dc_project_name,
    ADD COLUMN dc_task_description TEXT AFTER dc_task_name;

-- Store the Hilbert device name directly and keep active names unique.
ALTER TABLE robots
    ADD COLUMN device_name VARCHAR(255) AFTER device_id,
    ADD COLUMN _device_name_unique VARCHAR(255) GENERATED ALWAYS AS (
        CASE
            WHEN deleted_at IS NULL AND device_name IS NOT NULL AND device_name <> ''
            THEN device_name
            ELSE NULL
        END
    ) STORED AFTER _device_unique,
    ADD UNIQUE INDEX idx_device_name_active_unique (_device_name_unique);
