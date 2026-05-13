<!--
SPDX-FileCopyrightText: 2026 ArcheBase

SPDX-License-Identifier: MulanPSL-2.0
-->

# Production Units (Order / Batch / Task / Episode) Design (Edge)

**Scope:** Order · Batch · Task · Episode lineage (Edge)  

## 1. Purpose and sources of truth

This document defines the **public contract** and **core constraints** for production units on Keystone Edge. It is used to:

- Guide integration and evolution across Keystone / Synapse / Axon
- Ensure the Edge lineage (Order→Batch→Task→Episode) stays consistent, auditable, and extensible

---

## 2. Background, goals, and non-goals

### 2.1 Background

Production units turn a “production request” into “executable tasks”, and persist capture artifacts (MCAP + sidecar) as traceable Episodes to support downstream QA, sync, and search.

### 2.2 Goals

- **Complete lineage**: each Episode is traceable to a single Task, Batch, and Order, plus Scene/Subscene/SOP and workstation information.
- **Runnable Edge closed-loop**: Tasks can be generated and prepared (ready), recording/upload can be triggered, Episodes are persisted, and ACKs are confirmed.
- **Idempotent and recoverable**: upload/ACK retries do not create duplicate Episodes or incorrect counters.

### 2.3 Non-goals

- This document does not define cloud sync (Capstone), detailed QA rules, or UI interaction details.
- This document does not define richer order fulfillment policies such as reservation, reclaim, split/merge, or SLA handling. It documents the current Edge policy for dispatch and completion.

---

## 3. Domain relationships and invariants

```
Organization ── owns ──► Order ── has many ──► Batch ── has many ──► Task ── produces ──► Episode
                              │                    │
                              │                    └── bound to one Workstation (batch dimension)
                              └── target_count, priority, scene_id, status ...
```

### 3.1 Core concepts

- **Order**: a production request (target quantity, scene, priority, status).
- **Batch**: a production batch for an Order on a workstation (lineage dimension; carries episode count and batch status).
- **Task**: an atomic execution unit (binds SOP/Scene/Subscene/Workstation, drives recording and upload).
- **Episode**: a record of capture artifacts (mcap/json paths, QA fields, cloud processing fields, etc.).

### 3.2 Core invariants (must hold long-term)

- **Referential integrity**: an Episode must be able to resolve back to a Task; a Task must link to an Order and a Batch; a Task must resolve Scene/Subscene/SOP.
- **Idempotency**: a given `task_id` maps to at most one non-deleted Episode; repeated uploads/callbacks must not create duplicate Episodes.
- **Counter consistency**: `batches.episode_count` represents the number of persisted Episodes in that batch (+1 only when a new Episode is created).

---

## 4. Data model (external semantics)

This document does not restate full table schemas; it only defines key field semantics (see the migration file for details).

- **Order**
  - `target_count`: the desired number of **completed** Tasks for the order (see §6.1). For supported batch dispatch APIs, it is also the hard cap for non-deleted task rows under the order.
  - `task_count`: a derived statistic: `COUNT(tasks)` under the order (non-deleted; includes all statuses).
  - `completed_count`: a derived statistic: number of Tasks with `status='completed'` (non-deleted).
  - `cancelled_count` / `failed_count`: derived statistics from Tasks (non-deleted).
  - `remaining_assignable`: derived dispatch capacity, defined as `target_count - task_count` for non-deleted tasks. It must never be treated as `target_count - completed_count` for dispatch.
  - Fulfillment is derived from completed task rows: an order is considered fulfilled when `target_count > 0 && completed_count >= target_count`, or when `orders.status = 'completed'`.
- **Batch**
  - `batch_id`: a human-readable ID (for display/traceability).
  - `episode_count`: the number of persisted Episodes (see 3.2).
- **Task**
  - `task_id`: a human-readable ID (device/log-side primary identifier semantics).
  - `status`: task state (see 6).
  - Denormalized fields: `batch_name/scene_name/subscene_name/factory_id/organization_id/initial_scene_layout` for filtering and display.
- **Episode**
  - `episode_id`: a human-readable ID (currently a UUID string in the implementation).
  - `mcap_path/sidecar_path`: object storage paths (written by Transfer Verified ACK).

---

## 5. HTTP API (production unit related)

### 5.1 Orders (`/api/v1/orders`)

| Method | Path | Notes |
|------|------|------|
| GET | `/orders` | List (non-deleted only) |
| POST | `/orders` | Create (default `status=created`) |
| GET | `/orders/:id` | Detail (includes `completed_count`) |
| PUT | `/orders/:id` | Update (`scene_id/name/target_count/priority/status/deadline/metadata`) |
| DELETE | `/orders/:id` | Soft delete (rejected if referenced by `batches/tasks/episodes`) |

