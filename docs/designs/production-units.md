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
- This document does not pin down “order completion policy” (e.g., backfilling failures, completion thresholds). It only constrains data and state validity.

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
  - `target_count`: the maximum number of Tasks expected to be generated for the order (currently used to cap `POST /tasks`).
  - `completed_count`: a derived statistic (based on the number of completed Tasks).
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
- **State transitions**: should converge to controlled transitions (current implementation allows any valid enum; the transition graph will be enforced later).

---

### 5.2 Batches (`/api/v1/batches`)

| Method | Path | Notes |
|------|------|------|
| GET | `/batches` | List (filters: `order_id/workstation_id/status/limit/offset`) |
| GET | `/batches/:id` | Detail |
| PATCH | `/batches/:id` | Status only; allows `pending -> active/cancelled`, `active -> completed/cancelled` |
| DELETE | `/batches/:id` | Soft delete (only allowed when `status=cancelled`) |

**Design constraints:**

- There is currently no `POST /batches`. Batches are created/reused implicitly during Task generation (see 5.3).

---

### 5.3 Tasks (`/api/v1/tasks`)

#### 5.3.1 Create (`POST /tasks`)

`POST /tasks` is the entry point for creating Tasks per **(order + workstation)**:

- **Request fields**: `order_id`, `sop_id`, `subscene_id`, `workstation_id`, optional `quantity` (default 1, range 1..1000)
- **Quantity constraint**: total Tasks under the same Order must not exceed `orders.target_count`.
- **Batch association (implicit)**:
  - Prefer reusing a batch under the same `(order_id, workstation_id)` with status `pending/active`;
  - Otherwise create a new `pending` batch.

#### 5.3.2 Query and config

| Method | Path | Notes |
|------|------|------|
| GET | `/tasks` | List (filters: `workstation_id/status/limit/offset`) |
| GET | `/tasks/:id` | Detail (includes `episode` if linked) |
| PUT | `/tasks/:id` | Status update (restricted transitions) |
| GET | `/tasks/:id/config` | Generate recorder config (requires workstation robot + collector bindings) |

---

## 6. State machines (design constraints + current implementation)

### 6.1 Task states

- **State set**: `pending` | `ready` | `in_progress` | `completed` | `failed` | `cancelled`
- **Prepare (pending→ready)**: triggered by UI/scheduler (currently via `PUT /tasks/:id`).
- **Complete (→completed)**: set after Episode persistence via Transfer Verified ACK (current implementation).

### 6.2 Batch states

`pending` | `active` | `completed` | `cancelled` | `recalled`  
Currently `PATCH /batches/:id` only supports limited transitions (see 5.2) to control the batch lifecycle.

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
   - Update `tasks.status` to **`completed`** (and set `completed_at`)
3. **Send `upload_ack`** to the device

---

## 8. Known gaps and evolution

- **In-recording state**: `callbacks/start` does not persist state today; `ready -> in_progress` validation/persistence is not implemented yet.
- **Failure path**: an end-to-end `failed` terminal state and error attribution are not fully implemented (callbacks/transfer need to be extended).
- **Controlled order transitions**: Order status updates currently only validate enum values; it should converge to controlled transitions aligned with Primer and linked to Task statistics.

---

## 9. Code pointers (Keystone)

| Area | Path |
|------|------|
| Task HTTP + callbacks | `internal/api/handlers/task.go` |
| Transfer + Episode writes | `internal/api/handlers/transfer.go` |
| Route mounting | `internal/server/server.go` |
| Table schemas | `internal/storage/database/migrations/000001_initial_schema.up.sql` |

---
