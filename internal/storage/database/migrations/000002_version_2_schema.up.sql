-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

-- migrations/000002_version_2_schema.up.sql
-- Fix unique indexes with NULL values by using STORED virtual columns

-- ============================================================
-- Environmental Hierarchy
-- ============================================================

-- organizations: slug + deleted_at
ALTER TABLE organizations ADD COLUMN _slug_unique VARCHAR(200) GENERATED ALWAYS AS (CONCAT(IFNULL(slug, ''), '|', IFNULL(deleted_at, ''))) STORED;
DROP INDEX slug ON organizations;
DROP INDEX idx_slug ON organizations;
CREATE UNIQUE INDEX idx_slug_del ON organizations (_slug_unique);

-- factories: slug + deleted_at
ALTER TABLE factories ADD COLUMN _slug_unique VARCHAR(200) GENERATED ALWAYS AS (CONCAT(IFNULL(slug, ''), '|', IFNULL(deleted_at, ''))) STORED;
DROP INDEX idx_org_slug ON factories;
CREATE UNIQUE INDEX idx_slug_del ON factories (_slug_unique);

-- scenes: name + deleted_at (organization_id is now nullable, so we include it in the unique key)
ALTER TABLE scenes ADD COLUMN _name_unique VARCHAR(400) GENERATED ALWAYS AS (CONCAT(IFNULL(organization_id, ''), '|', IFNULL(name, ''), '|', IFNULL(deleted_at, ''))) STORED;
DROP INDEX idx_org_slug ON scenes;
CREATE UNIQUE INDEX idx_name_del ON scenes (_name_unique);

-- Allow scenes.organization_id to be NULL
ALTER TABLE scenes MODIFY COLUMN organization_id BIGINT NULL;

-- Allow scenes.slug to be NULL
ALTER TABLE scenes MODIFY COLUMN slug VARCHAR(100) NULL;

-- subscenes: name + deleted_at
ALTER TABLE subscenes ADD COLUMN _name_unique VARCHAR(400) GENERATED ALWAYS AS (CONCAT(IFNULL(scene_id, ''), '|', IFNULL(name, ''), '|', IFNULL(deleted_at, ''))) STORED;
DROP INDEX idx_scene_slug ON subscenes;
CREATE UNIQUE INDEX idx_name_del ON subscenes (_name_unique);

-- Allow subscenes.slug to be NULL
ALTER TABLE subscenes MODIFY COLUMN slug VARCHAR(100) NULL;

-- Allow subscenes.robot_type_id to be NULL
ALTER TABLE subscenes MODIFY COLUMN robot_type_id BIGINT NULL;

-- ============================================================
-- Capability & Procedure
-- ============================================================

-- skills: name + version + deleted_at
ALTER TABLE skills ADD COLUMN _name_unique VARCHAR(300) GENERATED ALWAYS AS (CONCAT(IFNULL(name, ''), '|', IFNULL(version, ''), '|', IFNULL(deleted_at, ''))) STORED;
DROP INDEX name ON skills;
CREATE UNIQUE INDEX idx_name_del ON skills (_name_unique);

-- sops: name + deleted_at
ALTER TABLE sops ADD COLUMN _name_unique VARCHAR(300) GENERATED ALWAYS AS (CONCAT(IFNULL(name, ''), '|', IFNULL(deleted_at, ''))) STORED;
DROP INDEX slug ON sops;
DROP INDEX idx_slug ON sops;
CREATE UNIQUE INDEX idx_name_del ON sops (_name_unique);

-- Allow sops.slug to be NULL
ALTER TABLE sops MODIFY COLUMN slug VARCHAR(100) NULL;

-- ============================================================
-- Operational Resources
-- ============================================================

-- robot_types: model + deleted_at
ALTER TABLE robot_types ADD COLUMN _model_unique VARCHAR(300) GENERATED ALWAYS AS (CONCAT(IFNULL(model, ''), '|', IFNULL(deleted_at, ''))) STORED;
CREATE UNIQUE INDEX idx_model_del ON robot_types (_model_unique);

-- robots: device_id + deleted_at
ALTER TABLE robots ADD COLUMN _device_unique VARCHAR(200) GENERATED ALWAYS AS (CONCAT(IFNULL(device_id, ''), '|', IFNULL(deleted_at, ''))) STORED;
DROP INDEX device_id ON robots;
DROP INDEX idx_device_id ON robots;
CREATE UNIQUE INDEX idx_device_del ON robots (_device_unique);

-- data_collectors: operator_id + deleted_at
ALTER TABLE data_collectors ADD COLUMN _operator_unique VARCHAR(200) GENERATED ALWAYS AS (CONCAT(IFNULL(operator_id, ''), '|', IFNULL(deleted_at, ''))) STORED;
DROP INDEX operator_id ON data_collectors;
DROP INDEX idx_operator_id ON data_collectors;
CREATE UNIQUE INDEX idx_operator_del ON data_collectors (_operator_unique);

-- workstations: data_collector_id + deleted_at
ALTER TABLE workstations ADD COLUMN _collector_unique VARCHAR(200) GENERATED ALWAYS AS (CONCAT(IFNULL(data_collector_id, ''), '|', IFNULL(deleted_at, ''))) STORED;
CREATE UNIQUE INDEX idx_datacollector_del ON workstations (_collector_unique);

-- inspectors: inspector_id + deleted_at
ALTER TABLE inspectors ADD COLUMN _inspector_unique VARCHAR(200) GENERATED ALWAYS AS (CONCAT(IFNULL(inspector_id, ''), '|', IFNULL(deleted_at, ''))) STORED;
DROP INDEX inspector_id ON inspectors;
CREATE UNIQUE INDEX idx_inspector_del ON inspectors (_inspector_unique);

-- ============================================================
-- Production Units
-- ============================================================

-- orders: name + deleted_at (include organization_id and scene_id for uniqueness)
ALTER TABLE orders ADD COLUMN _name_unique VARCHAR(600) GENERATED ALWAYS AS (CONCAT(IFNULL(organization_id, ''), '|', IFNULL(scene_id, ''), '|', IFNULL(name, ''), '|', IFNULL(deleted_at, ''))) STORED;
CREATE UNIQUE INDEX idx_name_del ON orders (_name_unique);

-- batches: name + deleted_at (include batch_id as unique identifier)
ALTER TABLE batches ADD COLUMN _name_unique VARCHAR(600) GENERATED ALWAYS AS (CONCAT(IFNULL(order_id, ''), '|', IFNULL(name, ''), '|', IFNULL(deleted_at, ''))) STORED;
DROP INDEX batch_id ON batches;
CREATE UNIQUE INDEX idx_name_del ON batches (_name_unique);

-- tasks: task_id + deleted_at
ALTER TABLE tasks ADD COLUMN _task_unique VARCHAR(200) GENERATED ALWAYS AS (CONCAT(IFNULL(task_id, ''), '|', IFNULL(deleted_at, ''))) STORED;
DROP INDEX task_id ON tasks;
CREATE UNIQUE INDEX idx_task_del ON tasks (_task_unique);

-- episodes: episode_id + deleted_at
ALTER TABLE episodes ADD COLUMN _episode_unique VARCHAR(200) GENERATED ALWAYS AS (CONCAT(IFNULL(episode_id, ''), '|', IFNULL(deleted_at, ''))) STORED;
DROP INDEX episode_id ON episodes;
CREATE UNIQUE INDEX idx_episode_del ON episodes (_episode_unique);