**Design constraints:**

- **Deletion guard**: an Order referenced by the production lineage must not be deletable.
- **Target count update guard**: `target_count` must stay greater than or equal to current `completed_count`.
- **State transitions**: should converge to controlled transitions (current implementation allows any valid enum; the transition graph will be enforced later).

---

### 5.2 Batches (`/api/v1/batches`)

| Method | Path | Notes |
|------|------|------|
| GET | `/batches` | List (filters: `order_id/workstation_id/status/limit/offset`) |
| POST | `/batches` | **Create Batch + Tasks atomically** (`task_groups`) |
| GET | `/batches/:id` | Detail |
| GET | `/batches/:id/tasks` | List tasks under a batch |
| POST | `/batches/:id/tasks` | Declaratively adjust task quantities (`task_groups`) |
| PATCH | `/batches/:id` | **Cancel only** (`pending/active -> cancelled`) |
| POST | `/batches/:id/recall` | Recall (`active/completed -> recalled`) |
| DELETE | `/batches/:id` | Soft delete (only allowed when `status=cancelled`) |

**Design constraints:**

- **Dispatch / fulfillment guard**:
  - `POST /batches` creates a batch and its initial task rows only when the requested total quantity is less than or equal to the order's current `remaining_assignable`.
  - `POST /batches/:id/tasks` is a declarative per-group target quantity adjustment. It may insert new task rows, soft-delete pending task rows, or leave a group unchanged.
  - `POST /batches/:id/tasks` must validate the post-edit order total:
    `order_task_count - current_batch_task_count + edited_batch_target_count <= target_count`.
    This prevents repeated small edits from bypassing the order target.
  - Reductions and no-op updates are allowed when they respect locked-task rules; task insertion is rejected once there is no remaining assignable quantity.
  - Admin bulk creation is a UI/client workflow over `POST /batches`, but it must validate the aggregate requested quantity against the same remaining assignable value before sending requests.
- **Cancellation semantics (PATCH)**: `PATCH /batches/:id` only supports transitioning to `cancelled`. It cascades cancellation to tasks in `pending/ready/in_progress` under the batch, and best-effort notifies Axon recorder:
  - `ready` tasks → recorder `clear`
  - `in_progress` tasks → recorder `cancel {task_id}`
- **Automatic advancement**:
  - `pending -> active`: automatic (triggered when a task under the batch reaches a terminal state; see §6.2).
  - `active -> completed`: automatic (when **all** non-deleted tasks are terminal: `completed/failed/cancelled`; see §6.2).
- **Recall semantics (POST)**: recall is a separate endpoint (`POST /batches/:id/recall`), not a `PATCH` status update.

---

### 5.3 Tasks (`/api/v1/tasks`)

#### 5.3.1 Create (`POST /tasks`)

`POST /tasks` exists for backwards compatibility. It creates Tasks per **(order + workstation)**:

- **Request fields**: `order_id`, `sop_id`, `subscene_id`, `workstation_id`, optional `quantity` (default 1, range 1..1000)
- **Batch association (implicit)**:
  - Prefer reusing a batch under the same `(order_id, workstation_id)` with status `pending/active`;
  - Otherwise create a new `pending` batch.

**Quantity constraint (current implementation):**

- `POST /tasks`: legacy behavior caps by **existing task rows** (non-deleted) under the order: `existing_tasks + quantity <= target_count`.
- `POST /batches`: caps by **existing task rows** (non-deleted) under the order: `existing_tasks + requested_total_quantity <= target_count`.
- `POST /batches/:id/tasks`: caps by the post-edit order task total:
  `order_task_count - current_batch_task_count + edited_batch_target_count <= target_count`.
- All production dispatch APIs should converge on this task-row quota definition.

#### 5.3.2 Query and config

| Method | Path | Notes |
|------|------|------|
| GET | `/tasks` | List (filters: `workstation_id/status/limit/offset`) |
| GET | `/tasks/:id` | Detail (includes `episode` if linked) |
| PUT | `/tasks/:id` | Status update (restricted transitions; see §6.2) |
| GET | `/tasks/:id/config` | Generate recorder config (requires workstation robot + collector bindings) |

---

## 6. State machines (design constraints + current implementation)

### 6.1 Order states

- **State set**: `created` | `in_progress` | `paused` | `completed` | `cancelled`
- **Auto-advance rules (completed-only)**:
  - `created -> in_progress`: when the order has **at least one** `completed` task.
  - `in_progress -> completed`: when `target_count > 0 && completed_count >= target_count`.
