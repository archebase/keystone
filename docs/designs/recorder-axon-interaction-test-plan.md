# Recorder / Axon Interaction Test Plan

This document defines the Keystone-side test coverage for recorder WebSocket and RPC behavior. The goal is to find protocol, state recovery, and duplicate-dispatch bugs with tests. If a test fails because product behavior is wrong, fix implementation code; do not weaken assertions only to make tests pass.

## Scope

Covered now:

1. Keystone handler/unit behavior around recorder state, RPC result handling, task state transitions, connection replacement, and disconnect recovery.
2. Keystone-to-fake-Axon WebSocket interaction behavior using real websocket connections, including connect-time state sync, reconnect, late messages, RPC response shape, timeout, and duplicate config prevention.

Out of scope for this document:

- Synapse browser/UI tests.
- Axon C++ implementation tests.
- Network stack integration outside in-process `httptest` WebSocket servers.

## Invariants

- Config may be sent only when Keystone has a trusted state snapshot for the current recorder connection and that snapshot is `idle`.
- A recorder connection starts as unsynced. `state_synced` becomes true only after a non-empty recorder state snapshot from `state_update` or `get_state`.
- If the recorder is `ready`, `recording`, or `paused`, any new `config` request must be rejected before Keystone sends an RPC.
- A ping timeout or recorder WebSocket close may revert DB tasks from `ready` or `in_progress` to `pending`, but reconnect state sync must restore the same task when Axon still reports it.
- A task that is reverted to `pending` during reconnect must not be reconfigured while Axon still reports `ready` with that task id.
- Old/replaced recorder connections must not update DB state, satisfy pending RPCs, or override current connection state.
- RPC success controls DB transitions. `success:false`, timeout, or disconnected recorder must not advance/revert task state as if the command succeeded.
- Repeated state events for the same task must be idempotent.

## Keystone Handler / Unit Test Matrix

### State snapshot and DB reconciliation

- [x] `state_update ready + task_id` advances `pending -> ready` and sets `ready_at`.
- [x] `state_update recording + task_id` advances `pending/ready -> in_progress` and sets `started_at`.
- [x] `state_update paused + task_id` advances `pending/ready -> in_progress` and sets `started_at`.
- [x] `get_state ready` with `task_config.task_id` advances `pending -> ready`.
- [x] `get_state recording` with top-level `task_id` advances `pending/ready -> in_progress`.
- [x] `get_state paused` with `task_config.task_id` advances `pending/ready -> in_progress`.
- [x] `idle + task_id` does not advance or revert DB state.
- [x] non-idle state without `task_id` marks connection synced but does not change DB state.
- [x] empty state snapshot keeps connection unsynced.
- [x] duplicate ready/recording snapshots are idempotent and do not corrupt timestamps.

### State API contract

- [x] disconnected `/state` returns `connected=false`, `state_synced=false`, `syncing=false`.
- [x] connected but unsynced `/state` returns `connected=true`, `state_synced=false`, `syncing=true`.
- [x] connected and synced `/state` returns `state_synced=true`, `syncing=false`.
- [x] `/devices` includes `state_synced` for each connected recorder.

### Config gating

- [x] `config` before initial state snapshot returns 409 and sends no RPC.
- [x] `config` while synced recorder is `ready` returns 409 and sends no RPC.
- [x] `config` while synced recorder is `recording` returns 409 and sends no RPC.
- [x] `config` while synced recorder is `paused` returns 409 and sends no RPC.
- [x] `config` while synced recorder state is unknown/non-idle returns 409 and sends no RPC.
- [x] `config` after synced `idle` sends RPC and advances `pending -> ready` only on success.
- [x] `config` with `success:false` keeps task `pending`.
- [x] `config` timeout keeps task `pending`.
- [x] `config` disconnected returns 404 and keeps task `pending`.

### Command RPC side effects

