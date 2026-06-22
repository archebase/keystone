-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

CREATE TABLE IF NOT EXISTS ws_client_auth_tokens (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    robot_id BIGINT NOT NULL,
    token_hash CHAR(64) NOT NULL,
    token_version VARCHAR(16) NOT NULL DEFAULT 'kws_v1',
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    last_rotated_at TIMESTAMP NULL,
    last_used_at TIMESTAMP NULL,
    revoked_at TIMESTAMP NULL,
    UNIQUE INDEX idx_ws_client_token_hash (token_hash),
    INDEX idx_ws_client_robot_active (robot_id, revoked_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
