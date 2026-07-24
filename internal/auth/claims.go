// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package auth provides JWT claim types and helpers for authentication.
package auth

import "github.com/golang-jwt/jwt/v5"

// Claims represents JWT claims for collector authentication.
type Claims struct {
	CollectorID   int64  `json:"collector_id"`
	OperatorID    string `json:"operator_id"`
	WorkstationID int64  `json:"workstation_id,omitempty"`
	RobotID       int64  `json:"robot_id,omitempty"`
	WorkspaceID   int64  `json:"workspace_id,omitempty"`
	Role          string `json:"role"`
	jwt.RegisteredClaims
}

// NewCollectorClaims creates claims for a data collector identity.
func NewCollectorClaims(collectorID int64, operatorID string) *Claims {
	return &Claims{
		CollectorID: collectorID,
		OperatorID:  operatorID,
		Role:        "data_collector",
	}
}

// NewCollectorWorkstationClaims creates claims bound to one active workstation session.
func NewCollectorWorkstationClaims(
	collectorID int64,
	operatorID string,
	workstationID int64,
	robotID int64,
	workspaceID int64,
) *Claims {
	return &Claims{
		CollectorID:   collectorID,
		OperatorID:    operatorID,
		WorkstationID: workstationID,
		RobotID:       robotID,
		WorkspaceID:   workspaceID,
		Role:          "data_collector",
	}
}

// NewAdminClaims creates claims for an admin identity.
// CollectorID is intentionally zero — admin accounts are not stored in the database.
func NewAdminClaims() *Claims {
	return &Claims{
		Role: "admin",
	}
}
