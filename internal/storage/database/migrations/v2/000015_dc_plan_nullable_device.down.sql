-- SPDX-FileCopyrightText: 2026 ArcheBase
-- SPDX-License-Identifier: MulanPSL-2.0

ALTER TABLE dc_plan
    MODIFY COLUMN dc_device_id BIGINT NOT NULL;
