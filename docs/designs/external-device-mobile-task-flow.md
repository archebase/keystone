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
- Robot type capabilities use `requires_axon` to decide which operator workflow
  to show. Missing `requires_axon` is treated as `true` for existing robot
  types.
- The original Axon data collector control page remains the default for robot
  types that require Axon.
- The data collector page must provide one-tap copy for `batch_id`; the operator
  will paste this value into the external device program.
- The data collector page is operated mainly on mobile phones and must be
  designed and tested as a mobile-first workflow.

## 3. Goals

- Reuse existing Keystone `orders`, `batches`, `tasks`, `sops`, `workstations`,
  `robots`, and `data_collectors` data models.
- Let admins continue creating and assigning batches/tasks in Keystone.
- Let data collectors see assigned batches on a phone, copy `batch_id`, and mark
  tasks as completed after the external device workflow is done when the current
  robot type has `capabilities.requires_axon === false`.
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
2. Synapse opens the existing operator control route.
3. Page loads the collector's assigned workstation.
4. If the workstation robot type has `capabilities.requires_axon !== false`,
   Synapse shows the original Axon recording control workflow.
5. If the workstation robot type has `capabilities.requires_axon === false`,
   Synapse shows the external-device task execution workflow.
6. Page lists active work:
   - batches in `pending` or `active` status;
   - task progress for each batch;
   - `batch_id` as the primary copyable value.
7. Data collector taps "copy batch ID".
8. Data collector pastes the copied `batch_id` into the external device program.
9. After the external device finishes its own record/upload workflow, data
   collector selects a task group, enters the completed quantity, and taps
   "complete tasks" in Synapse.
10. Keystone completes existing pending tasks in that group first, creates extra
    completed tasks in the same group when the requested quantity exceeds the
    group's pending count, and advances batch/order status when applicable.

## 6. Backend Design

### 6.1 Robot Type Capability

Use `robot_types.capabilities.requires_axon` to decide whether a workstation's
operator workflow depends on Axon.

Recommended capability shape:

```json
{
  "requires_axon": false
}
```

Semantics:

- `requires_axon: true`: the robot type depends on Axon recorder/transfer; use
  the original Axon recording control workflow.
- `requires_axon: false`: the robot type does not depend on Axon; use the
  external-device task execution workflow.
- missing `requires_axon`: treat as `true` to preserve current behavior for
  existing robot types.

The capability is a routing/UX decision. It does not change how admins create
orders, batches, or task groups.

Persistence and refresh behavior:

- `requires_axon` is persisted in the database as part of
  `robot_types.capabilities`.
- Keystone and Synapse should not treat it as a startup-only static switch.
- Backend responses that include the workstation robot type should read the
  current `capabilities` value from the database.
- Synapse should re-evaluate the workflow after loading or refreshing the current
  collector workstation.
- Updating `requires_axon` in the admin UI should take effect for newly opened
  operator pages immediately, and for already opened pages after the next
  workstation refresh.
- No Keystone or Synapse restart should be required.

### 6.2 Complete Tasks API

Add a batch-scoped endpoint for the data collector page:

| Method | Path | Auth | Purpose |
|--------|------|------|---------|
| `POST` | `/api/v1/batches/:id/complete-tasks` | data collector JWT | Complete tasks in a selected batch task group |

Recommended handler location:

- `keystone-worktree2/internal/api/handlers/batch.go`
- Register from `BatchHandler.RegisterRoutes`.

Request body:

```json
{
  "quantity": 50,
  "sop_id": 8,
  "subscene_id": 3
}
```

Response body:

