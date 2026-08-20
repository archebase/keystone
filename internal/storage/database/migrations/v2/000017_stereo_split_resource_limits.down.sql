-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

ALTER TABLE stereo_split_image_configs
    DROP COLUMN previous_resource_limits_enabled,
    DROP COLUMN resource_limits_enabled;
