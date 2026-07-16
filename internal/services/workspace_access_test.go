// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"context"
	"testing"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

func TestWorkspaceAccessUsesAdminsAndMembersAcrossWorkspaces(t *testing.T) {
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`
		CREATE TABLE workspaces (
			id INTEGER PRIMARY KEY,
			admins TEXT NOT NULL,
			members TEXT NOT NULL,
			deleted_at TIMESTAMP NULL
		)
	`); err != nil {
		t.Fatalf("create workspaces: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO workspaces (id, admins, members) VALUES
			(10, '["alice"]', '["bob"]'),
			(20, '[]', '["alice", "charlie"]'),
			(30, '["alice"]', '[]')
	`); err != nil {
		t.Fatalf("insert workspaces: %v", err)
	}
	if _, err := db.Exec(`UPDATE workspaces SET deleted_at = CURRENT_TIMESTAMP WHERE id = 30`); err != nil {
		t.Fatalf("delete workspace: %v", err)
	}

	ctx := context.Background()
	allowed, err := OperatorHasWorkspaceAccess(ctx, db, "alice", 20)
	if err != nil {
		t.Fatalf("check access: %v", err)
	}
	if !allowed {
		t.Fatal("alice should access workspace 20 as a member")
	}

	workspaceIDs, err := AccessibleWorkspaceIDs(ctx, db, "alice")
	if err != nil {
		t.Fatalf("list accessible workspaces: %v", err)
	}
	if len(workspaceIDs) != 2 || workspaceIDs[0] != 10 || workspaceIDs[1] != 20 {
		t.Fatalf("accessible workspace IDs = %v, want [10 20]", workspaceIDs)
	}
}

func TestEncodeWorkspacePeopleNormalizesValues(t *testing.T) {
	encoded, err := EncodeWorkspacePeople([]string{" alice ", "", "bob", "alice"})
	if err != nil {
		t.Fatalf("encode workspace people: %v", err)
	}
	if encoded != `["alice","bob"]` {
		t.Fatalf("encoded people = %s, want [\"alice\",\"bob\"]", encoded)
	}
}
