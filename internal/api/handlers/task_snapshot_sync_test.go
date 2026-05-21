// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"testing"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

func setupTaskSnapshotSyncDB(t *testing.T) *sqlx.DB {
	t.Helper()

	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close sqlite db: %v", err)
		}
	})

	_, err = db.Exec(`
		CREATE TABLE tasks (
			id INTEGER PRIMARY KEY,
			scene_id INTEGER,
			subscene_id INTEGER,
			scene_name TEXT,
			subscene_name TEXT,
			deleted_at TEXT
		)
	`)
	if err != nil {
		t.Fatalf("create tasks table: %v", err)
	}
	return db
}

func TestSyncTaskSceneNameBySceneID(t *testing.T) {
	db := setupTaskSnapshotSyncDB(t)
	if _, err := db.Exec(`
		INSERT INTO tasks (id, scene_id, subscene_id, scene_name, subscene_name, deleted_at) VALUES
		(1, 10, 20, 'old-scene', 'sub-a', NULL),
		(2, 10, 21, 'old-scene', 'sub-b', '2026-01-01'),
		(3, 11, 22, 'other-scene', 'sub-c', NULL)
	`); err != nil {
		t.Fatalf("insert tasks: %v", err)
	}

	if err := syncTaskSceneNameBySceneID(db, 10, "new-scene"); err != nil {
		t.Fatalf("syncTaskSceneNameBySceneID returned error: %v", err)
	}

	rows := []struct {
		ID        int    `db:"id"`
		SceneName string `db:"scene_name"`
	}{}
	if err := db.Select(&rows, "SELECT id, scene_name FROM tasks ORDER BY id"); err != nil {
		t.Fatalf("select tasks: %v", err)
	}
	want := map[int]string{1: "new-scene", 2: "old-scene", 3: "other-scene"}
	for _, row := range rows {
		if row.SceneName != want[row.ID] {
			t.Fatalf("task %d scene_name = %q, want %q", row.ID, row.SceneName, want[row.ID])
		}
	}
}

func TestSyncTaskSubsceneSnapshotBySubsceneID(t *testing.T) {
	db := setupTaskSnapshotSyncDB(t)
	if _, err := db.Exec(`
		INSERT INTO tasks (id, scene_id, subscene_id, scene_name, subscene_name, deleted_at) VALUES
		(1, 10, 20, 'old-scene', 'old-subscene', NULL),
		(2, 10, 20, 'old-scene', 'old-subscene', '2026-01-01'),
		(3, 10, 21, 'old-scene', 'other-subscene', NULL)
	`); err != nil {
		t.Fatalf("insert tasks: %v", err)
	}

	if err := syncTaskSubsceneSnapshotBySubsceneID(db, 20, 11, "new-scene", "new-subscene"); err != nil {
		t.Fatalf("syncTaskSubsceneSnapshotBySubsceneID returned error: %v", err)
	}

	rows := []struct {
		ID           int    `db:"id"`
		SceneID      int    `db:"scene_id"`
		SceneName    string `db:"scene_name"`
		SubsceneName string `db:"subscene_name"`
	}{}
	if err := db.Select(&rows, "SELECT id, scene_id, scene_name, subscene_name FROM tasks ORDER BY id"); err != nil {
		t.Fatalf("select tasks: %v", err)
	}

	want := map[int]struct {
		sceneID      int
		sceneName    string
		subsceneName string
	}{
		1: {sceneID: 11, sceneName: "new-scene", subsceneName: "new-subscene"},
		2: {sceneID: 10, sceneName: "old-scene", subsceneName: "old-subscene"},
		3: {sceneID: 10, sceneName: "old-scene", subsceneName: "other-subscene"},
	}
	for _, row := range rows {
		w := want[row.ID]
		if row.SceneID != w.sceneID || row.SceneName != w.sceneName || row.SubsceneName != w.subsceneName {
			t.Fatalf("task %d = (%d, %q, %q), want (%d, %q, %q)",
				row.ID,
				row.SceneID,
				row.SceneName,
				row.SubsceneName,
				w.sceneID,
				w.sceneName,
				w.subsceneName,
			)
		}
	}
}
