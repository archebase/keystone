-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

-- migrations/000004_org_adjust.up.sql
--
-- Consolidated migration for feat/org-adjust (squashed for single PR):
-- - Multi-tenancy hierarchy redesign (orgs belong to factories; workstations/collectors/inspectors reference org)
-- - Unique constraints for factories.name and organizations.name (non-deleted)
-- - Denormalize batches.organization_id for filtering

-- ============================================================
-- Part A: Multi-tenancy hierarchy redesign
-- ============================================================

-- Step 1: Add factory_id to organizations
ALTER TABLE organizations
    ADD COLUMN factory_id BIGINT NOT NULL DEFAULT 0 AFTER id;

UPDATE organizations o
    INNER JOIN factories f ON f.organization_id = o.id AND f.deleted_at IS NULL
SET o.factory_id = f.id;

UPDATE organizations
SET factory_id = 1
WHERE factory_id = 0;

ALTER TABLE organizations
    MODIFY COLUMN factory_id BIGINT NOT NULL,
    ADD INDEX idx_factory (factory_id);

-- Step 2: Drop organization_id from factories
ALTER TABLE factories
    DROP INDEX idx_org,
    DROP COLUMN organization_id;

    -- Step 3: Add organization_id to data_collectors
    -- NOTE: Development-only migration. No historical backfill to avoid cross-tenant corruption.
    -- If you need existing data, map collectors to correct organizations manually before running.
    ALTER TABLE data_collectors
        ADD COLUMN organization_id BIGINT NOT NULL DEFAULT 0 AFTER id;
    
    -- Intentionally no backfill: MIN(id) would assign all collectors to a single org and
    -- silently corrupt tenant boundaries in multi-factory deployments.
    -- Remove this default and assign correct organization_id in seed/test data.
    
    ALTER TABLE data_collectors
        MODIFY COLUMN organization_id BIGINT NOT NULL,
        ADD INDEX idx_organization (organization_id);

    -- Step 4: Add organization_id to inspectors
    -- NOTE: Development-only migration. No historical backfill to avoid cross-tenant corruption.
    -- If you need existing data, map inspectors to correct organizations manually before running.
    ALTER TABLE inspectors
        ADD COLUMN organization_id BIGINT NOT NULL DEFAULT 0 AFTER id;
    
    -- Intentionally no backfill: MIN(id) would assign all inspectors to a single org and
    -- silently corrupt tenant boundaries in multi-factory deployments.
    -- Remove this default and assign correct organization_id in seed/test data.
    
    ALTER TABLE inspectors
        MODIFY COLUMN organization_id BIGINT NOT NULL,
        ADD INDEX idx_organization (organization_id);

    -- Step 5: Add organization_id to workstations
    -- NOTE: Development-only migration. No historical backfill to avoid propagating
    -- the zeroed organization_id from data_collectors (Steps 3-4 leave those as 0).
    -- Assign correct organization_id in seed/test data after migration.
    ALTER TABLE workstations
        ADD COLUMN organization_id BIGINT NOT NULL DEFAULT 0 AFTER factory_id;

    -- Intentionally no backfill: data_collectors.organization_id is 0 at this point
    -- (Steps 3-4 removed the backfill), so deriving from it would stamp 0 on every
    -- workstation row, which is equivalent to no mapping and silently invalidates
    -- the tenant isolation checks in CreateBatch/AdjustBatchTasks.

    ALTER TABLE workstations
        MODIFY COLUMN organization_id BIGINT NOT NULL,
        ADD INDEX idx_organization (organization_id);

-- ============================================================
-- Part B: Unique name constraints (non-deleted)
-- ============================================================

ALTER TABLE factories
    ADD COLUMN _name_unique VARCHAR(400) GENERATED ALWAYS AS (CONCAT(IFNULL(name, ''), '|', IFNULL(deleted_at, ''))) STORED,
    ADD UNIQUE INDEX idx_name_del (_name_unique);

ALTER TABLE organizations
    ADD COLUMN _name_unique VARCHAR(400) GENERATED ALWAYS AS (CONCAT(IFNULL(name, ''), '|', IFNULL(deleted_at, ''))) STORED,
    ADD UNIQUE INDEX idx_name_del (_name_unique);

-- ============================================================
-- Part C: batches.organization_id (denormalized from orders)
-- ============================================================

ALTER TABLE batches
    ADD COLUMN organization_id BIGINT NULL AFTER workstation_id;

UPDATE batches b
INNER JOIN orders o ON o.id = b.order_id
SET b.organization_id = o.organization_id
WHERE b.organization_id IS NULL;

ALTER TABLE batches
    MODIFY COLUMN organization_id BIGINT NOT NULL,
    ADD INDEX idx_org (organization_id);

