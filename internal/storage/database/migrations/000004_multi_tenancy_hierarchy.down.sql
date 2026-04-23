-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

-- migrations/000004_multi_tenancy_hierarchy.down.sql
-- Reverses the multi-tenancy hierarchy redesign.

-- ============================================================
-- Step 5 reverse: drop workstations.organization_id
-- ============================================================

ALTER TABLE workstations
    DROP INDEX idx_organization,
    DROP COLUMN organization_id;

-- ============================================================
-- Step 4 reverse: drop inspectors.organization_id
-- ============================================================

ALTER TABLE inspectors
    DROP INDEX idx_organization,
    DROP COLUMN organization_id;

-- ============================================================
-- Step 3 reverse: drop data_collectors.organization_id
-- ============================================================

ALTER TABLE data_collectors
    DROP INDEX idx_organization,
    DROP COLUMN organization_id;

-- ============================================================
-- Step 2 reverse: restore factories.organization_id
-- ============================================================

ALTER TABLE factories
    ADD COLUMN organization_id BIGINT NOT NULL DEFAULT 0 AFTER id,
    ADD INDEX idx_org (organization_id);

-- Backfill from organizations.factory_id (reverse of the up migration).
UPDATE factories f
    INNER JOIN organizations o ON o.factory_id = f.id AND o.deleted_at IS NULL
SET f.organization_id = o.id;

ALTER TABLE factories
    MODIFY COLUMN organization_id BIGINT NOT NULL;

-- ============================================================
-- Step 1 reverse: drop organizations.factory_id
-- ============================================================

ALTER TABLE organizations
    DROP INDEX idx_factory,
    DROP COLUMN factory_id;
