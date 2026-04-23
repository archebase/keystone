-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

ALTER TABLE organizations
    DROP INDEX idx_name_del,
    DROP COLUMN _name_unique;
