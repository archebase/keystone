// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import "database/sql"

type taskSnapshotExecer interface {
	Exec(query string, args ...interface{}) (sql.Result, error)
}

func syncTaskSceneNameBySceneID(db taskSnapshotExecer, sceneID int64, sceneName string) error {
	_, err := db.Exec(
		`UPDATE tasks SET scene_name = ? WHERE scene_id = ? AND deleted_at IS NULL`,
		sceneName,
		sceneID,
	)
	return err
}

func syncTaskSubsceneSnapshotBySubsceneID(db taskSnapshotExecer, subsceneID int64, sceneID int64, sceneName string, subsceneName string) error {
	_, err := db.Exec(
		`UPDATE tasks
		 SET scene_id = ?, scene_name = ?, subscene_name = ?
		 WHERE subscene_id = ? AND deleted_at IS NULL`,
		sceneID,
		sceneName,
		subsceneName,
		subsceneID,
	)
	return err
}
