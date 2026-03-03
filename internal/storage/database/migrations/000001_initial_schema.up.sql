-- migrations/000001_initial_schema.up.sql
-- Initial schema for Keystone Edge

-- ============================================================
-- Environmental Hierarchy
-- ============================================================

CREATE TABLE IF NOT EXISTS organizations (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    name VARCHAR(255) NOT NULL,
    slug VARCHAR(100) NOT NULL UNIQUE,
    description TEXT,
    settings JSON DEFAULT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    deleted_at TIMESTAMP NULL,
    UNIQUE INDEX idx_slug (slug),
    INDEX idx_deleted (deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS factories (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    organization_id BIGINT NOT NULL,
    name VARCHAR(255) NOT NULL,
    slug VARCHAR(100) NOT NULL,
    location VARCHAR(255),
    timezone VARCHAR(50) DEFAULT 'UTC',
    settings JSON DEFAULT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    deleted_at TIMESTAMP NULL,
    UNIQUE INDEX idx_org_slug (organization_id, slug),
    INDEX idx_org (organization_id),
    INDEX idx_deleted (deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS scenes (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    organization_id BIGINT NOT NULL,
    factory_id BIGINT NOT NULL,
    name VARCHAR(255) NOT NULL,
    slug VARCHAR(100) NOT NULL,
    description TEXT,
    initial_scene_layout_template TEXT,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    deleted_at TIMESTAMP NULL,
    UNIQUE INDEX idx_org_slug (organization_id, slug),
    INDEX idx_factory (factory_id),
    INDEX idx_deleted (deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS subscenes (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    scene_id BIGINT NOT NULL,
    name VARCHAR(255) NOT NULL,
    slug VARCHAR(100) NOT NULL,
    description TEXT,
    initial_scene_layout TEXT,
    robot_type_id BIGINT NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    deleted_at TIMESTAMP NULL,
    UNIQUE INDEX idx_scene_slug (scene_id, slug),
    INDEX idx_scene (scene_id),
    INDEX idx_robot_type (robot_type_id),
    INDEX idx_deleted (deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ============================================================
-- Capability & Procedure
-- ============================================================

CREATE TABLE IF NOT EXISTS skills (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    name VARCHAR(100) NOT NULL UNIQUE,
    display_name VARCHAR(255) NOT NULL,
    description TEXT,
    version VARCHAR(20) DEFAULT '1.0',
    metadata JSON DEFAULT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    deleted_at TIMESTAMP NULL,
    INDEX idx_name (name),
    INDEX idx_deleted (deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS subscene_skills (
    subscene_id BIGINT NOT NULL,
    skill_id BIGINT NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (subscene_id, skill_id),
    INDEX idx_subscene (subscene_id),
    INDEX idx_skill (skill_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS sops (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    name VARCHAR(255) NOT NULL,
    slug VARCHAR(100) NOT NULL UNIQUE,
    description TEXT,
    skill_sequence JSON NOT NULL,
    version INT DEFAULT 1,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    deleted_at TIMESTAMP NULL,
    INDEX idx_slug (slug),
    INDEX idx_deleted (deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ============================================================
-- Operational Resources
-- ============================================================

CREATE TABLE IF NOT EXISTS robot_types (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    name VARCHAR(255) NOT NULL,
    model VARCHAR(255) NOT NULL,
    manufacturer VARCHAR(255),
    end_effector VARCHAR(100),
    sensor_suite JSON,
    ros_topics JSON NOT NULL,
    capabilities JSON,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    deleted_at TIMESTAMP NULL,
    INDEX idx_deleted (deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS robots (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    robot_type_id BIGINT NOT NULL,
    serial_number VARCHAR(100) NOT NULL UNIQUE,
    factory_id BIGINT NOT NULL,
    asset_id VARCHAR(100),
    status ENUM('active', 'maintenance', 'retired') DEFAULT 'active',
    metadata JSON DEFAULT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    deleted_at TIMESTAMP NULL,
    UNIQUE INDEX idx_serial (serial_number),
    INDEX idx_type (robot_type_id),
    INDEX idx_factory (factory_id),
    INDEX idx_status (status),
    INDEX idx_deleted (deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS data_collectors (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    name VARCHAR(255) NOT NULL,
    operator_id VARCHAR(100) NOT NULL UNIQUE,
    email VARCHAR(255),
    certification VARCHAR(100),
    status ENUM('active', 'inactive', 'on_leave') DEFAULT 'active',
    metadata JSON DEFAULT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    deleted_at TIMESTAMP NULL,
    UNIQUE INDEX idx_operator_id (operator_id),
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
    factory_id BIGINT NOT NULL,
    name VARCHAR(255),
    status ENUM('active', 'inactive', 'break', 'offline') DEFAULT 'active',
    metadata JSON DEFAULT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    deleted_at TIMESTAMP NULL,
    INDEX idx_robot (robot_id),
    INDEX idx_collector (data_collector_id),
    INDEX idx_factory (factory_id),
    INDEX idx_status (status),
    INDEX idx_deleted (deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS inspectors (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    name VARCHAR(255) NOT NULL,
    inspector_id VARCHAR(100) NOT NULL UNIQUE,
    email VARCHAR(255),
    certification_level ENUM('level_1', 'level_2', 'senior') DEFAULT 'level_1',
    status ENUM('active', 'inactive') DEFAULT 'active',
    metadata JSON DEFAULT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    deleted_at TIMESTAMP NULL,
    INDEX idx_status (status),
    INDEX idx_deleted (deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ============================================================
-- Production Units
-- ============================================================

CREATE TABLE IF NOT EXISTS orders (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    organization_id BIGINT NOT NULL,
    scene_id BIGINT NOT NULL,
    name VARCHAR(255),
    target_count INT NOT NULL,
    priority ENUM('low', 'normal', 'high', 'urgent') DEFAULT 'normal',
    status ENUM('created', 'in_progress', 'paused', 'completed', 'cancelled') DEFAULT 'created',
    deadline TIMESTAMP NULL,
    metadata JSON DEFAULT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    deleted_at TIMESTAMP NULL,
    INDEX idx_org (organization_id),
    INDEX idx_scene (scene_id),
    INDEX idx_status (status),
    INDEX idx_priority (priority),
    INDEX idx_created (created_at),
    INDEX idx_deleted (deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS batches (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    batch_id VARCHAR(100) NOT NULL UNIQUE COMMENT 'Human-readable batch ID',
    order_id BIGINT NOT NULL,
    workstation_id BIGINT NOT NULL,
    name VARCHAR(255) NOT NULL,
    notes TEXT,
    status ENUM('pending', 'active', 'completed', 'cancelled', 'recalled') DEFAULT 'pending',
    episode_count INT DEFAULT 0,
    started_at TIMESTAMP NULL,
    ended_at TIMESTAMP NULL,
    metadata JSON DEFAULT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    deleted_at TIMESTAMP NULL,
    INDEX idx_batch_id (batch_id),
    INDEX idx_order (order_id),
    INDEX idx_workstation (workstation_id),
    INDEX idx_status (status),
    INDEX idx_started (started_at),
    INDEX idx_deleted (deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS tasks (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    task_id VARCHAR(100) NOT NULL UNIQUE COMMENT 'Human-readable task ID',
    batch_id BIGINT NOT NULL,
    order_id BIGINT NOT NULL,
    sop_id BIGINT NOT NULL,
    workstation_id BIGINT,
    scene_id BIGINT NOT NULL,
    subscene_id BIGINT NOT NULL,
    batch_name VARCHAR(255) COMMENT 'Denormalized: batch name for display',
    scene_name VARCHAR(255) COMMENT 'Denormalized: scene name for display',
    subscene_name VARCHAR(255) COMMENT 'Denormalized: subscene name for display',
    factory_id BIGINT COMMENT 'Denormalized: from workstation.factory_id for filtering',
    organization_id BIGINT COMMENT 'Denormalized: from factory.organization_id for filtering',
    initial_scene_layout TEXT,
    status ENUM('pending', 'ready', 'in_progress', 'completed', 'failed', 'cancelled') DEFAULT 'pending',
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
    INDEX idx_task_id (task_id),
    INDEX idx_batch (batch_id),
    INDEX idx_order (order_id),
    INDEX idx_workstation (workstation_id),
    INDEX idx_factory (factory_id),
    INDEX idx_organization (organization_id),
    INDEX idx_status (status),
    INDEX idx_assigned (assigned_at),
    INDEX idx_created (created_at),
    INDEX idx_episode (episode_id),
    INDEX idx_deleted (deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS episodes (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    episode_id VARCHAR(100) NOT NULL UNIQUE COMMENT 'Human-readable episode ID',
    task_id BIGINT NOT NULL,
    batch_id BIGINT NOT NULL COMMENT 'Denormalized: from tasks.batch_id',
    order_id BIGINT NOT NULL COMMENT 'Denormalized: from tasks.order_id',
    scene_id BIGINT NOT NULL COMMENT 'Denormalized: from tasks.scene_id',
    scene_name VARCHAR(255) COMMENT 'Denormalized: from tasks.scene_name',
    workstation_id BIGINT COMMENT 'Denormalized: from tasks.workstation_id',
    factory_id BIGINT COMMENT 'Denormalized: from tasks.factory_id',
    organization_id BIGINT COMMENT 'Denormalized: from tasks.organization_id',
    sop_id BIGINT COMMENT 'Denormalized: from tasks.sop_id',
    mcap_path VARCHAR(1024) NOT NULL,
    sidecar_path VARCHAR(1024) NOT NULL,
    checksum VARCHAR(128),
    file_size_bytes BIGINT,
    duration_sec DECIMAL(10, 2),
    qa_status ENUM('pending_qa', 'qa_running', 'approved', 'needs_inspection', 'inspector_approved', 'rejected', 'failed') DEFAULT 'pending_qa',
    qa_score DECIMAL(4, 3) COMMENT '0.000 to 1.000',
    auto_approved BOOLEAN DEFAULT FALSE,
    inspector_id BIGINT NULL,
    inspection_decision ENUM('approved', 'rejected') NULL,
    inspection_reason TEXT,
    inspected_at TIMESTAMP NULL,
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
    INDEX idx_episode_id (episode_id),
    INDEX idx_task (task_id),
    INDEX idx_batch (batch_id),
    INDEX idx_order (order_id),
    INDEX idx_scene (scene_id),
    INDEX idx_workstation (workstation_id),
    INDEX idx_factory (factory_id),
    INDEX idx_organization (organization_id),
    INDEX idx_qa_status (qa_status),
    INDEX idx_auto_approved (auto_approved),
    INDEX idx_cloud_synced (cloud_synced, cloud_processed),
    INDEX idx_created (created_at),
    INDEX idx_deleted (deleted_at),
    INDEX idx_inspection_queue (qa_status, qa_score, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS operations (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    task_id BIGINT NOT NULL,
    skill_id BIGINT NOT NULL,
    sequence_order INT NOT NULL COMMENT 'Order within the task',
    description TEXT NOT NULL COMMENT 'Natural language operation description',
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    INDEX idx_task (task_id),
    INDEX idx_skill (skill_id)
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

CREATE TABLE IF NOT EXISTS inspections (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    episode_id BIGINT NOT NULL UNIQUE,
    inspector_id BIGINT NOT NULL,
    decision ENUM('approved', 'rejected') NOT NULL,
    reason TEXT NOT NULL,
    failed_tags JSON,
    duration_sec INT COMMENT 'Time spent inspecting',
    inspected_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    INDEX idx_episode (episode_id),
    INDEX idx_inspector (inspector_id),
    INDEX idx_inspected_at (inspected_at)
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
    source_factory_id VARCHAR(100),
    source_path VARCHAR(1024),
    destination_path VARCHAR(1024),
    status ENUM('pending', 'in_progress', 'completed', 'failed') DEFAULT 'pending',
    bytes_transferred BIGINT,
    duration_sec INT,
    error_message TEXT,
    started_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    completed_at TIMESTAMP NULL,
    INDEX idx_episode (episode_id),
    INDEX idx_status (status),
    INDEX idx_started (started_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
