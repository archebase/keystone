-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

ALTER TABLE robots
    DROP INDEX idx_device_name_active_unique,
    DROP COLUMN _device_name_unique,
    DROP COLUMN device_name;

ALTER TABLE dc_plan
    DROP COLUMN dc_task_description,
    DROP COLUMN dc_project_description;
