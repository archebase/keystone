<!--
SPDX-FileCopyrightText: 2026 ArcheBase

SPDX-License-Identifier: MulanPSL-2.0
-->

# External Device Mobile Task Flow Implementation Design

## 1. Background

This worktree has a customization requirement for a device that has its own
recording and upload program. The device does not depend on Axon recorder or
Axon transfer, but it still needs Keystone to manage production tasks and needs
Synapse to provide a mobile-friendly data collector operation page.

The customization is not expected to be merged into mainline, so the design
prioritizes small, direct changes over a generalized adapter framework.

## 2. Product Decisions

Confirmed decisions:

- Keystone does not store external upload IDs, file URLs, or upload artifacts.
- Batch lookup by `batch_id` must be available without login.
- The original Axon data collector control page can be replaced in worktree2.
- The data collector page must provide one-tap copy for `batch_id`; the operator
  will paste this value into the external device program.
- The data collector page is operated mainly on mobile phones and must be
  designed and tested as a mobile-first workflow.

## 3. Goals

- Reuse existing Keystone `orders`, `batches`, `tasks`, `sops`, `workstations`,
  `robots`, and `data_collectors` data models.
- Let admins continue creating and assigning batches/tasks in Keystone.
- Let data collectors see assigned batches on a phone, copy `batch_id`, and mark
  tasks as completed after the external device workflow is done.
- Let operators query batch details publicly by `batch_id`.
- Keep Axon recorder/transfer APIs untouched unless an existing route must be
  bypassed by the new mobile page.

## 4. Non-Goals

- No external upload tracking inside Keystone.
- No new WebSocket protocol for the external device.
- No adapter abstraction for multiple recording systems.
- No episode creation for externally uploaded files in the first version.
- No Axon recorder state machine on the customized data collector page.
- No dependency on recorder/transfer online state for task completion.

## 5. Target User Flows

### 5.1 Data Collector Mobile Flow

1. Data collector logs in on a phone.
2. Synapse opens the existing operator control route, now customized as a mobile
   task execution page.
3. Page loads the collector's assigned workstation.
4. Page lists active work:
   - batches in `pending` or `active` status;
   - task progress for each batch;
   - `batch_id` as the primary copyable value.
5. Data collector taps "copy batch ID".
6. Data collector pastes the copied `batch_id` into the external device program.
7. After the external device finishes its own record/upload workflow, data
   collector taps "complete one task" in Synapse.
8. Keystone completes the next pending task in that batch and advances batch and
   order status when applicable.

### 5.2 Public Operator Lookup Flow

1. Operator receives a `batch_id`.
2. Operator opens a public lookup page or direct URL.
3. Synapse calls a public Keystone API without JWT.
4. Page shows batch status, SOP/subscene summary, collector/workstation/device
   context, and task completion details.

## 6. Backend Design

### 6.1 Complete Next Task API

Add a batch-scoped endpoint for the data collector page:

| Method | Path | Auth | Purpose |
|--------|------|------|---------|
| `POST` | `/api/v1/batches/:id/complete-next-task` | data collector JWT | Complete the next pending task in a batch |

Recommended handler location:

- `keystone-worktree2/internal/api/handlers/batch.go`
- Register from `BatchHandler.RegisterRoutes`.

Request body:

```json
{}
```

Response body:

```json
{
  "batch_id": "batch_20260511_103000_123_01_abcd1234",
  "task": {
    "id": "123",
    "task_id": "task_20260511_103010_000_00_abcd1234",
    "status": "completed",
    "completed_at": "2026-05-11T10:31:00Z"
  },
  "batch": {
    "id": "12",
    "status": "active",
    "completed_count": 3,
    "task_count": 10
  }
}
```

Behavior:

- Resolve current collector from JWT claims.
- Resolve collector workstation from `workstations.data_collector_id`.
- Lock target batch with `FOR UPDATE`.
- Reject if the batch does not belong to the collector's workstation.
- Reject if batch status is not `pending` or `active`.
- Lock the earliest pending task in the batch:
  - `status = 'pending'`
  - `deleted_at IS NULL`
  - stable order by `assigned_at ASC, id ASC`
- If no pending task exists, return `409` with current batch progress.
- Update the task directly to `completed`.
- Set timestamps:
  - `started_at = COALESCE(started_at, now)`
  - `completed_at = now`
  - `updated_at = now`
- If the batch is `pending`, advance it to `active` and set `started_at`.
- Reuse existing batch/order advancement logic after commit:
  - `tryAdvanceBatchStatus`
  - `tryAdvanceOrderStatus`
