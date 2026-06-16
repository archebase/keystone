<!--
SPDX-FileCopyrightText: 2026 ArcheBase

SPDX-License-Identifier: MulanPSL-2.0
-->

# Task Recorder Upload Guard Design

**Status:** proposed

**Scope:** Keystone recorder RPC APIs, recording callbacks, Transfer reconciliation, and Synapse operator guardrails.

## 1. Problem

The observed failure for `task_20260612_052143_068_76_9a4d20e3` is not a task
database status regression. Keystone correctly kept the DB task in
`uploading` and skipped `uploading -> in_progress` after the second `begin`.

The unsafe part is earlier: Keystone still sent recorder-side `config` and
`begin` RPCs for the same `task_id` while the DB task was already `uploading`.
That allowed the recorder to start a second local recording using the same task
name while axon_transfer was uploading the original MCAP file.

The important log shape is:

```text
10:03:41 finish callback for task_..._76
10:03:41 DB status in_progress -> uploading
10:03:43 transfer upload_started for task_..._76
10:03:45 recorder config applied for task_..._76 again
10:03:56 recorder ready -> recording for task_..._76 again
10:03:56 DB begin transition skipped because current_status=uploading
10:06:18 second finish callback for task_..._76
10:06:18 upload_request sent again
10:06:18 transfer reports upload_not_found
10:07:41 original upload completes and is ACKed
```

The target fix is therefore to stop illegal recorder commands before they leave
Keystone, and to make recording finish callbacks idempotent once upload has
already started or completed.

## 2. Current Behavior

- `POST /recorder/:device_id/config` checks recorder connectivity, transfer
  connectivity, fresh recorder state, and recorder idle state. It does not
  require the DB task to be `pending`.
- `GET /tasks/:id/config` validates task metadata but does not require the task
  to be `pending`.
- `POST /recorder/:device_id/begin` sends the RPC first, then tries to advance
  DB status from `pending` or `ready` to `in_progress`. If the task is already
  `uploading`, the DB update is skipped, but the recorder has already begun.
- `POST /callbacks/finish` calls `markOwnedTaskUploading`, which currently
  accepts `pending`, `ready`, `in_progress`, and `uploading`. A duplicate finish
  callback for an `uploading` task therefore triggers another `upload_request`.
- axon_transfer does not resend recorder finish callbacks. Recorder finish is a
  recorder-side HTTP callback. Transfer only reports upload lifecycle events and
  status snapshots over WebSocket.

## 3. Invariants

- A task may be configured on a recorder only while the authoritative DB task
  status is `pending`.
- A task may be begun on a recorder only while the authoritative DB status is
  `ready`, with a narrow `pending` compatibility case for a recorder that is
  already ready for the same task.
- `uploading`, `completed`, `failed`, and `cancelled` are never valid sources
  for recorder `config` or `begin`.
- A finish callback for a task that is already `uploading` or `completed` is
  idempotent and must not send another `upload_request`.
- Transfer recovery must be driven by transfer `connected/status` reconciliation
  and persisted upload state, not by hoping for another recorder finish
  callback.
- `uploaded_wait_ack` must use the same verified ACK path as
  `upload_complete`; Keystone must not blindly ACK a task just because its ID
  appears in `waiting_ack_task_ids`.

## 4. API State Rules

### `GET /tasks/:id/config`

This is a UX and prevention layer. It should reject tasks whose DB status is not
`pending`.

| DB status | Result |
| --- | --- |
| `pending` | Return task config. |
| `ready` | `409 task_not_configurable`; refresh UI state. |
| `in_progress` | `409 task_not_configurable`; recording has started. |
| `uploading` | `409 task_not_configurable`; upload owns the task. |
| `completed` | `409 task_not_configurable`; terminal. |
| `failed` | `409 task_not_configurable`; terminal unless a future explicit retry flow exists. |
| `cancelled` | `409 task_not_configurable`; terminal. |

This endpoint is not the safety boundary. Operators or automation can bypass it
and call recorder RPC APIs directly, so the recorder RPC handlers must enforce
the same rules before sending WebSocket messages.

### `POST /recorder/:device_id/config`

This is a backend hard gate before the `config` RPC is sent.

