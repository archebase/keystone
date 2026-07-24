// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"errors"
	"testing"

	"archebase.com/keystone-edge/internal/auth"
)

func TestResolveTaskListWorkstationScopeUsesAuthenticatedWorkstation(t *testing.T) {
	claims := auth.NewCollectorWorkstationClaims(7, "collector-a", 11, 9, 123)

	workstationID, err := resolveTaskListWorkstationScope(claims, "")

	if err != nil {
		t.Fatalf("resolve scope: %v", err)
	}
	if workstationID != "11" {
		t.Fatalf("workstation_id=%q want authenticated workstation 11", workstationID)
	}
}

func TestResolveTaskListWorkstationScopeRejectsConflictingFilter(t *testing.T) {
	claims := auth.NewCollectorWorkstationClaims(7, "collector-a", 11, 9, 123)

	_, err := resolveTaskListWorkstationScope(claims, "12")

	if !errors.Is(err, errTaskListWorkstationScopeMismatch) {
		t.Fatalf("error=%v want workstation scope mismatch", err)
	}
}

func TestResolveTaskListWorkstationScopeRequiresActivation(t *testing.T) {
	claims := auth.NewCollectorClaims(7, "collector-a")

	_, err := resolveTaskListWorkstationScope(claims, "")

	if !errors.Is(err, errTaskListWorkstationActivationRequired) {
		t.Fatalf("error=%v want workstation activation required", err)
	}
}
