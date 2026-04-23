-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

-- Reverses 000004_org_adjust.up.sql

-- ============================================================
-- Part C reverse: drop batches.organization_id
-- ============================================================

ALTER TABLE batches
    DROP INDEX idx_org,
    DROP COLUMN organization_id;

-- ============================================================
-- Part B reverse: drop unique name constraints
-- ============================================================

ALTER TABLE organizations
    DROP INDEX idx_name_del,
    DROP COLUMN _name_unique;

ALTER TABLE factories
    DROP INDEX idx_name_del,
    DROP COLUMN _name_unique;

-- ============================================================
-- Part A reverse: revert multi-tenancy hierarchy redesign
-- ============================================================

ALTER TABLE workstations
    DROP INDEX idx_organization,
    DROP COLUMN organization_id;

ALTER TABLE inspectors
    DROP INDEX idx_organization,
    DROP COLUMN organization_id;

ALTER TABLE data_collectors
    DROP INDEX idx_organization,
    DROP COLUMN organization_id;

ALTER TABLE factories
    ADD COLUMN organization_id BIGINT NOT NULL DEFAULT 0 AFTER id,
    ADD INDEX idx_org (organization_id);

UPDATE factories f
    INNER JOIN organizations o ON o.factory_id = f.id AND o.deleted_at IS NULL
SET f.organization_id = o.id;

ALTER TABLE factories
    MODIFY COLUMN organization_id BIGINT NOT NULL;

ALTER TABLE organizations
    DROP INDEX idx_factory,
    DROP COLUMN factory_id;

