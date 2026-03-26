-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

-- migrations/000002_version_2_schema.down.sql
-- Revert index optimizations from version 2 (remove virtual columns and restore original indexes)

-- ============================================================
-- Production Units (reverse order of creation)
-- ============================================================

DROP INDEX idx_episode_del ON episodes;
ALTER TABLE episodes DROP COLUMN _episode_unique;
CREATE INDEX episode_id ON episodes (episode_id);

DROP INDEX idx_task_del ON tasks;
ALTER TABLE tasks DROP COLUMN _task_unique;
CREATE UNIQUE INDEX task_id ON tasks (task_id);

DROP INDEX idx_name_del ON batches;
ALTER TABLE batches DROP COLUMN _name_unique;
CREATE UNIQUE INDEX batch_id ON batches (batch_id);

DROP INDEX idx_name_del ON orders;
ALTER TABLE orders DROP COLUMN _name_unique;

-- ============================================================
-- Operational Resources
-- ============================================================

DROP INDEX idx_inspector_del ON inspectors;
ALTER TABLE inspectors DROP COLUMN _inspector_unique;
CREATE UNIQUE INDEX inspector_id ON inspectors (inspector_id);

DROP INDEX idx_datacollector_del ON workstations;
ALTER TABLE workstations DROP COLUMN _collector_unique;

DROP INDEX idx_operator_del ON data_collectors;
ALTER TABLE data_collectors DROP COLUMN _operator_unique;
CREATE UNIQUE INDEX operator_id ON data_collectors (operator_id);
CREATE INDEX idx_operator_id ON data_collectors (operator_id);

DROP INDEX idx_device_del ON robots;
ALTER TABLE robots DROP COLUMN _device_unique;
CREATE UNIQUE INDEX device_id ON robots (device_id);
CREATE INDEX idx_device_id ON robots (device_id);

DROP INDEX idx_model_del ON robot_types;
ALTER TABLE robot_types DROP COLUMN _model_unique;

-- ============================================================
-- Capability & Procedure
-- ============================================================

DROP INDEX idx_name_del ON sops;
ALTER TABLE sops DROP COLUMN _name_unique;
CREATE INDEX slug ON sops (slug);
CREATE INDEX idx_slug ON sops (slug);

-- Revert: Make sops.slug NOT NULL again
ALTER TABLE sops MODIFY COLUMN slug VARCHAR(100) NOT NULL;

DROP INDEX idx_name_del ON skills;
ALTER TABLE skills DROP COLUMN _name_unique;
CREATE UNIQUE INDEX name ON skills (name);

-- ============================================================
-- Environmental Hierarchy
-- ============================================================

DROP INDEX idx_name_del ON subscenes;
ALTER TABLE subscenes DROP COLUMN _name_unique;
CREATE UNIQUE INDEX idx_scene_slug ON subscenes (scene_id, slug);

-- Revert: Make subscenes.robot_type_id NOT NULL again
ALTER TABLE subscenes MODIFY COLUMN robot_type_id BIGINT NOT NULL;

-- Revert: Make subscenes.slug NOT NULL again
ALTER TABLE subscenes MODIFY COLUMN slug VARCHAR(100) NOT NULL;

DROP INDEX idx_name_del ON scenes;
ALTER TABLE scenes DROP COLUMN _name_unique;
CREATE UNIQUE INDEX idx_org_slug ON scenes (organization_id, slug);

-- Revert: Make scenes.slug NOT NULL again
ALTER TABLE scenes MODIFY COLUMN slug VARCHAR(100) NOT NULL;

-- Revert: Make scenes.organization_id NOT NULL again
ALTER TABLE scenes MODIFY COLUMN organization_id BIGINT NOT NULL;

DROP INDEX idx_slug_del ON factories;
ALTER TABLE factories DROP COLUMN _slug_unique;
CREATE UNIQUE INDEX idx_org_slug ON factories (organization_id, slug);

DROP INDEX idx_slug_del ON organizations;
ALTER TABLE organizations DROP COLUMN _slug_unique;
CREATE UNIQUE INDEX slug ON organizations (slug);
CREATE INDEX idx_slug ON organizations (slug);
