<!--
SPDX-FileCopyrightText: 2026 ArcheBase

SPDX-License-Identifier: MulanPSL-2.0
-->

# Cloud Sync Interaction Implementation Design

## 1. Overview

This document defines how Synapse should expose Keystone cloud sync operations for
episodes that have already been uploaded from Axon edge devices into Keystone's
local MinIO storage.

Cloud sync in this context means:

```
Keystone local MinIO episode MCAP
        -> Keystone SyncWorker
        -> data-platform DataGateway
        -> cloud OSS/object storage
```

It does not cover the earlier edge-device upload path:

```
axon_recorder / axon_transfer
        -> Keystone local MinIO
        -> episodes table
```

Product direction: cloud sync should become its own operational surface in
Synapse. Data production statistics should keep cloud sync as a metric only, but
should not be the primary place for queue control, retry diagnosis, navigation,
or future sync operations.

## 2. Current Backend Capability

### 2.1 Existing Cloud Sync APIs

Keystone exposes the following endpoints today, with the summary/history
endpoints recommended for the episode-centered Cloud Sync Center redesign:

| Method | Path | Purpose |
|--------|------|---------|
| `POST` | `/api/v1/sync/episodes` | Enqueue pending approved, unsynced episodes for cloud sync |
| `POST` | `/api/v1/sync/episodes/:id` | Enqueue one episode for cloud sync by numeric episode ID |
| `GET` | `/api/v1/sync/episodes` | Existing: list raw sync log entries for history/diagnosis |
| `GET` | `/api/v1/sync/episodes/summary` | Recommended new endpoint: list latest sync state grouped by episode |
| `GET` | `/api/v1/sync/episodes/:id/status` | Get latest sync log for one episode |
| `GET` | `/api/v1/sync/episodes/:id/logs` | Recommended new endpoint: list raw sync log history for one episode |
| `GET` | `/api/v1/sync/config` | Get sanitized sync worker configuration |

### 2.2 SyncWorker Eligibility Rules

An episode is eligible for cloud sync when all of the following are true:

| Rule | Source |
|------|--------|
| `episodes.qa_status IN ('approved', 'inspector_approved')` | `episodes` |
| `episodes.cloud_synced = FALSE` | `episodes` |
| `episodes.deleted_at IS NULL` | `episodes` |
| Latest sync log is not `completed` | `sync_logs` |
| No active `pending` or `in_progress` sync log exists | `sync_logs` |

The worker reads `episodes.mcap_path`, strips the bucket prefix, streams the MCAP
from local MinIO, uploads it through the cloud DataGateway/OSS flow, and updates:

- `sync_logs.status`
- `sync_logs.destination_path`
- `sync_logs.bytes_transferred`
- `sync_logs.duration_sec`
- `sync_logs.error_message`
- `sync_logs.next_retry_at`
- `episodes.cloud_synced`
- `episodes.cloud_synced_at`
- `episodes.cloud_mcap_path`
- `episodes.cloud_processed`

### 2.3 Manual Trigger Semantics

The existing manual APIs have different retry behavior.

#### Single Episode Trigger

`POST /api/v1/sync/episodes/:id` calls `EnqueueEpisodeManual()`.

Behavior:

- No previous sync log: creates a new `sync_logs` row with `attempt_count = 1`.
- Latest failed log is retryable and due: reuses the failed row and increments
  `attempt_count`.
- Latest failed log is exhausted or still in backoff: creates a new sync log row
  with `attempt_count = 1`.
- Active `pending` or `in_progress`: rejected.
- Already `cloud_synced = true`: rejected by API.

#### Batch Trigger

`POST /api/v1/sync/episodes` calls `EnqueuePendingEpisodes()`.

Behavior:

- Enqueues pending episodes returned by backend discovery.
- Does not force a fresh attempt chain for failed rows.
- Reuses normal automatic retry rules during job acquisition.
- May discover exhausted failed rows, but `manual=false` means acquisition still
  rejects rows that exceeded automatic retry limits.

For product clarity, the current batch API should be presented as "scan and
enqueue eligible pending episodes", not as "force retry all failures".

