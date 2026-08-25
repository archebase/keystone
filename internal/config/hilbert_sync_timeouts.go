// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package config

import "time"

const (
	// HilbertRequestTimeout bounds one outbound Hilbert HTTP request.
	HilbertRequestTimeout = 10 * time.Second
	// HilbertWorkspaceSyncTimeout bounds synchronization for one workspace.
	HilbertWorkspaceSyncTimeout = 60 * time.Second
	// HilbertSyncBatchTimeout bounds one complete workspace or dc-plan batch.
	HilbertSyncBatchTimeout = 10 * time.Minute
)
