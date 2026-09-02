-- SPDX-FileCopyrightText: 2026 ArcheBase
-- SPDX-License-Identifier: MulanPSL-2.0

-- organization_id is the legacy column name for the Workspace ID stored on episodes.
-- Keep the column name for compatibility, but index it with the list filter and sort keys.
ALTER TABLE episodes
    ADD INDEX idx_episodes_org_deleted_created_id (
        organization_id,
        deleted_at,
        created_at,
        id
    );
