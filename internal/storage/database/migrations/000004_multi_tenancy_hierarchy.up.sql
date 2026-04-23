-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

-- migrations/000004_multi_tenancy_hierarchy.up.sql
-- Redesign org/factory hierarchy: factory is the top-level physical boundary;
-- organizations are tenants within a factory.
--
-- Execution order matters:
--   1. Add organizations.factory_id (backfill from factories.organization_id reverse)
--   2. Drop factories.organization_id
--   3. Add data_collectors.organization_id (seed with first org per factory)
--   4. Add inspectors.organization_id (same seed)
--   5. Add workstations.organization_id (derive from data_collectors)

-- ============================================================
-- Step 1: Add factory_id to organizations
-- ============================================================

ALTER TABLE organizations
    ADD COLUMN factory_id BIGINT NOT NULL DEFAULT 0 AFTER id;

-- Backfill: each organization gets the factory that previously referenced it.
-- Before the hierarchy flip, factories.organization_id pointed from factory to org.
-- We reverse that: org.factory_id = the factory that owned it.
UPDATE organizations o
    INNER JOIN factories f ON f.organization_id = o.id AND f.deleted_at IS NULL
SET o.factory_id = f.id;

-- For any organization not yet matched (edge case: org with no factory), assign
-- factory id = 1 (the default seeded factory). Operators must correct via API.
UPDATE organizations
SET factory_id = 1
WHERE factory_id = 0;

-- Remove DEFAULT 0 constraint now that backfill is done.
ALTER TABLE organizations
    MODIFY COLUMN factory_id BIGINT NOT NULL;

-- Add index for org lookups by factory.
ALTER TABLE organizations
    ADD INDEX idx_factory (factory_id);

-- ============================================================
-- Step 2: Drop organization_id from factories
-- ============================================================

ALTER TABLE factories
    DROP INDEX idx_org,
    DROP COLUMN organization_id;

-- ============================================================
-- Step 3: Add organization_id to data_collectors
-- ============================================================

ALTER TABLE data_collectors
    ADD COLUMN organization_id BIGINT NOT NULL DEFAULT 0 AFTER id;

-- Seed: assign all existing data_collectors to the first (lowest id) organization.
-- In a fresh deployment there are no data_collectors yet, so this is a no-op.
-- In upgraded deployments, operators must re-assign via API.
UPDATE data_collectors dc
    INNER JOIN (
        SELECT MIN(id) AS org_id FROM organizations WHERE deleted_at IS NULL
    ) first_org ON TRUE
SET dc.organization_id = first_org.org_id
WHERE dc.organization_id = 0;

-- Fallback: if no organization exists at all, leave 0 (will fail FK validation
-- at application level but keeps DB consistent for empty deployments).

ALTER TABLE data_collectors
    MODIFY COLUMN organization_id BIGINT NOT NULL,
    ADD INDEX idx_organization (organization_id);

-- ============================================================
-- Step 4: Add organization_id to inspectors
-- ============================================================

ALTER TABLE inspectors
    ADD COLUMN organization_id BIGINT NOT NULL DEFAULT 0 AFTER id;

UPDATE inspectors ins
    INNER JOIN (
        SELECT MIN(id) AS org_id FROM organizations WHERE deleted_at IS NULL
    ) first_org ON TRUE
SET ins.organization_id = first_org.org_id
WHERE ins.organization_id = 0;

ALTER TABLE inspectors
    MODIFY COLUMN organization_id BIGINT NOT NULL,
    ADD INDEX idx_organization (organization_id);

-- ============================================================
-- Step 5: Add organization_id to workstations
-- ============================================================

ALTER TABLE workstations
    ADD COLUMN organization_id BIGINT NOT NULL DEFAULT 0 AFTER factory_id;

-- Derive from existing data_collector rows.
UPDATE workstations ws
    INNER JOIN data_collectors dc ON dc.id = ws.data_collector_id
SET ws.organization_id = dc.organization_id
WHERE ws.organization_id = 0;

ALTER TABLE workstations
    MODIFY COLUMN organization_id BIGINT NOT NULL,
    ADD INDEX idx_organization (organization_id);