- [x] `begin` with `success:false` keeps task unchanged.
- [x] `begin` success advances `pending/ready -> in_progress` and is idempotent for already `in_progress`.
- [x] `begin` timeout or disconnect keeps task unchanged.
- [x] `cancel` success reverts `ready/in_progress -> pending` only for provided task id.
- [x] `cancel` `success:false`, timeout, or disconnect keeps task unchanged.
- [x] `clear` success reverts `ready -> pending`, does not revert `in_progress`, and sends no task payload to Axon.
- [x] `clear` `success:false`, timeout, or disconnect keeps task unchanged.
- [x] `pause`, `resume`, `finish`, and `quit` forward the RPC action and do not mutate task state directly.
- [x] `get_stats` disconnected returns HTTP 200 with `connected=false` and empty data.
- [x] `get_stats` Axon failure returns HTTP 200 with `connected=true`, data, and error message.
- [x] `get_stats` timeout returns HTTP 504.

### Disconnect and replacement

- [x] messages from replaced recorder connections are ignored for `state_update` and `config_applied`.
- [x] late `rpc_response` from replaced connections cannot satisfy current pending RPCs.
- [x] recorder disconnect reverts matching `ready` tasks to `pending` without sending recorder RPCs.
- [x] recorder disconnect reverts matching `in_progress` tasks to `pending` without sending recorder RPCs.
- [x] recorder disconnect does not revert `pending`, `completed`, `failed`, deleted, or other-device tasks.
- [x] old connection disconnect after replacement does not revert tasks for the current connection.

## Keystone <-> Fake Axon WebSocket Test Matrix

### Connect-time sync

- [x] On WebSocket connect, Keystone sends `get_state`; fake Axon replies `idle`; `/state` becomes synced idle.
- [x] Fake Axon sends `state_update ready + task_id` before `get_state` response; task advances to `ready` and stays correct when `get_state` later returns same state.
- [x] `get_state` returns `ready + task_id`; task advances to `ready`; direct `config` is rejected as busy.
- [x] `get_state` returns `recording + task_id`; task advances to `in_progress`; direct `config` is rejected as busy.
- [x] `get_state` timeout leaves the connection unsynced, `/state` keeps `syncing=true`, and `config` is rejected until a valid `state_update` arrives.
- [x] `get_state` returns `success:false`; connection remains unsynced and `config` is rejected until a valid `state_update` arrives.
- [x] `state_update` with empty current state does not mark connection synced.

### Reconnect after timeout / close

- [x] Ready task path: task is `ready`; recorder WebSocket closes; DB reverts to `pending`; fake Axon reconnects and reports `ready + same task_id`; DB returns to `ready`; no `config` RPC is sent during the window.
- [x] Recording task path: task is `in_progress`; recorder WebSocket closes; DB reverts to `pending`; fake Axon reconnects and reports `recording + same task_id`; DB returns to `in_progress`; no `config` RPC is sent during the window.
- [x] Idle-after-reconnect path: task is reverted to `pending`; fake Axon reconnects and reports `idle`; `config` is allowed and sends exactly one RPC.
- [x] Reconnect replaces a still-active old connection; old connection late `state_update`, `config_applied`, and `rpc_response` do not mutate task state or satisfy new requests.

### RPC action protocol

- [x] `config` sends action `config` with `task_config` payload exactly once when recorder is synced idle.
- [x] `clear` sends action `clear` without UI task payload even when HTTP body includes `task_id`.
- [x] `begin`, `cancel`, `finish`, `pause`, `resume`, `quit`, and `get_stats` send the expected action names and payloads.
- [x] Axon response with unmatched `request_id` is ignored and does not unblock Keystone RPC waiters.
- [x] Axon response `success:false` returns HTTP 200 with the RPC response body but does not mutate DB state.
- [x] Missing Axon response causes Keystone HTTP timeout and no DB mutation.

## Implementation Notes

- Prefer deterministic fake Axon helpers over sleeps. Helpers should expose channels for inbound RPC requests and send explicit RPC responses by request id.
- Use real `httptest` WebSocket servers for layer 2 tests so the Keystone read loop, connect-time `get_state`, and disconnect defers are exercised.
- Keep DB fixtures small but include `robots`, `workstations`, and `tasks` when testing disconnect joins.
- Use `ResponseTimeout` values short enough for timeout tests, but avoid making the suite flaky.
- Keep assertions strict: status code, response body, sent action, request payload, DB status, and timestamp side effects should be checked where relevant.