### 2.4 Listing Model

`sync_logs` is an audit-style attempt-chain table. One episode may have multiple
rows because manual retry can create a fresh `sync_logs` row when the latest
failed row is exhausted or still in backoff. Showing raw `sync_logs` as the
default Cloud Sync Center table can make users think Keystone created duplicate
work for the same episode.

The default Cloud Sync Center table should therefore be episode-centered:

- one row per `episodes.id`
- current status from the latest `sync_logs` row for that episode
- total attempt count summed across all `sync_logs` rows for that episode
- latest error, retry time, timestamps, and cloud destination from the latest row

Raw `sync_logs` rows should remain available as a history/diagnostic view, such
as an expandable row, drawer, or "View history" action.

## 3. Product Goals

### 3.1 User Goals

| User | Goal |
|------|------|
| Admin | Confirm whether data has reached cloud storage |
| Admin | Retry failed cloud sync jobs safely |
| Admin | Understand why a sync failed |
| Admin | Trigger sync for all eligible unsynced episodes |
| Operator | See whether collected data is available, without managing retries |

### 3.2 Design Goals

- Make cloud sync state visible where users already inspect data production.
- Separate "accepted for background sync" from "sync completed".
- Avoid fake progress bars because Keystone does not expose upload percentage.
- Make failure recovery explicit and auditable.
- Keep destructive or expensive bulk actions deliberate.
- Preserve current backend behavior while identifying API gaps for follow-up.
- Keep the data production statistics page focused on analysis, not operations.

## 4. Information Architecture

Cloud sync should be organized around a dedicated `Cloud Sync Center` page, with
lighter contextual entry points elsewhere.

| Surface | Purpose | Scope |
|---------|---------|-------|
| Cloud Sync Center | Primary operation and diagnosis surface | Global queue, failures, worker state |
| Episode detail page | Inspect and retry one episode | Single episode |
| Data production statistics page | Show sync outcome as a production metric | Metrics and filters only |

### 4.1 Cloud Sync Center Page

Add a dedicated route, for example:

```
/admin/cloud-sync
```

Recommended navigation placement:

- `Data Management > Cloud Sync`, if Synapse groups data lifecycle features.
- `System Operations > Cloud Sync`, if Synapse groups operational controls.

The page should behave like an operational console, not a marketing, help, or
analytics page. It should be dense, scannable, and action-oriented.

First viewport layout:

| Area | Purpose | Notes |
|------|---------|-------|
| Header | Page identity and primary action | Title plus one primary enqueue action |
| Compact status band | Worker health and queue summary | Lower visual weight than the task table |
| Task panel | Main work area | Status tabs, table, pagination, row actions |

Do not add:

- A separate explanatory card such as "background sync queue".
- A "view data production statistics" card or shortcut on this page.
- A top-right refresh button competing with the primary enqueue action.
- Marketing-style hero copy, oversized metric cards, or decorative panels.

Recommended header action copy:

| Copy | Use |
|------|-----|
| `Scan and Queue Eligible Episodes` | Preferred English product copy |
| `Sync Approved Unsynced Episodes` | Acceptable if the product wants explicit eligibility |
| `扫描并加入同步队列` | Preferred Chinese product copy |
| `同步合格未上云片段` | Current implementation-compatible Chinese copy |

The primary action should be disabled when the worker is not running or when the
request is already in flight. Worker-unavailable state should keep historical
tasks readable and filterable; only enqueue/retry actions are disabled.

Compact status band:

| Item | Source | Notes |
|------|--------|-------|
| Worker running | `/api/v1/sync/config` | Green/gray status |
| Queued count | Episode sync summary | Latest status is `pending` |
| In-progress count | Episode sync summary | Latest status is `in_progress` |
| Failed count | Episode sync summary | Latest status is `failed` |
| Last successful sync | Latest completed sync log | Useful health signal |

The status band should read as a compact control surface, not as five separate
feature cards. It can use cell separators, narrow borders, or small chips, but
the task table must remain the visual center of the page.

Task panel:

