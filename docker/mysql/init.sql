-- init.sql - Keystone Edge database initialization script

-- Create tasks table
CREATE TABLE IF NOT EXISTS tasks (
    id VARCHAR(64) PRIMARY KEY,
    batch_id VARCHAR(64) NOT NULL,
    scene_id VARCHAR(64) NOT NULL,
    workstation_id VARCHAR(64),
    robot_id VARCHAR(64) NOT NULL,
    operator_id VARCHAR(64),
    status VARCHAR(32) NOT NULL DEFAULT 'pending',
    priority INT NOT NULL DEFAULT 0,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    INDEX idx_status (status, created_at),
    INDEX idx_batch (batch_id),
    INDEX idx_robot (robot_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Create episodes table
CREATE TABLE IF NOT EXISTS episodes (
    id VARCHAR(64) PRIMARY KEY,
    task_id VARCHAR(64) NOT NULL,
    batch_id VARCHAR(64) NOT NULL,
    scene_id VARCHAR(64) NOT NULL,
    robot_id VARCHAR(64) NOT NULL,
    start_time TIMESTAMP NOT NULL,
    end_time TIMESTAMP NOT NULL,
    file_size_bytes BIGINT NOT NULL DEFAULT 0,
    file_path VARCHAR(512) NOT NULL,
    sidecar_path VARCHAR(512),
    qa_status VARCHAR(32) NOT NULL DEFAULT 'pending',
    qa_score FLOAT,
    qa_comment TEXT,
    cloud_synced BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    INDEX idx_qa (qa_status, cloud_synced),
    INDEX idx_task (task_id),
    INDEX idx_batch (batch_id),
    INDEX idx_robot (robot_id),
    INDEX idx_time (start_time, end_time)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Create workstations table
CREATE TABLE IF NOT EXISTS workstations (
    id VARCHAR(64) PRIMARY KEY,
    name VARCHAR(128) NOT NULL,
    location VARCHAR(256),
    status VARCHAR(32) NOT NULL DEFAULT 'offline',
    current_robot_id VARCHAR(64),
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    INDEX idx_status (status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Create scenes table (synced from cloud, read-only)
CREATE TABLE IF NOT EXISTS scenes (
    id VARCHAR(64) PRIMARY KEY,
    name VARCHAR(128) NOT NULL,
    description TEXT,
    version INT NOT NULL DEFAULT 1,
    active BOOLEAN NOT NULL DEFAULT TRUE,
    synced_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    INDEX idx_active (active, version)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Create sync checkpoints table
CREATE TABLE IF NOT EXISTS sync_checkpoints (
    id VARCHAR(64) PRIMARY KEY,
    last_synced_episode VARCHAR(64),
    last_synced_at TIMESTAMP,
    retry_count INT NOT NULL DEFAULT 0,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Create state event log table (for state machine auditing)
CREATE TABLE IF NOT EXISTS state_events (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    entity_type VARCHAR(32) NOT NULL,
    entity_id VARCHAR(64) NOT NULL,
    old_state VARCHAR(32),
    new_state VARCHAR(32) NOT NULL,
    event_type VARCHAR(64) NOT NULL,
    metadata JSON,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    INDEX idx_entity (entity_type, entity_id),
    INDEX idx_time (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Insert sample data
INSERT INTO scenes (id, name, description, active) VALUES
    ('scene-001', 'Basic Picking Scene', 'Standard robot picking task scene', TRUE),
    ('scene-002', 'Precision Assembly Scene', 'High-precision assembly task scene', TRUE),
    ('scene-003', 'Quality Inspection Scene', 'Visual inspection task scene', TRUE)
ON DUPLICATE KEY UPDATE name=VALUES(name);

INSERT INTO workstations (id, name, location, status) VALUES
    ('ws-001', 'Workstation-A1', 'Workshop Zone A', 'online'),
    ('ws-002', 'Workstation-A2', 'Workshop Zone A', 'offline'),
    ('ws-003', 'Workstation-B1', 'Workshop Zone B', 'maintenance')
ON DUPLICATE KEY UPDATE name=VALUES(name);