Required checks:

- request contains `task_config.task_id`;
- task exists, is not deleted, belongs to `device_id`, and is `pending`;
- recorder connection exists and has a synced, fresh state snapshot;
- recorder state is `idle`;
- transfer connection exists when the existing config path requires it.

Rejected statuses should return `409` with a stable code such as
`task_not_configurable`. No recorder RPC should be sent on rejection.

After a successful recorder RPC, Keystone may still perform the existing
`pending -> ready` transition. If the update affects zero rows, Keystone should
log the current status and return success for the RPC result only if the
pre-RPC gate already passed; zero rows after the RPC should be rare and worth
logging.

### `POST /recorder/:device_id/begin`

This is also a backend hard gate before the `begin` RPC is sent.

Allowed cases:

| DB status | Recorder state | Recorder task_id | Result |
| --- | --- | --- | --- |
| `ready` | `ready` | same task | Send `begin`; then mark `in_progress`. |
| `pending` | `ready` | same task | Send `begin`; then mark `in_progress`. |

The `pending` compatibility case exists for transient Keystone/recorder state
skew where the recorder has already accepted config and reports ready, but the
DB row has not reached `ready` or was rolled back to `pending` after a
disconnect.

Rejected cases:

| DB status | Result |
| --- | --- |
| `pending` with recorder not ready for the same task | `409 task_not_beginable`. |
| `in_progress` | `409 task_not_beginable`; already recording. |
| `uploading` | `409 task_not_beginable`; upload owns the task. |
| `completed` | `409 task_not_beginable`; terminal. |
| `failed` | `409 task_not_beginable`; terminal unless a future explicit retry flow exists. |
| `cancelled` | `409 task_not_beginable`; terminal. |

The key ordering requirement is: validate DB ownership/status and recorder
cached/refreshed state before sending `begin`. A skipped DB transition after a
successful RPC is too late to protect the recorder.

## 5. Finish Callback Semantics

`POST /callbacks/finish` should be idempotent for already-uploading or
already-completed tasks.

| Current DB status | HTTP result | DB transition | Send `upload_request` |
| --- | --- | --- | --- |
| `pending` | `200` | `pending -> uploading` | Yes. |
| `ready` | `200` | `ready -> uploading` | Yes. |
| `in_progress` | `200` | `in_progress -> uploading` | Yes. |
| `uploading` | `200` | No-op | No. |
| `completed` | `200` | No-op | No. |
| `failed` | `409` | No-op | No. |
| `cancelled` | `409` | No-op | No. |
| task not found or not owned by device | `409` | No-op | No. |

This design deliberately does not try to distinguish a duplicate callback from
an illegal second same-name recording. The current callback payload has no
session ID, recording generation, or MCAP object identity that can prove which
physical recording produced the callback. The safe rule is status-based
idempotency plus stricter prevention at `config` and `begin`.

Implementation note: the upload-request branch should be reached only when the
finish handler actually changed the task into `uploading` from
`pending/ready/in_progress`. If the current status is already `uploading` or
`completed`, return `200` with `upload_request_sent=false`.

## 6. Transfer Reconciliation

Recorder callbacks and transfer recovery solve different problems:

- Recorder finish callback tells Keystone that local recording ended and upload
  should be requested.
- Transfer `connected/status` tells Keystone what axon_transfer currently knows
  about upload state.
- Transfer should not invent or replay recorder finish callbacks.

### `upload_complete` and `uploaded_wait_ack`

`upload_complete` already runs the verified ACK flow:

1. verify expected objects exist in S3/MinIO;
2. update Keystone episode/task metadata;
3. send `upload_ack` to axon_transfer.

When transfer reports a task in `uploaded_wait_ack`, Keystone may only ACK it by
reusing that same verified ACK flow. It must have enough data to identify and
verify the uploaded object, such as the matching record in `uploads[]` with
`task_id`, status, object key/S3 key, file size, and checksum where available.

If the status snapshot only contains `waiting_ack_task_ids` and lacks a matching
`uploads[]` record, Keystone should not ACK. It may send `status_query` to ask
for a fuller snapshot.

### Re-sending `upload_request`

Keystone may re-send `upload_request` automatically only for a narrow recovery
case:

