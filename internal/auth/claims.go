// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package auth provides JWT claim types and helpers for authentication.
package auth

import "github.com/golang-jwt/jwt/v5"

// Claims represents JWT claims for collector authentication.
type Claims struct {
	CollectorID int64  `json:"collector_id"`
	OperatorID  string `json:"operator_id"`
	Role        string `json:"role"`
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

// NewAdminClaims creates claims for an admin identity.
// CollectorID is intentionally zero — admin accounts are not stored in the database.
func NewAdminClaims() *Claims {
	return &Claims{
		Role: "admin",
	}
}
