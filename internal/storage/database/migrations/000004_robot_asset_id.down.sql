-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

ALTER TABLE robots
    DROP INDEX idx_asset_active_unique,
    DROP COLUMN _asset_unique;