- Do not call recorder RPC.
- Do not call transfer upload.
- Do not require device online state.

Status and error rules:

| Case | Status | Response |
|------|--------|----------|
| Missing/invalid JWT | `401` | `{"error": "authentication required"}` |
| Non-collector role | `403` | `{"error": "data collector role required"}` |
| Collector has no workstation | `404` | `{"error": "workstation not assigned"}` |
| Batch not found | `404` | `{"error": "batch not found"}` |
| Batch belongs to another workstation | `403` | `{"error": "batch is not assigned to current workstation"}` |
| Batch cancelled/recalled/completed | `409` | include `current_status` |
| No pending task | `409` | include `completed_count` and `task_count` |

### 6.2 Public Batch Lookup API

Add a public read-only endpoint:

| Method | Path | Auth | Purpose |
|--------|------|------|---------|
| `GET` | `/api/v1/public/batches/:batch_id` | none | Lookup batch details by public batch code |

Recommended implementation:

- Add a small public handler, or add a public method to `BatchHandler`.
- Register under `/api/v1/public` before JWT-only route groups.
- Query by `batches.batch_id`, not numeric `batches.id`.

Response body:

```json
{
  "batch_id": "batch_20260511_103000_123_01_abcd1234",
  "status": "active",
  "order": {
    "name": "Order A",
    "scene_name": "Warehouse"
  },
  "workstation": {
    "name": "ws-abc12345"
  },
  "device": {
    "device_id": "external-device-001",
    "asset_id": "robot-a"
  },
  "collector": {
    "operator_id": "op001",
    "name": "Collector A"
  },
  "progress": {
    "task_count": 10,
    "completed_count": 3,
    "failed_count": 0,
    "cancelled_count": 0
  },
  "groups": [
    {
      "sop_id": "8",
      "sop_slug": "pick-place",
      "sop_version": 1,
      "scene_name": "Warehouse",
      "subscene_id": "3",
      "subscene_name": "Shelf A",
      "task_count": 10,
      "completed_count": 3,
      "failed_count": 0,
      "cancelled_count": 0
    }
  ],
  "tasks": [
    {
      "task_id": "task_20260511_103010_000_00_abcd1234",
      "status": "completed",
      "sop_slug": "pick-place",
      "subscene_name": "Shelf A",
      "assigned_at": "2026-05-11T10:30:10Z",
      "completed_at": "2026-05-11T10:31:00Z"
    }
  ],
  "created_at": "2026-05-11T10:30:00Z",
  "started_at": "2026-05-11T10:31:00Z",
  "ended_at": null
}
```

Public response constraints:

- Do not expose internal numeric IDs unless required for display.
- Do not expose raw `metadata`.
- Do not expose auth claims or password-related fields.
- Do not expose upload paths because this customized workflow does not store
  external upload artifacts.

### 6.3 Batch ID Security

`batch_id` becomes a public lookup token. The current format is readable and
unique. For public unauthenticated lookup, future new `batch_id` generation
should consider a longer random suffix.

Recommended format:

```text
batch_YYYYMMDD_HHMMSS_mmm_<seq>_<rand16>
```

This keeps the value readable for operators while making guessing impractical.
Changing generation only affects newly created batches. Existing batches remain
queryable.

## 7. Synapse Design

### 7.1 Replace Operator Control Page

Replace the worktree2 operator control page behavior:

- Existing route can stay the same for navigation compatibility:
  - `/operator/control`
  - existing `OperatorControl` route name
- Existing component can be rewritten or replaced:
  - `synapse-worktree2/src/views/RobotControl.vue`

Remove from the customized page:

- recorder online/offline requirement;
- transfer online/offline requirement;
- recorder state machine;
- configure/begin/pause/resume/finish/cancel/clear recorder commands;
- topics/stats panels;
- recorder polling.

Add to the customized page:

- current collector summary;
- workstation summary;
- assigned active batch list;
- copyable `batch_id`;
- progress for each batch;
- task group summary by SOP/subscene;
- "complete one task" action;
- refresh action;
- empty, loading, error, and conflict states.

### 7.2 Mobile-First Layout

Primary target: phone browser.

Supported viewport checks:

- `360 x 640`
- `375 x 812`
- `390 x 844`
- `430 x 932`

Layout requirements:

