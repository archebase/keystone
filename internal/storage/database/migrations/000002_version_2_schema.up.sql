-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

-- migrations/000002_version_2_schema.up.sql
-- Optimize indexes for better performance

-- ============================================================
-- Environmental Hierarchy
-- ============================================================

DROP INDEX slug ON organizations;
DROP INDEX idx_slug ON organizations;
CREATE UNIQUE INDEX idx_slug_del ON organizations (slug, deleted_at);

DROP INDEX idx_org_slug ON factories;
CREATE UNIQUE INDEX idx_org_slug_del ON factories (organization_id, slug, deleted_at);

DROP INDEX idx_org_slug ON scenes;
CREATE UNIQUE INDEX idx_org_factory_slug_del ON scenes (organization_id, factory_id, slug, deleted_at);

DROP INDEX idx_scene_slug ON subscenes;
ALTER TABLE subscenes 
ADD COLUMN organization_id BIGINT NOT NULL AFTER scene_id,
ADD COLUMN factory_id BIGINT NOT NULL AFTER organization_id;
CREATE UNIQUE INDEX idx_org_factory_scene_slug_del ON subscenes (organization_id, factory_id, scene_id, slug, deleted_at);

-- ============================================================
-- Capability & Procedure
-- ============================================================

DROP INDEX name ON skills;
CREATE UNIQUE INDEX idx_name_del ON skills (name, deleted_at);

DROP INDEX slug ON sops;
DROP INDEX idx_slug ON sops;
CREATE UNIQUE INDEX idx_slug_del ON sops (slug, deleted_at);

-- ============================================================
-- Operational Resources
-- ============================================================

CREATE UNIQUE INDEX idx_model_del ON robot_types (model, deleted_at);

DROP INDEX device_id ON robots;
DROP INDEX idx_device_id ON robots;
CREATE UNIQUE INDEX idx_robottype_device_del ON robots (robot_type_id, device_id, deleted_at);

DROP INDEX operator_id ON data_collectors;
DROP INDEX idx_operator_id ON data_collectors;
CREATE UNIQUE INDEX idx_operator_del ON data_collectors (operator_id, deleted_at);

CREATE UNIQUE INDEX idx_robot_datacollector_del ON workstations (robot_id, data_collector_id, deleted_at);

DROP INDEX inspector_id ON inspectors;
CREATE UNIQUE INDEX idx_inspector_del ON inspectors (inspector_id, deleted_at);

-- ============================================================
-- Production Units
-- ============================================================

CREATE UNIQUE INDEX idx_org_name_del ON orders (organization_id, name, deleted_at);

DROP INDEX batch_id ON batches;
CREATE UNIQUE INDEX idx_order_name_del ON batches (order_id, name, deleted_at);

DROP INDEX task_id ON tasks;
CREATE UNIQUE INDEX idx_order_batch_task_del ON tasks (order_id, batch_id, task_id, deleted_at);

DROP INDEX episode_id ON episodes;
CREATE UNIQUE INDEX idx_order_batch_task_episode_del ON episodes (order_id, batch_id, task_id, episode_id, deleted_at);