| Element | Behavior |
|---------|----------|
| Status tabs | Above the table; all / pending / in_progress / completed / failed |
| Failed tab/count | Use alert color only when failed count is greater than zero |
| Table | Episode summary table; one row per episode; sticky header is recommended |
| Row navigation | Episode ID opens episode detail when available |
| Row retry | Show only for failed rows; avoid disabled retry buttons on normal rows |
| Row history | Optional drawer/expand action showing raw `sync_logs` for the episode |
| Long paths/errors | Truncate in-cell, expose full value via title; copy/expand is a follow-up |
| Pagination | Bottom of the task panel |

The failure view should be a status tab and filtered table state, not a separate
large card. Diagnostics should be represented by row-level error content and
compact notices until backend health APIs become richer.

Primary actions:

| Action | Current Backend Support | UI Behavior |
|--------|-------------------------|-------------|
| Scan and queue eligible unsynced episodes | `POST /api/v1/sync/episodes` | Confirm before enqueue, then refresh counts/table |
| Retry one failed episode | `POST /api/v1/sync/episodes/:id` | Row-level action, then refresh the row/list |
| Retry failed episodes in bulk | Not explicit yet | TODO; requires backend API semantics |
| Pause/resume automatic sync | Not supported yet | TODO; do not show as active control |
| Edit sync configuration | Not supported yet | TODO; read-only summary for now |

The page should make queue state explicit:

```
Not Synced -> Queued -> Syncing -> Synced
                         -> Failed -> Retry
```

After any manual trigger, do not rely on the `202 Accepted` message alone. The
UI should immediately refresh task counts and the affected rows. For single
retry, the row should feel queued immediately; if the backend returns `409
already_queued` or `409 already synced`, refresh the latest status and present
that real state.

### 4.2 Episode Detail Page

Add a `Cloud Sync` panel below the existing episode metadata or next to data
preview actions.

The panel should show:

- Sync status label
- Sync worker availability
- Latest attempt count
- Started/completed timestamps
- Next retry time
- Cloud destination path
- Failure reason
- Primary action button

Recommended actions:

| Episode / Sync State | Primary Action | UI State |
|----------------------|----------------|----------|
| `cloud_synced = true` | None | Show "Synced" and cloud path |
| QA not approved | None | Disabled, "QA approval required" |
| No sync log | `Sync to Cloud` | Enabled |
| Latest log `failed` | `Retry Now` | Enabled |
| Latest log `in_progress` | None | Disabled, "Syncing" |
| Worker not running | None | Disabled, "Cloud sync service unavailable" |

After a successful trigger response, show "Queued for cloud sync" and poll the
status endpoint until the latest status becomes `completed` or `failed`.

### 4.3 Data Production Statistics Page

The statistics page should not be the primary cloud sync operation surface. It
should answer "how much data was produced and how much has reached cloud". Users
should reach Cloud Sync Center from the main navigation, not from an extra card
inside the statistics page.

Keep:

- Cloud sync rate metric.
- Cloud sync filters in statistics queries.

Avoid:

- Large `Cloud Sync` operation cards.
- Small `Cloud Sync` entry cards that compete with statistics content.
- Batch enqueue as a primary action on the statistics page.
- Failure diagnosis tables inside the statistics workflow.
- Worker health checks or sync configuration calls from the statistics page.
- Copy that implies statistics filters control the batch sync operation.

### 4.4 Episode Summary Table / Sync History

The Cloud Sync Center default table should show one row per episode, not one row
per `sync_logs` entry. This keeps the page aligned with the operator question:
"which episodes need attention now?" Raw log rows are secondary history.

Default summary columns:

| Column | Source |
|--------|--------|
| Episode ID | `episode_public_id` / `episode_id` |
| Status | latest `sync_logs.status` |
| Total attempts | `SUM(sync_logs.attempt_count)` grouped by episode |
| Latest attempt | latest `sync_logs.attempt_count` |
| Next retry | latest `sync_logs.next_retry_at`; hide when retry is exhausted |
| Duration | latest `sync_logs.duration_sec` |
| Size | latest `sync_logs.bytes_transferred` |
| Started | latest `sync_logs.started_at` |
| Completed | latest `sync_logs.completed_at` |
| Destination path | latest `sync_logs.destination_path` |
| Error | latest `sync_logs.error_message` |

