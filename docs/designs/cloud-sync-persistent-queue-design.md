<!--
SPDX-FileCopyrightText: 2026 ArcheBase

SPDX-License-Identifier: MulanPSL-2.0
-->

# Cloud Sync Persistent Queue Design

## 1. Problem

Synapse Cloud Sync Center exposes a queued status:

```text
Failed -> Queued -> Syncing -> Synced
```

However, Keystone manual retry does not currently persist a queued state.

When an admin retries failed sync jobs:

1. Synapse calls `POST /api/v1/sync/episodes/:id`.
2. Keystone validates the episode and calls `SyncWorker.EnqueueEpisodeManual()`.
3. The worker places the episode into an in-memory channel.
4. When a worker goroutine claims the job, Keystone writes or updates
   `sync_logs.status = 'in_progress'` directly.

As a result, the API request can be accepted and the upload can eventually
complete, while Synapse never observes `sync_logs.status = 'pending'`. With
`KEYSTONE_SYNC_MAX_CONCURRENT=2`, retrying four failed episodes often appears as:

```text
2 syncing, 2 still failed, then all synced
```

instead of:

```text
2 syncing, 2 queued, then all synced
```

This is a backend state-model gap, not a configuration issue.

## 2. Current Behavior

### 2.1 Manual Retry

Current single episode retry path:

```text
POST /sync/episodes/:id
        -> SyncHandler.TriggerEpisodeSync()
        -> SyncWorker.EnqueueEpisodeManual()
        -> enqueueCh
        -> jobCh
        -> acquireSyncLogWithMode()
        -> sync_logs.status = 'in_progress'
```

Important properties:

- `pending` is not written before the API returns `202 Accepted`.
- In-memory `enqueuedEpisode` prevents duplicate scheduling only within one
  running Keystone process.
- `manual=true` intentionally bypasses exhausted automatic retry and active
  backoff checks.
- An episode with latest `pending` or `in_progress` is rejected as already active.

### 2.2 Automatic Retry

The polling worker periodically:

1. Finds failed rows whose `next_retry_at` is due and whose `attempt_count` is
   below `MaxRetries`.
2. Dispatches them to worker goroutines.
3. Reuses the failed `sync_logs` row and changes it directly to `in_progress`.

Automatic retry also does not create an observable queued period.

### 2.3 Frontend Listing

Cloud Sync Center lists one row per episode using the latest `sync_logs` row.
Its queued count is the number of latest rows whose status is `pending`.

Therefore, a job that only exists in memory cannot be counted as queued.

## 3. Goals

- Make `sync_logs.status = 'pending'` a real, persistent queued state.
- Make manual retry visible immediately after the API returns `202 Accepted`.
- Preserve manual retry semantics: operators can retry exhausted or backoff
  failures explicitly.
- Preserve automatic retry limits and backoff behavior.
- Allow queued work to recover after Keystone restarts.
- Keep the change scoped to the existing `sync_logs` model unless stronger
  queue semantics become necessary later.

## 4. Non-Goals

- Do not introduce a new `sync_queue` table in the first implementation.
- Do not change cloud upload protocol behavior.
- Do not change episode QA eligibility rules.
- Do not make batch trigger a forced retry-all-failures operation unless a
  separate API contract is designed.

## 5. Recommended Design

Use the existing `sync_logs.status = 'pending'` as the durable sync queue state.

### 5.1 State Model

```text
No sync log
    -> pending
    -> in_progress
    -> completed

failed
    -> pending
    -> in_progress
    -> completed

failed
    -> pending
    -> in_progress
    -> failed
```

Automatic retry remains bounded:

```text
failed(attempt_count < max_retries, next_retry_at due)
    -> pending
```

Manual retry remains operator-forced:

```text
failed(exhausted or still in backoff)
    -> new pending attempt chain
```

### 5.2 Manual Enqueue