- Use a single-column layout by default.
- Avoid dense tables in the data collector page.
- Use full-width batch cards.
- Keep the current batch code visible near the top of each card.
- Use sticky bottom actions only when they do not cover card content.
- Use `env(safe-area-inset-bottom)` for bottom action padding.
- Keep primary touch targets at least `44px` high.
- Avoid hover-only affordances.
- Avoid tiny icon-only controls unless there is a visible label.
- Do not rely on desktop sidebars for primary navigation.
- Keep text wrapping predictable for long `batch_id` values.

Recommended mobile card structure:

```text
Batch card
  batch_id + Copy button
  status + progress
  order / scene
  SOP + subscene summary
  Complete one task button
```

### 7.3 Copy Interaction

The data collector page must make copying `batch_id` obvious.

Implementation requirements:

- Copy button appears next to every visible `batch_id`.
- Use `navigator.clipboard.writeText` when available.
- Provide a fallback using a temporary text selection for older mobile browsers.
- Show immediate feedback:
  - copied state on the button for about 1.5 seconds;
  - short toast or inline message;
  - no blocking `alert`.
- The copied value must be the raw `batch_id`, without spaces or labels.

### 7.4 Complete Task Interaction

Recommended UI wording:

- Button: `完成一条任务`
- Confirm dialog/sheet title: `确认完成任务`
- Confirm copy: `确认外部设备已完成录制和上传后，再标记 Keystone 任务完成。`

Mobile behavior:

- Use a bottom sheet or compact modal, not a large desktop modal.
- Disable the confirm button while the request is in flight.
- After success:
  - update local progress immediately;
  - refresh batch details;
  - keep the same batch card in view;
  - show `已完成 1 条任务`.
- If no pending tasks remain:
  - show `该批次暂无待完成任务`;
  - refresh status, likely `completed`.

## 8. Admin/Public UI Design

### 8.1 Admin Batch List and Detail

Add copy affordance for `batch_id`:

- batch list row/card;
- batch detail header;
- any modal that shows newly created batch result.

The copy behavior should reuse the same helper as the operator mobile page.

### 8.2 Public Batch Lookup Page

Add a route such as:

```text
/public/batches/:batch_id
```

Page requirements:

- No login required.
- Mobile-friendly by default.
- Show a clear not-found state for invalid `batch_id`.
- Show batch status and progress first.
- Show SOP/subscene summary before task details.
- Task details can be collapsed on mobile when task count is large.
- Include copy button for the batch code.

## 9. Implementation Plan

### Phase 1: Backend APIs

- Add `POST /api/v1/batches/:id/complete-next-task`.
- Add `GET /api/v1/public/batches/:batch_id`.
- Add focused Go tests for:
  - completing the next pending task;
  - rejecting another collector's batch;
  - rejecting terminal batches;
  - no pending task conflict;
  - public lookup by `batch_id`;
  - public lookup does not require JWT.

### Phase 2: Synapse Operator Mobile Page

- Replace `RobotControl.vue` with the external device task execution workflow.
- Add/extend API client functions:
  - `completeNextTask(batchNumericId)`;
  - `getPublicBatchByCode(batchId)`.
- Remove recorder API calls from this page.
- Add mobile copy helper and feedback states.
- Verify mobile viewport behavior manually and with browser screenshots if
  available.

### Phase 3: Public Lookup UI

- Add public route.
- Add public page component.
- Render batch summary, grouped progress, and task details.
- Ensure no authenticated layout is required.

### Phase 4: Admin Copy Polish

- Add copy button for `batch_id` on admin batch list/detail.
- Optionally show public lookup URL copy beside raw `batch_id`.

## 10. Acceptance Criteria

Backend:

- A data collector can complete a pending task from a batch assigned to their
  workstation without Axon recorder or transfer online.
- Completing tasks advances batch status and order status consistently with
  existing Keystone rules.
- A data collector cannot complete tasks from another workstation's batch.
- Public batch lookup works without JWT.
- Public batch lookup does not return upload URLs or external upload IDs.

Operator mobile page:

- On a phone viewport, the collector can find a batch, copy `batch_id`, and
  complete one task without horizontal scrolling.
- `batch_id` copy feedback is visible and non-blocking.
- Main buttons are easy to tap on `360px` wide screens.
- Page still works when the external device is offline from Keystone's point of
  view, because the external workflow is independent.
- No recorder/transfer state is shown.

Public page:

- A user with only `batch_id` can view batch details without login.
- The page is readable on mobile.
- Invalid `batch_id` shows a clear not-found state.

## 11. Open Questions

- Should newly created `batch_id` values use a longer random suffix immediately
  in this customization branch?
- Should the admin batch creation UI also be simplified for this external-device
  workflow, or should this first version only change the data collector and
  public lookup surfaces?