- DB task status is `uploading`;
- transfer is connected;
- transfer status/`uploads[]` does not show the task as `pending`, `active`,
  `retry-wait`, or `uploaded_wait_ack`;
- Keystone has a previous `error_message` that clearly means the original
  `upload_request` was not sent, failed to send, or timed out.

Keystone should not auto retry when the previous transfer response was
`upload_not_found`. That error means the transfer scanner did not find a local
MCAP for the task, so blindly resending can loop and obscure the real cause.

For this round, use process-local in-flight and cooldown maps to prevent
reconciliation storms. Do not add a DB schema migration only for retry
bookkeeping unless later requirements need cross-process persistence.

### Status Snapshot Handling

Keystone should parse and cache enough of transfer `connected/status` to make
the reconciliation decisions above:

- counts: `pending_count`, `active_count`, `uploading_count`,
  `retry_wait_count`, `waiting_ack_count`, `completed_count`, `failed_count`;
- `waiting_ack_task_ids`;
- `uploads[]` records, including at least `task_id`, `status`, `s3_key` or
  `object_key`, `file_size_bytes`, and checksum fields when present.

## 7. Synapse Guardrails

Synapse operator pages are an auxiliary guard, not the source of truth.

Expected UI behavior:

- refresh task state immediately before prepare/config actions;
- request task config only for `pending` tasks;
- disable or hide prepare/config controls for `ready`, `in_progress`,
  `uploading`, `completed`, `failed`, and `cancelled`;
- on `task_not_configurable` or `task_not_beginable`, show a concise message
  and refresh the task list/device state;
- keep auto-prepare logic constrained to `pending` tasks.

These guardrails reduce accidental operator actions, but backend hard gates are
still required for correctness.

## 8. Test Plan

### Keystone recorder RPC tests

- `config` sends no RPC and returns `409 task_not_configurable` for
  `ready/in_progress/uploading/completed/failed/cancelled`.
- `config` sends RPC for owned `pending` task when recorder is idle and transfer
  is connected.
- `begin` sends RPC for owned `ready` task when recorder state is ready for the
  same `task_id`.
- `begin` sends RPC for owned `pending` task only when recorder state is ready
  for the same `task_id`.
- `begin` sends no RPC for `uploading/completed/failed/cancelled/in_progress`.

### Keystone callback tests

- finish callback transitions `pending`, `ready`, and `in_progress` to
  `uploading` and sends exactly one `upload_request`.
- finish callback for `uploading` returns `200`, does not change DB state, and
  does not send `upload_request`.
- finish callback for `completed` returns `200`, does not change DB state, and
  does not send `upload_request`.
- finish callback for `failed` and `cancelled` returns `409` and sends no
  `upload_request`.

### Transfer reconciliation tests

- `uploaded_wait_ack` with a complete `uploads[]` record uses the same verified
  ACK flow as `upload_complete`.
- `waiting_ack_task_ids` without a matching upload record does not send ACK and
  may trigger `status_query`.
- DB `uploading` plus a send-failure `error_message` and no active transfer
  upload can re-send `upload_request` once per cooldown window.
- `upload_not_found` does not auto re-send `upload_request`.

### Synapse checks

- operator prepare/config buttons are disabled for non-`pending` statuses;
- auto-prepare skips non-`pending` tasks;
- backend `409` codes refresh local task state and show a friendly message.

## 9. Non-goals

- Do not introduce a recorder session ID or recording generation in this round.
- Do not change MCAP naming rules in this round.
- Do not add a DB schema migration solely for reconciliation cooldown state.
- Do not make axon_transfer resend recorder finish callbacks.
- Do not treat `upload_not_found` as an automatic retryable condition.

## 10. Future Work

- Add a recorder session ID or recording generation to `config`, `begin`, start
  callback, finish callback, sidecar metadata, and upload records. That would
  allow Keystone to distinguish duplicate callbacks from a second physical
  recording with the same task ID.
- Persist structured upload request state instead of encoding retry decisions
  in `tasks.error_message`.
- Add a transfer-side startup flush for `uploaded_wait_ack` records so Keystone
  can reconcile completed uploads immediately after reconnect.