- **Order completion side-effects (current implementation)**:
  - Keystone finalizes still-open batches for the completed order:
    - cancels runnable tasks (`pending/ready/in_progress`) under batches in `pending/active`;
    - marks those batches `completed`;
    - best-effort notifies Axon recorder (`ready` tasks -> `clear`, `in_progress` tasks -> `cancel {task_id}`) when a recorder connection exists.
  - New task dispatch through batch creation or batch task insertion is blocked because `remaining_assignable <= 0`.

### 6.2 Task states

- **State set**: `pending` | `ready` | `in_progress` | `completed` | `failed` | `cancelled`
- **Prepare (pending→ready)**: triggered by UI/scheduler (currently via `PUT /tasks/:id`).
- **Run (ready→in_progress)**: triggered by UI/device workflow (currently via `PUT /tasks/:id`).
- **Transfer ACK**:
  - On verified upload ACK, Keystone marks task `in_progress -> completed` (only if currently `in_progress`).
  - On `upload_failed`, Keystone marks task `in_progress -> failed`.
- **Revert to pending (ready/in_progress→pending)**: used for recovery when Transfer disconnects (to avoid stuck runnable tasks).

### 6.3 Batch states

`pending` | `active` | `completed` | `cancelled` | `recalled`  

**Current transition rules:**

- **Manual cancellation**: `pending/active -> cancelled` via `PATCH /batches/:id` (and cascade-cancel tasks).
- **Manual recall**: `active/completed -> recalled` via `POST /batches/:id/recall` (and labels Episodes).
- **Automatic advancement**:
  - `pending -> active`: when a task under the batch reaches a terminal state (`completed` or `failed`).
  - `active -> completed`: when **all** non-deleted tasks under the batch are terminal (`completed/failed/cancelled`).

---

## 7. Key flows (end-to-end closed-loop)

### 7.1 Callbacks (HTTP: `/api/v1/callbacks/*`)

| Method | Path | Notes |
|------|------|------|
| POST | `/callbacks/start` | **ACK only** (no DB validation; does not update `tasks.status`) |
| POST | `/callbacks/finish` | If the device is online, send `upload_request` to the Transfer hub (does not write Episode directly) |

**Design constraint:** callbacks are device-side event entrypoints, but **Task terminal state and Episode persistence must be idempotent and retryable**; implementation may treat Transfer ACK as the source of truth.

### 7.2 Transfer (WebSocket: `GET /transfer/:device_id`, separate port)

When the device reports `upload_complete`, Keystone runs the Verified ACK flow:

1. **Verify objects exist in S3**: `<factoryID>/<deviceID>/<YYYY-MM-DD>/<task_id>.mcap` and `.json`
2. **DB transaction**:
   - Resolve `tasks.id` by `tasks.task_id` (numeric PK)
   - **Idempotent**: if an Episode already exists for this `task_id`, do not insert again
   - Insert into `episodes` (persist denormalized fields such as `batch_id/order_id/scene_id/...`)
   - `batches.episode_count += 1` (only when a new Episode is inserted)
   - Update `tasks.status` to **`completed`** (and set `completed_at`) **only when current status is `in_progress`**
3. **Send `upload_ack`** to the device

---

## 8. Known gaps and evolution

- **In-recording state**: `callbacks/start` does not persist state today; `ready -> in_progress` validation/persistence is not implemented yet.
- **Failure path**: an end-to-end `failed` terminal state and error attribution are not fully implemented (callbacks/transfer need to be extended).
- **Quota consistency**:
  - Dispatch quota is based on non-deleted task rows, not completed rows.
  - New/bulk batch creation uses `remaining_assignable = target_count - order_task_count`.
  - Batch editing validates the post-edit order task total so repeated edits cannot bypass the target.
  - Any older endpoint or client that still uses a different quota definition should be aligned before it is treated as a supported production dispatch API.
- **No reservation model yet**: Keystone does not reserve order target capacity per workstation ahead of task-row creation. Capacity is consumed when task rows are created and released only when pending task rows are soft-deleted by allowed edit/cancel flows.
- **Controlled order transitions**: `PUT /orders/:id` validates enum values, but auto-advance also exists (see §6.1). These should converge to a single, explicit policy aligned with Primer.

---

## 9. Code pointers (Keystone)

| Area | Path |
|------|------|
| Order HTTP + auto-advance | `internal/api/handlers/order.go` |
| Batch HTTP + batch task adjustment | `internal/api/handlers/batch.go` |
| Task HTTP + callbacks | `internal/api/handlers/task.go` |
| Transfer + Episode writes | `internal/api/handlers/transfer.go` |
| Route mounting | `internal/server/server.go` |
| Table schemas | `internal/storage/database/migrations/000001_initial_schema.up.sql` |

---
