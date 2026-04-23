-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

-- Unique factory name among non-deleted rows (same pattern as slug).
ALTER TABLE factories
    ADD COLUMN _name_unique VARCHAR(400) GENERATED ALWAYS AS (CONCAT(IFNULL(name, ''), '|', IFNULL(deleted_at, ''))) STORED,
    ADD UNIQUE INDEX idx_name_del (_name_unique);