Filters:

- Status: all / in_progress / completed / failed
- Limit / offset pagination

History view:

- available from each summary row
- lists raw `sync_logs` rows for that episode, ordered by `started_at DESC` or
  `id DESC`
- shows each attempt-chain row's status, attempt count, retry time, timestamps,
  destination path, and error
- supports row-level navigation back to episode detail when `episode_id` is
  available

## 5. Visual Design

### 5.1 Status Language

| State | Label | Visual Treatment |
|-------|-------|------------------|
| Synced | `Synced` | Green badge |
| Not synced | `Not Synced` | Neutral badge |
| Queued | `Queued` | Amber/yellow badge |
| Syncing | `Syncing` | Blue badge with subtle spinner |
| Waiting retry | Not shown as a separate main status | Use `Failed`; hide next retry when retry is exhausted |
| Failed | `Failed` | Red badge |
| Unavailable | `Unavailable` | Muted badge |
| Not eligible | `Not Eligible` | Muted outlined badge |

### 5.2 Cloud Sync Center Layout

- Keep the header compact: title, optional short kicker, and one primary action.
- Keep the status band visually lighter than the task table.
- Let the task table take most of the vertical space on desktop.
- Use status tabs as the main filtering control; avoid duplicate filter cards.
- Keep the failed count easy to notice without turning the whole page into an
  alarm state.
- Avoid explanatory text blocks. Empty states and notices can carry short,
  contextual copy when needed.

### 5.3 Task Table Interaction

- Show retry actions only for `failed` rows.
- Hide or leave the action cell empty for `pending`, `in_progress`, and
  `completed` rows instead of showing disabled retry buttons.
- Keep episode ID, status, total attempt count, next retry, started/completed
  time, destination path, error, and row action visible in the first
  implementation.
- Use `total_attempt_count` for the default table. Use latest-row
  `attempt_count` only inside the history/diagnostic view.
- Truncate long object paths and error messages in the table; full-value copy or
  expandable detail rows can be added later.
- Use sticky table headers when the page grows beyond one viewport.
- After batch enqueue, refresh config, counts, and the active table page.
- After single retry, refresh counts and either the active table page or the
  affected row.

### 5.4 Interaction Principles

- Use compact operational UI, not a marketing layout.
- Put actions close to the related state.
- Use confirmation for batch enqueue.
- Use clear background-job language.
- Do not imply completion immediately after `202 Accepted`.
- Truncate long paths in tables and provide access to the full value.
- Keep the statistics page visually quiet: do not add cloud sync control or
  navigation cards there.
- Keep `.env`-backed configuration read-only/TODO for now; do not design an
  editable configuration screen until backend semantics are explicit.

## 6. Frontend Implementation

### 6.1 API Module

Add `synapse/src/api/sync.js`:

```js
import { useApiClient } from './client.js'

export function useSyncApi() {
  const api = useApiClient()

  return {
    triggerBatch: () => api.post('/sync/episodes', {}),
    triggerEpisode: (id) => api.post(`/sync/episodes/${id}`, {}),
    listEpisodeSummaries: (params = {}) => api.get('/sync/episodes/summary', params),
    listJobs: (params = {}) => api.get('/sync/episodes', params),
    listEpisodeHistory: (id, params = {}) => api.get(`/sync/episodes/${id}/logs`, params),
    getStatus: (id) => api.get(`/sync/episodes/${id}/status`),
    getConfig: () => api.get('/sync/config')
  }
}

export default useSyncApi
```

### 6.2 Episode Detail Changes

Update `synapse/src/views/admin/episodes/EpisodeDetail.vue`:

- Import `useSyncApi`.
- Fetch `getConfig()` and `getStatus(episodeId)` after episode load.
- Add computed fields:
  - `syncWorkerRunning`
  - `latestSyncStatus`
  - `canTriggerSync`
  - `syncActionLabel`
  - `syncDisabledReason`
- Add `handleTriggerSync()`.
- Poll status after trigger:
  - Initial interval: 2 seconds
  - Stop after `completed`, `failed`, route leave, or a fixed timeout
  - Refresh episode detail after terminal status