```json
{
  "batch_id": "batch_20260511_103000_123_01_abcd1234",
  "requested_count": 50,
  "completed_count": 50,
  "created_count": 38,
  "group": {
    "sop_id": 8,
    "subscene_id": 3,
    "scene_name": "Warehouse",
    "subscene_name": "Shelf A"
  },
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
    "completed_count": 50,
    "task_count": 50
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
- Validate `sop_id` and `subscene_id` as the selected task group.
- Reject if the selected task group does not exist in the target batch.
- Do not use a fixed numeric upper bound and do not cap by current pending
  count.
- Lock the earliest pending tasks in the selected task group:
  - `status = 'pending'`
  - `deleted_at IS NULL`
  - matching `batch_id`, `sop_id`, and `subscene_id`
  - stable order by `assigned_at ASC, id ASC`
- If fewer pending tasks exist than requested, complete all selected pending
  tasks and create the remaining quantity as new `completed` tasks in the same
  group.
- If the selected group has no pending tasks but exists in the batch, create all
  requested tasks as new `completed` tasks in that group.
- Update selected tasks directly to `completed`.
- Create overflow tasks from a representative non-deleted task row in the
  selected group:
  - inherit group fields such as `sop_id`, `scene_name`, `subscene_id`, and
    `subscene_name`;
  - generate new `task_id` values using the existing task ID generation pattern;
  - set `status = 'completed'`;
  - set `assigned_at`, `started_at`, `completed_at`, and `updated_at` to `now`;
  - preserve any required foreign keys and task metadata needed for downstream
    list/detail views.
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
| Invalid task group | `400` | `{"error": "invalid task group"}` |
| Task group not found in batch | `404` | `{"error": "task group not found in batch"}` |

## 7. Synapse Design

### 7.1 Operator Control Workflow Selection

Keep the existing operator control route for navigation compatibility:

- `/operator/control`
- existing `OperatorControl` route name

Workflow selection:

- Load the current collector's workstation and bound robot type.
- If `robot_type.capabilities.requires_axon !== false`, render the original Axon
  recording control workflow.
- If `robot_type.capabilities.requires_axon === false`, render the
  external-device task execution workflow.

For the external-device workflow, the existing component can be rewritten or
split into focused components:

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
- "complete tasks" action with task group selection and quantity input;
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
- If the batch has one task group, select it by default.
- If the batch has multiple task groups, require the data collector to choose one
  group before submitting.
- Quantity input only needs to enforce `quantity >= 1`; it must allow values
  above the selected group's current pending count.
- Do not show a special warning when the quantity exceeds pending count. The
  overflow is treated as naturally completed work in the selected group.
- Disable the confirm button while the request is in flight.
- After success:
  - update local progress immediately;
  - refresh batch details;
  - keep the same batch card in view;
  - show `已完成 N 条任务`.
- If the refreshed batch has completed, show the normal completed/empty state
  produced by the batch list refresh.

## 8. Admin UI Design

### 8.1 Admin Batch List and Detail

Add copy affordance for `batch_id`:

- batch list row/card;
- batch detail header;
- any modal that shows newly created batch result.

The copy behavior should reuse the same helper as the operator mobile page.

## 9. Implementation Plan

### Phase 1: Backend APIs

- Add `requires_axon` support in robot type capabilities consumers. Missing
  value must behave as `true`.
- Add `POST /api/v1/batches/:id/complete-tasks`.
- Add focused Go tests for:
  - completing tasks in the selected `sop_id` + `subscene_id` group;
  - creating overflow completed tasks when requested quantity exceeds group
    pending count;
  - allowing completion when the selected group exists but has no pending tasks;
  - rejecting missing or invalid task group identifiers;
  - rejecting a group that does not exist in the batch;
  - rejecting another collector's batch;
  - rejecting terminal batches;
  - rejecting invalid quantity.

### Phase 2: Synapse Operator Mobile Page

- Select between the original Axon workflow and the external-device workflow by
  `robot_type.capabilities.requires_axon`.
- Add/extend API client functions:
  - `completeTasks(batchNumericId, { quantity, sop_id, subscene_id })`.
- For the external-device workflow, remove recorder API calls from the active
  page path.
- Add task group selection to the complete-task bottom sheet.
- Add mobile copy helper and feedback states.
- Verify mobile viewport behavior manually and with browser screenshots if
  available.

### Phase 3: Admin Copy Polish

- Add copy button for `batch_id` on admin batch list/detail.

## 10. Acceptance Criteria

Backend:

- A data collector can complete tasks in a selected task group from a batch
  assigned to their workstation without Axon recorder or transfer online when
  the workstation robot type has `requires_axon === false`.
- Completing tasks with a quantity greater than the selected group's pending
  count creates extra completed tasks in that same group.
- Completing tasks advances batch status and order status consistently with
  existing Keystone rules.
- A data collector cannot complete tasks from another workstation's batch.
- Existing robot types without `requires_axon` continue to use the Axon workflow.

Operator mobile page:

- On a phone viewport, the collector can find a batch, copy `batch_id`, and
  complete tasks for a selected group without horizontal scrolling.
- Multi-group batches require group selection before task completion.
- Completion quantity can exceed pending count without blocking submission.
- `batch_id` copy feedback is visible and non-blocking.
- Main buttons are easy to tap on `360px` wide screens.
- Page still works when the external device is offline from Keystone's point of
  view, because the external workflow is independent.
- No recorder/transfer state is shown.

## 11. Open Questions

- Should the admin batch creation UI also be simplified for this external-device
  workflow, or should this first version only change the data collector and
  task execution surface?
