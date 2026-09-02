// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package config

import "time"

const (
	// HilbertRequestTimeout bounds one outbound Hilbert HTTP request.
	HilbertRequestTimeout = 10 * time.Second
	// HilbertWorkspaceSyncTimeout bounds synchronization for one workspace.
	// Workspace sync also performs local projection and pending-pool maintenance,
	// which can wait on concurrent task/workstation transactions under load.
	HilbertWorkspaceSyncTimeout = 5 * time.Minute
	// HilbertSyncBatchTimeout bounds one complete workspace or dc-plan batch.
	HilbertSyncBatchTimeout = 10 * time.Minute
)