Important status handling:

- `GET /sync/episodes/:id/status` may return 404 before the worker creates a
  `sync_logs` row. Treat this as `queued` after a trigger, not as failure.
- `POST /sync/episodes/:id` returns `202 Accepted`, not completion.
- `409 already synced` should refresh episode detail.
- `409 already queued` should switch UI to `queued` or `syncing`.

### 6.3 Cloud Sync Center Page

Add a dedicated Synapse view, for example:

```
synapse/src/views/admin/sync/CloudSyncCenter.vue
```

Recommended responsibilities:

- Fetch `getConfig()` during page load.
- Load episode-centered sync summaries from `GET /sync/episodes/summary`.
- Provide status filters:
  - all
  - pending
  - in_progress
  - completed
  - failed
- Show top-level counters derived from episode summary counts.
- Provide `Sync all eligible unsynced episodes`.
- Show confirmation before calling batch API.
- Refresh task list after successful enqueue.
- Navigate from a sync row to episode detail.
- Open raw sync history from a row when diagnosis is needed.
- Keep the header free of secondary refresh/navigation buttons.
- Keep retry visible only on failed rows.
- Keep worker-stopped state action-scoped: disable enqueue/retry, but keep
  historical sync inspection available.

The page should treat `202 Accepted` as a queued background operation, not as
completion.

### 6.4 Statistics Page Changes

Update `synapse/src/views/admin/statistics/DataProductionStatistics.vue` to
remove cloud sync operations and navigation:

- Keep the cloud sync rate metric.
- Keep cloud sync filters for statistics queries.
- Do not fetch `getConfig()` from this page.
- Do not render a Cloud Sync card, strip, drawer, or entry button.
- Do not keep batch enqueue as an action on this page.

### 6.5 Sync Summary Table / History Component

Create a reusable episode summary table component. A history drawer can be kept
as a secondary diagnostic component that reads raw `sync_logs` for a selected
episode.

Example:

```
synapse/src/components/sync/SyncEpisodeSummaryTable.vue
synapse/src/components/sync/SyncEpisodeHistoryDrawer.vue
```

Summary table props:

| Prop | Type | Description |
|------|------|-------------|
| `items` | array | Episode-centered summary rows |
| `loading` | boolean | Loading state |
| `status` | string | Active status filter |

Events:

| Event | Payload | Description |
|-------|---------|-------------|
| `view-episode` | numeric episode ID | Navigate to episode detail |
| `view-history` | numeric episode ID | Open raw sync log history |
| `retry-episode` | numeric episode ID | Trigger single episode sync |

History drawer props:

| Prop | Type | Description |
|------|------|-------------|
| `open` | boolean | Drawer visibility |
| `episodeId` | number | Selected episode numeric ID |

History drawer events:

| Event | Payload | Description |
|-------|---------|-------------|
| `close` | none | Close drawer |

The component should own:

- `status` filter
- pagination
- loading state
- error state
- refresh action

## 7. Backend Follow-up Recommendations

The original MVP can use existing raw log endpoints, but the episode-centered
Cloud Sync Center requires the summary/history API improvements below.

### 7.1 Add Episode Sync Summary Endpoint

Add `GET /api/v1/sync/episodes/summary` for the Cloud Sync Center default table.
It should return one row per episode, using the latest `sync_logs` row as the
current state and aggregate attempt counts across all rows for that episode.

Recommended response shape:

```json
{
  "items": [
    {
      "id": 120,
      "episode_id": 42,
      "episode_public_id": "dpstat-devices-20260507083341391413-episode-002",
      "status": "failed",
      "total_attempt_count": 12,
      "latest_attempt_count": 2,
      "sync_log_count": 6,
      "next_retry_at": null,
      "started_at": "2026-05-09T13:02:00Z",
      "completed_at": "2026-05-09T13:02:02Z",
      "destination_path": null,
      "error_message": "The specified key does not exist"
    }
  ],
  "total": 1,
  "limit": 50,
  "offset": 0,
  "hasNext": false,
  "hasPrev": false
}
```

