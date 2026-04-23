-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

ALTER TABLE batches
    DROP INDEX idx_org,
    DROP COLUMN organization_id;

