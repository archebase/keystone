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
   collector enters the completed quantity and taps "complete tasks" in Synapse.
8. Keystone completes up to that many pending tasks in the batch and advances
   batch and order status when applicable.

## 6. Backend Design

### 6.1 Complete Tasks API

Add a batch-scoped endpoint for the data collector page:

| Method | Path | Auth | Purpose |
|--------|------|------|---------|
| `POST` | `/api/v1/batches/:id/complete-tasks` | data collector JWT | Complete pending tasks in a batch |

Recommended handler location:

- `keystone-worktree2/internal/api/handlers/batch.go`
- Register from `BatchHandler.RegisterRoutes`.

Request body:

```json
{
  "quantity": 50
}
```

Response body:

```json
{
  "batch_id": "batch_20260511_103000_123_01_abcd1234",
  "requested_count": 50,
  "completed_count": 12,
  "tasks": [
    {
      "id": "123",
      "task_id": "task_20260511_103010_000_00_abcd1234",
      "status": "completed",
      "completed_at": "2026-05-11T10:31:00Z"
    }
  ],
  "batch": {
    "id": "12",
    "status": "completed",
    "completed_count": 12,
    "task_count": 12
  }
}
```

Behavior:

- Resolve current collector from JWT claims.
- Resolve collector workstation from `workstations.data_collector_id`.
- Lock target batch with `FOR UPDATE`.
- Reject if the batch does not belong to the collector's workstation.
- Reject if batch status is not `pending` or `active`.
- Validate `quantity >= 1`.
- Do not use a fixed numeric upper bound. The effective maximum is the number
  of currently pending tasks in the batch.
- Lock the earliest pending tasks in the batch:
  - `status = 'pending'`
  - `deleted_at IS NULL`
  - stable order by `assigned_at ASC, id ASC`
- If fewer pending tasks exist than requested, complete all remaining pending
  tasks and return both `requested_count` and actual `completed_count`.
- If no pending task exists, return `409` with current batch progress.
- Update selected tasks directly to `completed`.
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
| Invalid quantity | `400` | `{"error": "quantity must be >= 1"}` |
| No pending task | `409` | include `completed_count` and `task_count` |

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
- "complete tasks" action with quantity input;
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
  Complete tasks button
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

### 7.4 Complete Tasks Interaction

Recommended UI wording:

- Button: `完成任务`
- Confirm dialog/sheet title: `确认完成任务`
- Confirm copy: `确认外部设备已完成录制和上传后，再标记 Keystone 任务完成。`

Mobile behavior:

- Use a bottom sheet or compact modal, not a large desktop modal.
- Quantity input must not allow submission above the batch's pending task count.
- Disable the confirm button while the request is in flight.
- After success:
  - update local progress immediately;
  - refresh batch details;
  - keep the same batch card in view;
  - show `已完成 N 条任务`.
- If no pending tasks remain:
  - show `该批次暂无待完成任务`;
  - refresh status, likely `completed`.

## 8. Admin UI Design

### 8.1 Admin Batch List and Detail

Add copy affordance for `batch_id`:

- batch list row/card;
- batch detail header;
- any modal that shows newly created batch result.

The copy behavior should reuse the same helper as the operator mobile page.

## 9. Implementation Plan

### Phase 1: Backend APIs

- Add `POST /api/v1/batches/:id/complete-tasks`.
- Add focused Go tests for:
  - completing pending tasks by requested quantity;
  - rejecting another collector's batch;
  - rejecting terminal batches;
  - rejecting invalid quantity;
  - no pending task conflict.

### Phase 2: Synapse Operator Mobile Page

- Replace `RobotControl.vue` with the external device task execution workflow.
- Add/extend API client functions:
  - `completeTasks(batchNumericId, quantity)`.
- Remove recorder API calls from this page.
- Add mobile copy helper and feedback states.
- Verify mobile viewport behavior manually and with browser screenshots if
  available.

### Phase 3: Admin Copy Polish

- Add copy button for `batch_id` on admin batch list/detail.

## 10. Acceptance Criteria

Backend:

- A data collector can complete a pending task from a batch assigned to their
  workstation without Axon recorder or transfer online.
- Completing tasks advances batch status and order status consistently with
  existing Keystone rules.
- A data collector cannot complete tasks from another workstation's batch.

Operator mobile page:

- On a phone viewport, the collector can find a batch, copy `batch_id`, and
  complete tasks without horizontal scrolling.
- `batch_id` copy feedback is visible and non-blocking.
- Main buttons are easy to tap on `360px` wide screens.
- Page still works when the external device is offline from Keystone's point of
  view, because the external workflow is independent.
- No recorder/transfer state is shown.

## 11. Open Questions

- Should the admin batch creation UI also be simplified for this external-device
  workflow, or should this first version only change the data collector and
  task execution surface?
