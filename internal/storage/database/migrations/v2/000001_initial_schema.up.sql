-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

-- migrations/v2/000001_initial_schema.up.sql
-- V2 baseline schema for Keystone Edge

-- ============================================================
-- Environmental Hierarchy
-- ============================================================

CREATE TABLE IF NOT EXISTS workspaces (
    id BIGINT PRIMARY KEY,
    name VARCHAR(100) NOT NULL,
    description VARCHAR(200),
    source VARCHAR(32) NOT NULL,
    admins JSON NOT NULL,
    members JSON NOT NULL,
    last_synced_at TIMESTAMP NULL,
    hilbert_created_at TIMESTAMP NULL,
    hilbert_updated_at TIMESTAMP NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    deleted_at TIMESTAMP NULL,
    INDEX idx_source (source),
    INDEX idx_deleted (deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

INSERT INTO workspaces (
    id,
    name,
    description,
    source,
    admins,
    members,
    last_synced_at
) VALUES (
    0,
    'Default Workspace',
    'Local-only fallback workspace',
    'default',
    JSON_ARRAY(),
    JSON_ARRAY(),
    NULL
) ON DUPLICATE KEY UPDATE
    name = VALUES(name),
    description = VALUES(description),
    source = VALUES(source),
    admins = VALUES(admins),
    members = VALUES(members),
    last_synced_at = VALUES(last_synced_at);

CREATE TABLE IF NOT EXISTS dc_plan (
    id BIGINT PRIMARY KEY,
    workspace_id BIGINT NOT NULL,
    name VARCHAR(100) NOT NULL,
    description VARCHAR(200),
    dc_factory_id BIGINT NOT NULL,
    dc_service_provider_id BIGINT NOT NULL,
    operator VARCHAR(100) NOT NULL,
    operator_display_name VARCHAR(100),
    dc_project_id BIGINT NOT NULL,
    dc_project_name VARCHAR(100),
    dc_task_id BIGINT NOT NULL,
    dc_task_name VARCHAR(100),
    dc_device_id BIGINT NOT NULL,
    dc_device_name VARCHAR(100),
    dc_type VARCHAR(100) NOT NULL,
    dc_date DATE NOT NULL,
    target_count BIGINT NOT NULL,
    cur_count BIGINT NOT NULL DEFAULT 0,
    target_duration BIGINT NOT NULL,
    cur_duration BIGINT NOT NULL DEFAULT 0,
    created_by VARCHAR(100) NOT NULL,
    created_time TIMESTAMP NOT NULL,
    updated_by VARCHAR(100),
    updated_time TIMESTAMP NULL,
    raw_payload JSON DEFAULT NULL,
    last_synced_at TIMESTAMP NULL,
    sync_error TEXT,
    local_created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    local_updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    deleted_at TIMESTAMP NULL,
    INDEX idx_workspace (workspace_id),
    INDEX idx_name (name),
    INDEX idx_dc_type (dc_type),
    INDEX idx_operator (operator),
    INDEX idx_dc_date (dc_date),
    INDEX idx_last_synced (last_synced_at),
    INDEX idx_deleted (deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ============================================================
-- Operational Resources
-- ============================================================

CREATE TABLE IF NOT EXISTS robots (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    device_id VARCHAR(100) NOT NULL,
    device_name VARCHAR(255),
    workspace_id BIGINT NOT NULL DEFAULT 0,
    device_type_id BIGINT,
    device_type VARCHAR(255),
    asset_id VARCHAR(100),
    status ENUM('active', 'maintenance', 'retired') DEFAULT 'active',
    auth_epoch BIGINT NOT NULL DEFAULT 1,
    metadata JSON DEFAULT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    deleted_at TIMESTAMP NULL,
    _device_unique VARCHAR(200) GENERATED ALWAYS AS (CONCAT(IFNULL(device_id, ''), '|', IFNULL(deleted_at, ''))) STORED,
    _device_name_unique VARCHAR(255) GENERATED ALWAYS AS (
        CASE
            WHEN deleted_at IS NULL AND device_name IS NOT NULL AND device_name <> ''
            THEN device_name
            ELSE NULL
        END
    ) STORED,
    _asset_unique VARCHAR(100) GENERATED ALWAYS AS (
        CASE
            WHEN deleted_at IS NULL AND asset_id IS NOT NULL AND asset_id <> ''
            THEN asset_id
            ELSE NULL
        END
    ) STORED,
    UNIQUE INDEX idx_device_del (_device_unique),
    UNIQUE INDEX idx_device_name_active_unique (_device_name_unique),
    UNIQUE INDEX idx_asset_active_unique (_asset_unique),
    INDEX idx_workspace (workspace_id),
    INDEX idx_device_type_id (device_type_id),
    INDEX idx_status (status),
    INDEX idx_deleted (deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS data_collectors (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    name VARCHAR(255) NOT NULL,
    operator_id VARCHAR(100) NOT NULL,
    email VARCHAR(255),
    password_hash VARCHAR(255) NULL COMMENT 'Bcrypt hash for password login',
    last_login_at TIMESTAMP NULL COMMENT 'Last successful login time',
    certification VARCHAR(100),
    status ENUM('active', 'inactive', 'on_leave') DEFAULT 'active',
    metadata JSON DEFAULT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    deleted_at TIMESTAMP NULL,
    _operator_unique VARCHAR(200) GENERATED ALWAYS AS (CONCAT(IFNULL(operator_id, ''), '|', IFNULL(deleted_at, ''))) STORED,
    UNIQUE INDEX idx_operator_del (_operator_unique),
    INDEX idx_status (status),
    INDEX idx_deleted (deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS workstations (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    robot_id BIGINT NOT NULL,
    robot_name VARCHAR(255) COMMENT 'Denormalized: avoids join to robots',
    robot_serial VARCHAR(100) COMMENT 'Denormalized: avoids join to robots',
    data_collector_id BIGINT NOT NULL,
    collector_name VARCHAR(255) COMMENT 'Denormalized: avoids join to data_collectors',
    collector_operator_id VARCHAR(100) COMMENT 'Denormalized: avoids join to data_collectors',
    workspace_id BIGINT NOT NULL DEFAULT 0,
    name VARCHAR(255),
    status ENUM('active', 'inactive', 'break', 'offline') DEFAULT 'active',
    metadata JSON DEFAULT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    deleted_at TIMESTAMP NULL,
    is_current BOOLEAN NOT NULL DEFAULT TRUE COMMENT 'Current binding version visible for new work',
    superseded_at TIMESTAMP NULL COMMENT 'When this binding was replaced by a newer workstation row',
    superseded_by BIGINT NULL COMMENT 'Newer workstation id that replaced this binding',
    _current_collector_workspace_unique VARCHAR(240) GENERATED ALWAYS AS (
        IF(
            is_current AND deleted_at IS NULL,
            CONCAT(CAST(data_collector_id AS CHAR), '|', CAST(workspace_id AS CHAR)),
            NULL
        )
    ) STORED,
    _current_robot_unique VARCHAR(200) GENERATED ALWAYS AS (
        IF(is_current AND deleted_at IS NULL, CAST(robot_id AS CHAR), NULL)
    ) STORED,
    UNIQUE INDEX idx_current_collector_workspace (_current_collector_workspace_unique),
    UNIQUE INDEX idx_current_robot (_current_robot_unique),
    INDEX idx_robot (robot_id),
    INDEX idx_collector (data_collector_id),
    INDEX idx_workspace (workspace_id),
    INDEX idx_status (status),
    INDEX idx_current (is_current),
    INDEX idx_superseded_by (superseded_by),
    INDEX idx_deleted (deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ============================================================
-- Production Units
-- ============================================================

CREATE TABLE IF NOT EXISTS orders (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    organization_id BIGINT NOT NULL,
    name VARCHAR(255) NOT NULL,
    target_count INT NOT NULL,
    priority ENUM('low', 'normal', 'high', 'urgent') DEFAULT 'normal',
    status ENUM('created', 'in_progress', 'paused', 'completed', 'cancelled') DEFAULT 'created',
    deadline TIMESTAMP NULL,
    metadata JSON DEFAULT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    deleted_at TIMESTAMP NULL,
    _name_unique VARCHAR(600) GENERATED ALWAYS AS (CONCAT(IFNULL(organization_id, ''), '|', IFNULL(name, ''), '|', IFNULL(deleted_at, ''))) STORED,
    UNIQUE INDEX idx_name_del (_name_unique),
    INDEX idx_org (organization_id),
    INDEX idx_status (status),
    INDEX idx_priority (priority),
    INDEX idx_created (created_at),
    INDEX idx_deleted (deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS batches (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    batch_id VARCHAR(100) NOT NULL,
    order_id BIGINT NOT NULL,
    workstation_id BIGINT NOT NULL,
    organization_id BIGINT NOT NULL DEFAULT 0,
    name VARCHAR(255) NULL COMMENT 'Human-readable name',
    notes TEXT,
    status ENUM('pending', 'active', 'completed', 'cancelled', 'recalled') DEFAULT 'pending',
    episode_count INT DEFAULT 0,
    started_at TIMESTAMP NULL,
    ended_at TIMESTAMP NULL,
    metadata JSON DEFAULT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    deleted_at TIMESTAMP NULL,
    _name_unique VARCHAR(600) GENERATED ALWAYS AS (CONCAT(IFNULL(order_id, ''), '|', IFNULL(batch_id, ''), '|', IFNULL(deleted_at, ''))) STORED,
    UNIQUE INDEX idx_name_del (_name_unique),
    INDEX idx_batch_id (batch_id),
    INDEX idx_order (order_id),
    INDEX idx_workstation (workstation_id),
    INDEX idx_org (organization_id),
    INDEX idx_status (status),
    INDEX idx_started (started_at),
    INDEX idx_deleted (deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS tasks (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    task_id VARCHAR(100) NOT NULL COMMENT 'Human-readable task ID',
    batch_id BIGINT NULL,
    order_id BIGINT NULL,
    workstation_id BIGINT,
    batch_name VARCHAR(255) COMMENT 'Denormalized: batch name for display',
    organization_id BIGINT COMMENT 'Workspace ownership',
    dc_plan_id BIGINT NULL,
    local_dc_plan_id BIGINT NULL,
    status ENUM('pending', 'ready', 'in_progress', 'uploading', 'completed', 'failed', 'cancelled') DEFAULT 'pending',
    version INT DEFAULT 0 COMMENT 'Optimistic locking version',
    assigned_at TIMESTAMP NULL,
    ready_at TIMESTAMP NULL,
    started_at TIMESTAMP NULL,
    completed_at TIMESTAMP NULL,
    episode_id BIGINT NULL,
    error_message TEXT,
    metadata JSON DEFAULT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    deleted_at TIMESTAMP NULL,
    _task_unique VARCHAR(200) GENERATED ALWAYS AS (CONCAT(IFNULL(task_id, ''), '|', IFNULL(deleted_at, ''))) STORED,
    UNIQUE INDEX idx_task_del (_task_unique),
    INDEX idx_task_id (task_id),
    INDEX idx_batch (batch_id),
    INDEX idx_order (order_id),
    INDEX idx_workstation (workstation_id),
    INDEX idx_organization (organization_id),
    INDEX idx_dc_plan (dc_plan_id),
    INDEX idx_local_dc_plan (local_dc_plan_id),
    INDEX idx_tasks_order_status_del (order_id, status, deleted_at),
    INDEX idx_status (status),
    INDEX idx_assigned (assigned_at),
    INDEX idx_created (created_at),
    INDEX idx_episode (episode_id),
    INDEX idx_deleted (deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS episodes (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    episode_id VARCHAR(100) NOT NULL COMMENT 'Human-readable episode ID',
    task_id BIGINT NOT NULL,
    batch_id BIGINT NULL COMMENT 'Legacy observation field',
    order_id BIGINT NULL COMMENT 'Legacy observation field',
    workstation_id BIGINT COMMENT 'Denormalized: from tasks.workstation_id',
    organization_id BIGINT COMMENT 'Denormalized: from tasks.organization_id',
    dc_plan_id BIGINT NULL,
    local_dc_plan_id BIGINT NULL,
    mcap_path VARCHAR(1024) NOT NULL,
    sidecar_path VARCHAR(1024) NOT NULL,
    checksum VARCHAR(128),
    file_size_bytes BIGINT,
    duration_sec DECIMAL(10, 2),
    qa_status ENUM('pending_qa', 'qa_running', 'approved', 'failed', 'manual_review_failed') DEFAULT 'pending_qa',
    qa_score DECIMAL(4, 3) COMMENT '0.000 to 1.000',
    auto_approved BOOLEAN DEFAULT FALSE,
    cloud_synced BOOLEAN DEFAULT FALSE,
    cloud_synced_at TIMESTAMP NULL,
    cloud_mcap_path VARCHAR(1024),
    cloud_sidecar_path VARCHAR(1024),
    cloud_processed BOOLEAN DEFAULT FALSE,
    cloud_processed_at TIMESTAMP NULL,
    dataset_id VARCHAR(255),
    labels JSON COMMENT 'Array of labels e.g. ["recalled_batch", "sensor_issue"]',
    quality_flag TEXT COMMENT 'Human-readable quality warning for researchers',
    metadata JSON DEFAULT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    deleted_at TIMESTAMP NULL,
    _episode_unique VARCHAR(200) GENERATED ALWAYS AS (CONCAT(IFNULL(episode_id, ''), '|', IFNULL(deleted_at, ''))) STORED,
    UNIQUE INDEX idx_episode_del (_episode_unique),
    INDEX idx_episode_id (episode_id),
    INDEX idx_task (task_id),
    INDEX idx_batch (batch_id),
    INDEX idx_order (order_id),
    INDEX idx_workstation (workstation_id),
    INDEX idx_organization (organization_id),
    INDEX idx_dc_plan (dc_plan_id),
    INDEX idx_local_dc_plan (local_dc_plan_id),
    INDEX idx_qa_status (qa_status),
    INDEX idx_auto_approved (auto_approved),
    INDEX idx_cloud_synced (cloud_synced, cloud_processed),
    INDEX idx_created (created_at),
    INDEX idx_deleted (deleted_at),
    INDEX idx_qa_queue (qa_status, qa_score, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ============================================================
-- Quality Assurance
-- ============================================================

CREATE TABLE IF NOT EXISTS qa_checks (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    episode_id BIGINT NOT NULL,
    check_name VARCHAR(100) NOT NULL,
    passed BOOLEAN NOT NULL,
    score DECIMAL(4, 3) NOT NULL COMMENT '0.000 to 1.000',
    weight DECIMAL(4, 3) NOT NULL DEFAULT 1.000,
    details TEXT,
    check_metadata JSON,
    checked_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    INDEX idx_episode (episode_id),
    INDEX idx_name (check_name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ============================================================
-- Audit & Monitoring
-- ============================================================

CREATE TABLE IF NOT EXISTS state_transitions (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    entity_type VARCHAR(50) NOT NULL COMMENT 'task, episode, order, workstation, robot',
    entity_id BIGINT NOT NULL,
    from_state VARCHAR(50),
    to_state VARCHAR(50) NOT NULL,
    triggered_by VARCHAR(100) NOT NULL COMMENT 'user, axon_callback, dagster_job, api',
    triggered_by_id VARCHAR(255),
    transition_metadata JSON,
    occurred_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    INDEX idx_entity (entity_type, entity_id),
    INDEX idx_occurred (occurred_at),
    INDEX idx_triggered_by (triggered_by)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS api_logs (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    request_id VARCHAR(100) NOT NULL UNIQUE,
    method VARCHAR(10) NOT NULL,
    path VARCHAR(500) NOT NULL,
    status_code INT NOT NULL,
    response_time_ms INT,
    user_id VARCHAR(100),
    user_role VARCHAR(50),
    ip_address VARCHAR(50),
    user_agent TEXT,
    error_message TEXT,
    occurred_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    INDEX idx_occurred (occurred_at),
    INDEX idx_status (status_code),
    INDEX idx_user (user_id),
    INDEX idx_path (path)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS sync_logs (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    episode_id BIGINT NOT NULL,
    source_path VARCHAR(1024),
    destination_path VARCHAR(1024),
    status ENUM('pending', 'in_progress', 'completed', 'failed') DEFAULT 'pending',
    bytes_transferred BIGINT,
    duration_sec INT,
    error_message TEXT,
    attempt_count INT NOT NULL DEFAULT 0,
    next_retry_at TIMESTAMP NULL,
    started_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    completed_at TIMESTAMP NULL,
    INDEX idx_episode (episode_id),
    INDEX idx_status (status),
    INDEX idx_started (started_at),
    INDEX idx_sync_retry (status, next_retry_at),
    INDEX idx_sync_episode_status (episode_id, status),
    INDEX idx_sync_episode_latest (episode_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS bulk_runs (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    run_id VARCHAR(64) NOT NULL UNIQUE,
    action VARCHAR(32) NOT NULL,
    status VARCHAR(32) NOT NULL,
    total_count BIGINT NOT NULL DEFAULT 0,
    processed_count BIGINT NOT NULL DEFAULT 0,
    passed_count BIGINT NOT NULL DEFAULT 0,
    qa_failed_count BIGINT NOT NULL DEFAULT 0,
    processing_failed_count BIGINT NOT NULL DEFAULT 0,
    skipped_count BIGINT NOT NULL DEFAULT 0,
    error_message TEXT,
    started_at TIMESTAMP NULL,
    finished_at TIMESTAMP NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    INDEX idx_bulk_runs_action_status (action, status),
    INDEX idx_bulk_runs_updated_at (updated_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS ws_client_auth_tokens (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    robot_id BIGINT NOT NULL,
    token_hash CHAR(64) NOT NULL,
    token_version ENUM('kda_v1') NOT NULL DEFAULT 'kda_v1',
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    last_rotated_at TIMESTAMP NULL,
    last_used_at TIMESTAMP NULL,
    sdk_initialized_at TIMESTAMP NULL,
    recovery_requested_at TIMESTAMP NULL,
    recovery_stage ENUM('none', 'authorized', 'epoch_incremented', 'deleted', 'generated', 'completed') NOT NULL DEFAULT 'none',
    recovery_completed_at TIMESTAMP NULL,
    revoked_at TIMESTAMP NULL,
    _active_robot_id BIGINT GENERATED ALWAYS AS (
        CASE WHEN revoked_at IS NULL THEN robot_id ELSE NULL END
    ) STORED,
    UNIQUE INDEX idx_ws_client_token_hash (token_hash),
    UNIQUE INDEX idx_ws_client_active_robot (_active_robot_id),
    INDEX idx_ws_client_robot_active (robot_id, revoked_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
