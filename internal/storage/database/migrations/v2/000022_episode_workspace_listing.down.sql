-- SPDX-FileCopyrightText: 2026 ArcheBase
-- SPDX-License-Identifier: MulanPSL-2.0

ALTER TABLE episodes
    DROP INDEX idx_episodes_org_deleted_created_id;