The default status filter should apply to the latest row status, not historical
rows. For example, `status=failed` means "episodes whose latest sync state is
failed", not "all failed sync log rows".

Recommended SQL shape:

```sql
SELECT
  latest_log.id,
  latest_log.episode_id,
  e.episode_id AS episode_public_id,
  latest_log.source_path,
  latest_log.destination_path,
  latest_log.status,
  latest_log.bytes_transferred,
  latest_log.duration_sec,
  latest_log.error_message,
  COALESCE(agg.total_attempt_count, 0) AS total_attempt_count,
  latest_log.attempt_count AS latest_attempt_count,
  agg.sync_log_count,
  latest_log.next_retry_at,
  latest_log.started_at,
  latest_log.completed_at
FROM sync_logs latest_log
JOIN (
  SELECT episode_id, MAX(id) AS latest_id
  FROM sync_logs
  GROUP BY episode_id
) latest
  ON latest_log.episode_id = latest.episode_id
 AND latest_log.id = latest.latest_id
JOIN (
  SELECT
    episode_id,
    SUM(COALESCE(attempt_count, 0)) AS total_attempt_count,
    COUNT(*) AS sync_log_count
  FROM sync_logs
  GROUP BY episode_id
) agg
  ON agg.episode_id = latest_log.episode_id
LEFT JOIN episodes e
  ON e.id = latest_log.episode_id
 AND e.deleted_at IS NULL
WHERE 1=1
-- optional latest-state filter:
-- AND latest_log.status = ?
ORDER BY latest_log.started_at DESC
LIMIT ? OFFSET ?;
```

The count query should use the same latest-row scope:

```sql
SELECT COUNT(*)
FROM sync_logs latest_log
JOIN (
  SELECT episode_id, MAX(id) AS latest_id
  FROM sync_logs
  GROUP BY episode_id
) latest
  ON latest_log.episode_id = latest.episode_id
 AND latest_log.id = latest.latest_id
WHERE 1=1
-- optional latest-state filter:
-- AND latest_log.status = ?;
```

The existing indexes are sufficient for the first implementation:

```sql
INDEX idx_sync_episode_latest (episode_id, id)
INDEX idx_sync_episode_status (episode_id, status)
INDEX idx_status (status)
```

Only add a migration after checking `EXPLAIN` on production-like data. Candidate
follow-up indexes are `(status, episode_id, id)` for status-filtered summary
queries and `(episode_id, started_at)` for per-episode history ordering.

### 7.2 Add Raw Sync Log History Endpoint

Keep `GET /api/v1/sync/episodes` as the raw log list for compatibility and
diagnosis. Add a scoped history endpoint for row drill-down:

```http
GET /api/v1/sync/episodes/:id/logs
```

This endpoint should return raw `sync_logs` rows for one episode, ordered by
`id DESC` or `started_at DESC`, with the same pagination shape as the existing
list API.

### 7.3 Add Latest Sync Summary to Episode Responses

Add optional fields to `GET /episodes` and `GET /episodes/:id`:

```json
{
  "sync_status": "failed",
  "sync_attempt_count": 5,
  "sync_error_message": "complete upload on gateway: deadline exceeded",
  "sync_next_retry_at": "2026-05-09T10:15:00Z",
  "sync_started_at": "2026-05-09T10:00:00Z",
  "sync_completed_at": "2026-05-09T10:01:23Z"
}
```

This avoids N+1 status queries in episode lists.

### 7.4 Clarify Batch Trigger Modes

Current batch trigger is not a forced retry. Add request body support:

```json
{
  "force": false,
  "status": "failed",
  "created_at_from": "2026-05-01T00:00:00Z",
  "created_at_to": "2026-05-09T23:59:59Z"
}
```

Recommended semantics:

| Option | Behavior |
|--------|----------|
| `force=false` | Existing automatic retry rules |
| `force=true` | Manual mode, starts a new attempt chain when exhausted/backoff |
| filters | Only enqueue episodes matching provided filters |

Until this exists, frontend copy must say "eligible unsynced episodes", not
"failed episodes" or "current filters".

### 7.5 Return More Sync Config