`EnqueueEpisodeManual()` should persist a pending row before returning success.

The operation must be transactionally protected:

1. Lock the parent `episodes` row with `FOR UPDATE`.
2. Load the latest `sync_logs` row for the episode.
3. Reject latest `pending` or `in_progress`.
4. Reject if `episodes.cloud_synced = TRUE`.
5. If the latest row is `failed` and manual retry should create a fresh chain,
   insert a new `sync_logs` row with `status = 'pending'`.
6. If there is no sync log, insert a new `pending` row.
7. Commit before returning `202 Accepted`.

The in-memory enqueue channel should become an acceleration path only. If the
channel is full after the pending row has been persisted, the API can still
return accepted because the polling loop can recover the pending job.

### 5.3 Worker Dispatch

The worker loop should process DB-backed queued work before discovering new work:

1. Dispatch latest `pending` sync logs.
2. Promote due failed rows to `pending`, then dispatch them.
3. Discover approved, unsynced episodes with no active/latest completed log.

This makes the queue restart-safe. If Keystone crashes after writing `pending`
but before placing the job into memory, the next polling cycle will pick it up.

### 5.4 Claiming Pending Rows

Worker goroutines should claim a pending row using an atomic DB transition:

```sql
UPDATE sync_logs
SET status = 'in_progress',
    started_at = ?,
    source_path = ?,
    error_message = NULL,
    duration_sec = NULL,
    completed_at = NULL,
    next_retry_at = NULL,
    attempt_count = ?
WHERE id = ?
  AND status = 'pending'
```

The worker must check `RowsAffected == 1`.

This protects against duplicate execution if multiple dispatchers or future
Keystone instances see the same pending row.

### 5.5 Attempt Count Semantics

Recommended semantics:

- A fresh pending row uses `attempt_count = 0`; claim changes it to `1`.
- A retryable due failed row can be reused by changing `failed -> pending` and
  preserving its current `attempt_count`; claim increments it to the next
  attempt number.
- `in_progress.attempt_count >= 1` means an upload attempt has been claimed by a
  worker.
- `failed.attempt_count` records the number of failed attempts in the current
  attempt chain.
- Manual retry after exhausted/backoff failure creates a new row with
  `attempt_count = 0`, then claim changes it to `1`.

This preserves the existing automatic retry counter while still making fresh
manual attempt chains clearly visible.

### 5.6 Frontend Behavior

Synapse can keep the current status model:

- `pending`: queued
- `in_progress`: syncing
- `completed`: synced
- `failed`: failed

After a successful manual retry:

1. Optimistically mark the affected row as `pending`.
2. Refresh counts and the current page.
3. Poll every 2 to 3 seconds while any latest row is `pending` or `in_progress`.
4. Stop polling once no active work remains.

The frontend should continue treating the backend summary endpoint as the source
of truth.

## 6. Risks and Mitigations

### 6.1 Duplicate Pending Rows

Risk: two admins retry the same failed episode at the same time.

Mitigation:

- Lock the `episodes` row before inspecting or inserting `sync_logs`.
- Reject latest `pending` or `in_progress`.
- Keep the in-memory duplicate marker as an optimization, not as the only guard.

### 6.2 Pending Rows Stuck Forever

Risk: API writes `pending`, Keystone crashes before enqueueing in memory.

Mitigation:

- Poll and dispatch latest `pending` rows from the database.
- Treat in-memory enqueue as best-effort acceleration.

### 6.3 In-Progress Rows Stuck Forever

Risk: Keystone crashes after claiming a row as `in_progress`.

Mitigation:

- Add stale `in_progress` recovery in a follow-up step.
- A row can be considered stale when `started_at` exceeds a conservative timeout
  derived from request timeout, OSS timeout, and a buffer.
- Stale rows can be marked `failed` with a retryable error and `next_retry_at`
  set by the normal backoff function.

This risk already exists today; making `pending` durable makes it more visible.

