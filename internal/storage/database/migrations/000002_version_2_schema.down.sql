-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

-- migrations/000002_version_2_schema.down.sql
-- Revert index optimizations from version 2

-- ============================================================
-- Production Units (reverse order)
-- ============================================================

DROP INDEX idx_order_batch_task_episode_del ON episodes;
CREATE INDEX idx_episode_id ON episodes (episode_id);

DROP INDEX idx_order_batch_task_del ON tasks;
CREATE INDEX idx_task_id ON tasks (task_id);

DROP INDEX idx_order_name_del ON batches;
CREATE INDEX idx_batch_id ON batches (batch_id);

DROP INDEX idx_org_name_del ON orders;

-- ============================================================
-- Operational Resources
-- ============================================================

DROP INDEX idx_inspector_del ON inspectors;

DROP INDEX idx_robot_datacollector_del ON workstations;

DROP INDEX idx_operator_del ON data_collectors;
CREATE UNIQUE INDEX idx_operator_id ON data_collectors (operator_id);

DROP INDEX idx_robottype_device_del ON robots;
CREATE UNIQUE INDEX idx_device_id ON robots (device_id);

-- robot_types has no original index to restore (none existed before)

-- ============================================================
-- Capability & Procedure
-- ============================================================

DROP INDEX idx_slug_del ON sops;
CREATE INDEX idx_slug ON sops (slug);

DROP INDEX idx_name_del ON skills;
CREATE INDEX idx_name ON skills (name);

-- ============================================================
-- Environmental Hierarchy
-- ============================================================

DROP INDEX idx_org_factory_scene_slug_del ON subscenes;
ALTER TABLE subscenes 
    DROP COLUMN factory_id,
    DROP COLUMN organization_id;
CREATE UNIQUE INDEX idx_scene_slug ON subscenes (scene_id, slug);

DROP INDEX idx_org_factory_slug_del ON scenes;
CREATE UNIQUE INDEX idx_org_slug ON scenes (organization_id, slug);

DROP INDEX idx_org_slug_del ON factories;
CREATE UNIQUE INDEX idx_org_slug ON factories (organization_id, slug);

DROP INDEX idx_slug_del ON organizations;
CREATE INDEX idx_slug ON organizations (slug);