For the first Cloud Sync Center version, sync configuration should be read-only.
Do not move `.env` editing into Synapse yet.

TODO: extend `GET /sync/config` with non-sensitive runtime values:

```json
{
  "worker_running": true,
  "batch_size": 10,
  "max_concurrent": 2,
  "max_retries": 5,
  "worker_interval_sec": 60
}
```

TODO: decide later whether any config should become editable from Synapse. If
that is added, keep secret values masked and require admin permissions, audit
logs, validation, and clear restart/reload semantics.

Credential handling: `KEYSTONE_CLOUD_API_KEY` is a cloud-issued opaque
credential. Keystone should only check that it is present when sync is enabled,
then forward it to cloud auth as `credential_base64`. Keystone must not decode
the key, derive `site_id`, extract a secret, or enforce the key's internal
encoding format; the cloud AuthService owns credential validation.

### 7.6 Require Admin Permission

Cloud sync trigger APIs should require an authenticated admin role. Batch sync can
move large files and consume cloud resources, so it should not be available to
unauthenticated or operator-only clients.

## 8. Error Handling

### 8.1 Single Trigger Responses

| HTTP Status | UI Handling |
|-------------|-------------|
| `202` | Show queued state and start polling |
| `400` | Show validation error, usually not eligible |
| `404` | Episode no longer exists; refresh page |
| `409` | Already synced, queued, or in progress; refresh status |
| `429` | Queue full; ask user to retry later |
| `503` | Sync worker not configured or not running |
| `500` | Show generic failure and keep action available |

### 8.2 Status Polling

Stop polling when:

- latest status is `completed`
- latest status is `failed`
- route changes
- drawer/page unmounts
- max polling duration is reached

Recommended maximum polling duration for a detail page is 2 minutes. Large MCAP
uploads can take longer, so after timeout the UI should switch to:

> Sync is still running in the background. Check Cloud Sync Center for details.

## 9. Implementation Phases

### Phase 1: Minimal Single-Episode Control

- Add `synapse/src/api/sync.js`.
- Add Cloud Sync panel to episode detail.
- Add single trigger and status polling.
- Add basic error handling.

Acceptance criteria:

- Unsynced approved episode can be manually enqueued.
- UI shows queued/syncing/completed/failed states.
- Already synced episode cannot be triggered again.
- Worker unavailable state disables the action.

### Phase 2: Cloud Sync Center

- Add a dedicated Cloud Sync Center route/page.
- Add batch trigger with confirmation.
- Add episode summary table with status filtering and pagination.
- Add per-episode sync history drill-down for raw `sync_logs`.
- Add failed-job focused view.
- Remove Cloud Sync operation/entry cards from data production statistics.

Acceptance criteria:

- Admin can enqueue all eligible unsynced episodes.
- UI clearly states the operation runs in background.
- Cloud Sync Center defaults to one row per episode.
- Raw sync logs can be inspected from an episode row.
- Data production statistics no longer exposes cloud sync operation or entry
  cards.

### Phase 3: Backend API Enhancements

- Add episode sync summary and per-episode history endpoints.
- Add latest sync summary to episode list/detail responses.
- Add batch trigger body with `force` and filters.
- Extend sync config response with read-only non-sensitive values.
- Add admin auth guard for sync APIs.

Acceptance criteria:

- Episode list can render sync status without N+1 requests.
- Batch retry semantics are explicit.
- Cloud Sync Center can show worker runtime details without reading `.env`.
- Cloud sync operations are permission controlled.

## 10. Open Decisions

| Decision | Recommended Default |
|----------|---------------------|
| Should batch trigger force failed retries? | No for MVP; add explicit `force=true` later |
| Should sidecar JSON be uploaded to cloud as an object? | Not in MVP; current backend only sends sidecar as `raw_tags` |
| Should operators see retry buttons? | No; admin-only for sync controls |
| Should batch sync use current statistics filters? | Only after backend supports filter parameters |
| Should long-running uploads show percentage? | No; backend does not expose progress |
| Should `.env` sync config be editable in Synapse? | Not now; keep as TODO after read-only config visibility |