### 6.4 Automatic and Manual Retry Confusion

Risk: manual retry bypass rules accidentally affect automatic retry.

Mitigation:

- Keep explicit `manual` mode in the enqueue/acquire path.
- Automatic retry can only promote failed rows when `attempt_count < MaxRetries`
  and `next_retry_at <= NOW()`.
- Manual retry can create a new pending attempt chain even when backoff is active
  or the latest chain is exhausted.

### 6.5 Queue Full API Semantics

Risk: current code returns `429 queue_full` when the in-memory channel is full.

Mitigation:

- After durable pending is introduced, a full channel should not fail the API if
  the DB write succeeded.
- Return `202 Accepted`; the polling loop will dispatch the pending job later.

### 6.6 Listing Sort Semantics

Risk: `started_at` currently acts as both queue time and start time.

Mitigation:

- For the first implementation, keep using `started_at` as the row ordering time.
- If stronger audit semantics are needed, add `queued_at` in a later migration.

### 6.7 Multi-Instance Keystone

Risk: future multi-instance deployments may have multiple workers scanning the
same pending rows.

Mitigation:

- Use conditional `pending -> in_progress` updates and require
  `RowsAffected == 1`.
- Avoid assuming the in-memory marker is globally authoritative.

## 7. Implementation Plan

### Phase 1: Durable Manual Queue

- Add a worker method to create or reuse a pending sync log transactionally.
- Change `EnqueueEpisodeManual()` to persist pending before returning success.
- Dispatch latest pending rows during each poll.
- Keep existing `sync_logs` schema.
- Keep existing summary/history APIs.

Expected user-visible behavior:

```text
Retry 4 failed episodes with MaxConcurrent=2
    -> 2 in_progress, 2 pending
    -> pending rows become in_progress as workers free up
    -> completed or failed
```

### Phase 2: Automatic Retry Alignment

- Change due failed-row retry from direct dispatch to `failed -> pending`.
- Preserve `MaxRetries` and `next_retry_at` checks.
- Ensure exhausted automatic retry rows remain failed until manually retried.

### Phase 3: Stale In-Progress Recovery

- Detect `in_progress` rows older than a conservative timeout.
- Mark stale rows failed with a clear error message.
- Let the normal retry/backoff mechanism decide when to retry.

### Phase 4: Frontend Polling Polish

- Add short polling while active work exists.
- Keep retry buttons visible only on latest failed rows.
- Keep backend summary results as the source of truth after optimistic updates.

## 8. Test Plan

### Backend Unit Tests

- Manual retry creates a pending row for a failed exhausted episode.
- Manual retry rejects an episode whose latest row is pending.
- Manual retry rejects an episode whose latest row is in_progress.
- Two concurrent manual retries result in one pending row.
- Polling dispatches existing pending rows after process restart simulation.
- Worker claim changes exactly one pending row to in_progress.
- Automatic retry promotes due failed rows to pending only below MaxRetries.
- Exhausted automatic failures are not promoted without manual retry.

### Backend Integration Tests

- Retry four failed episodes with `MaxConcurrent=2`; observe two active and two
  queued before completion.
- Kill and restart Keystone after pending rows are written; verify pending rows
  are dispatched after restart.
- Simulate upload failure; verify failed status, attempt count, and
  `next_retry_at`.

### Frontend Verification

- Cloud Sync Center queued count increases immediately after retry.
- Rows transition from failed to queued to syncing to synced or failed.
- Refreshing the page does not lose queued rows.
- Polling stops when no pending or in_progress rows remain.

## 9. Recommendation

Implement Phase 1 first. It fixes the operator-visible problem with a small,
contained backend change and does not require a new database table.

Do not ship durable pending without DB-backed duplicate protection and polling
recovery. Those two pieces are required for correctness; otherwise the system
can trade an invisible in-memory queue for stuck or duplicated persistent queue
entries.
