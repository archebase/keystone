-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

-- migrations/v2/000001_initial_schema.down.sql
-- Rollback v2 baseline schema

DROP TABLE IF EXISTS ws_client_auth_tokens;
DROP TABLE IF EXISTS bulk_runs;
DROP TABLE IF EXISTS sync_logs;
DROP TABLE IF EXISTS api_logs;
DROP TABLE IF EXISTS state_transitions;
DROP TABLE IF EXISTS qa_checks;
DROP TABLE IF EXISTS episodes;
DROP TABLE IF EXISTS tasks;
DROP TABLE IF EXISTS batches;
DROP TABLE IF EXISTS orders;
DROP TABLE IF EXISTS device_id_sequences;
DROP TABLE IF EXISTS workstations;
DROP TABLE IF EXISTS data_collectors;
DROP TABLE IF EXISTS robots;
DROP TABLE IF EXISTS robot_type_config_templates;
DROP TABLE IF EXISTS robot_types;
DROP TABLE IF EXISTS sops;
DROP TABLE IF EXISTS subscenes;
DROP TABLE IF EXISTS scenes;
DROP TABLE IF EXISTS factories;
DROP TABLE IF EXISTS organizations;
