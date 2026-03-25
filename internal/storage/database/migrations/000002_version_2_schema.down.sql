-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

-- migrations/000002_version_2_schema.down.sql
-- Revert index optimizations from version 2

-- ============================================================
-- Production Units (reverse order of creation)
-- ============================================================

DROP INDEX idx_episode_del ON episodes;
CREATE INDEX episode_id ON episodes (episode_id);

DROP INDEX idx_task_del ON tasks;
CREATE INDEX task_id ON tasks (task_id);

DROP INDEX idx_name_del ON batches;
CREATE INDEX batch_id ON batches (batch_id);

DROP INDEX idx_name_del ON orders;

-- ============================================================
-- Operational Resources
-- ============================================================

DROP INDEX idx_inspector_del ON inspectors;
CREATE INDEX inspector_id ON inspectors (inspector_id);

DROP INDEX idx_datacollector_del ON workstations;

DROP INDEX idx_operator_del ON data_collectors;
CREATE INDEX operator_id ON data_collectors (operator_id);
CREATE INDEX idx_operator_id ON data_collectors (operator_id);

DROP INDEX idx_device_del ON robots;
CREATE INDEX device_id ON robots (device_id);
CREATE INDEX idx_device_id ON robots (device_id);

DROP INDEX idx_model_del ON robot_types;

-- ============================================================
-- Capability & Procedure
-- ============================================================

DROP INDEX idx_name_del ON sops;
CREATE INDEX slug ON sops (slug);
CREATE INDEX idx_slug ON sops (slug);

DROP INDEX idx_name_del ON skills;
CREATE INDEX name ON skills (name);

-- ============================================================
-- Environmental Hierarchy
-- ============================================================

DROP INDEX idx_name_del ON subscenes;
CREATE INDEX idx_scene_slug ON subscenes (scene_id, slug);

DROP INDEX idx_name_del ON scenes;
CREATE UNIQUE INDEX idx_org_slug ON scenes (organization_id, slug);

DROP INDEX idx_slug_del ON factories;
CREATE UNIQUE INDEX idx_org_slug ON factories (organization_id, slug);

DROP INDEX idx_slug_del ON organizations;
CREATE INDEX slug ON organizations (slug);
CREATE INDEX idx_slug ON organizations (slug);
