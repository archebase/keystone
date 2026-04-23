-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

-- Denormalize organization_id onto batches for filtering (derived from orders).
ALTER TABLE batches
    ADD COLUMN organization_id BIGINT NULL AFTER workstation_id;

UPDATE batches b
INNER JOIN orders o ON o.id = b.order_id
SET b.organization_id = o.organization_id
WHERE b.organization_id IS NULL;

-- Enforce non-null after backfill (all valid batches must have an order).
ALTER TABLE batches
    MODIFY COLUMN organization_id BIGINT NOT NULL,
    ADD INDEX idx_org (organization_id);

