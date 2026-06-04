-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

ALTER TABLE robots
    ADD COLUMN _asset_unique VARCHAR(100)
        GENERATED ALWAYS AS (
            CASE
                WHEN deleted_at IS NULL AND asset_id IS NOT NULL AND asset_id <> ''
                THEN asset_id
                ELSE NULL
            END
        ) STORED,
    ADD UNIQUE INDEX idx_asset_active_unique (_asset_unique);
