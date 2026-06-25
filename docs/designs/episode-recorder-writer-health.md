<!--
SPDX-FileCopyrightText: 2026 ArcheBase

SPDX-License-Identifier: MulanPSL-2.0
-->

# Episode Recorder Writer Health Design

**Status:** proposed

**Scope:** Keystone transfer upload completion, episode metadata, episode detail API, and Synapse episode detail UI.

## 1. Problem

Axon Recorder now writes a `writer_health` summary into the recording sidecar JSON.
The summary describes whether the recorder write path had queue pressure, writer
stall, queue overflow, or partial write failures during a recording.

Keystone already reads the sidecar JSON during transfer `upload_complete` to
populate episode fields such as duration, file size, and checksum. Keystone
should also persist the sidecar `writer_health` summary on the episode so
Synapse can show recording diagnostics on the episode detail page.

This diagnostic is not a QA result. It is a recorder-side write-path signal that
helps users understand whether the captured data may need extra review.

## 2. Current Behavior

- Axon writes top-level `writer_health` into sidecar JSON when a recording is
  finished and sidecar generation is enabled.
- Keystone `POST /api/v1/callbacks/finish` marks the task as `uploading` and
  sends an upload request to `axon_transfer`.
- Keystone creates the `episodes` row later, when `axon_transfer` reports
  `upload_complete` and Keystone verifies that both MCAP and sidecar objects are
  present in MinIO.
- Keystone reads sidecar JSON at that point to extract:
  - `recording.duration_sec`
  - `recording.file_size_bytes`
  - `recording.checksum_sha256`
- Episode metadata currently stores an `asset_id` snapshot when it can be
  resolved from the workstation's robot.
- Synapse episode detail currently does not render episode metadata and has no
  recording diagnostics panel.

## 3. Decisions

### 3.1 Source of Truth

Keystone should read `writer_health` from the uploaded sidecar JSON, not from
the recorder finish callback.

Rationale:

- It matches the existing flow for duration, file size, and checksum.
- It avoids adding finish callback persistence before an episode row exists.
- It keeps the episode diagnostic tied to the uploaded recording artifact.
- Finish callback handling stays focused on task lifecycle and upload request
  dispatch.

### 3.2 Episode Metadata Shape

Persist the recorder diagnostic under a namespaced metadata path:

```json
{
  "asset_id": "robot-asset-001",
  "recorder": {
    "writer_health": {
      "state": "critical",
      "writer_stall_state": "critical",
      "writer_stall_suspected": true,
      "writer_partial_failures": 2,
      "writer_queue_overflows": 1,
      "error": "writer_partial_failures=2"
    }
  }
}
```

Rules:

- Preserve existing metadata fields such as `asset_id`.
- Preserve existing `metadata.recorder` fields.
- Only overwrite `metadata.recorder.writer_health` when a new sidecar
  `writer_health` object is present.
- Do not store `writer_health` at metadata top level.
- Keep `error: null` when the sidecar explicitly contains no error. The frontend
  should hide an empty or null diagnostic message.

### 3.3 Sidecar Parsing Rules

Keystone should extend its sidecar parser with a top-level optional
`writer_health` object.

The expected Axon sidecar shape is:

```json
{
  "writer_health": {
    "state": "normal",
    "writer_stall_state": "normal",
    "writer_stall_suspected": false,
    "writer_partial_failures": 0,
    "writer_queue_overflows": 0,
    "error": null
  }
}
```

Rules:

- If top-level `writer_health` exists, persist it even when the state is
  `normal` and all counters are zero.
- If top-level `writer_health` is absent, do not write or clear
  `metadata.recorder.writer_health`.
- If sidecar reading or unmarshalling fails, keep the existing best-effort
  behavior and do not block upload completion.
- Backend should preserve the sidecar values rather than rejecting or rewriting
  unknown state strings.

### 3.4 Idempotency

Keystone should support both creation-time persistence and later idempotent
repair.

| Case | Behavior |
| --- | --- |
| New episode, sidecar has `writer_health` | Insert `metadata.recorder.writer_health`. |
| New episode, sidecar lacks `writer_health` | Insert existing metadata only, such as `asset_id`. |
| Episode already exists, sidecar has `writer_health` | Merge and overwrite only `metadata.recorder.writer_health`. |
| Episode already exists, sidecar lacks `writer_health` | Leave metadata unchanged. |
| Sidecar read fails | Leave metadata unchanged and continue existing flow. |

This allows repeated `upload_complete` handling to backfill or correct writer
health without duplicating episodes or destroying unrelated metadata.

### 3.5 Database Model

Do not add a dedicated database column in this change.

Rationale:

- The current requirement is detail-page display, not filtering, sorting, or
  statistics.
- `episodes.metadata` already exists and is appropriate for artifact-derived
  optional diagnostics.
- A future list filter can add a dedicated `writer_health_state` column or JSON
  index when the product needs it.

### 3.6 Episode API

`GET /api/v1/episodes/:id` should return `metadata`.

`GET /api/v1/episodes` should not return `metadata`.

Rationale:

- Only the episode detail page needs this diagnostic.
- List payloads should remain compact.
- Future list filtering should use explicit fields or query parameters rather
  than returning complete metadata for every row.

## 4. Writer Health Field Meaning

| Field | Meaning | UI Use |
| --- | --- | --- |
| `state` | Overall recorder write-path health: `normal`, `warning`, `critical`, or a future value. | Main panel status. |
| `writer_stall_state` | Health of MCAP writer latency/stall detection. | Detail row. |
| `writer_stall_suspected` | Whether Axon suspects writer stalls occurred. | Detail row as yes/no. |
| `writer_partial_failures` | Count of partial write failures observed by the writer path. | High-risk counter. |
| `writer_queue_overflows` | Count of writer queue overflows. | High-risk counter. |
| `error` | Human-readable diagnostic summary, often `writer_partial_failures=N`. | Show only when non-empty. |

Recommended interpretation:

- `writer_partial_failures > 0` is a serious signal that some messages may not
  have been written successfully.
- `writer_queue_overflows > 0` is a serious signal that the writer queue could
  not keep up.
- `writer_stall_suspected = true` indicates write-path latency or blocking risk.
- `state` is the user-facing summary.
- `error` is supplemental text, not the primary status source.

## 5. Synapse Episode Detail UI

Synapse should add a standalone recording diagnostics panel on the admin episode
detail page.

Placement:

1. Identity panel
2. Quality check panel
3. Recording diagnostics panel
4. Cloud sync panel
5. File path panel

Rules:

- Read `episode.metadata?.recorder?.writer_health`.
- If `writer_health` is missing, do not render the panel.
- If `writer_health` exists with `state = normal`, render the panel with a light
  normal treatment.
- If state is `warning`, render a warning treatment.
- If state is `critical`, render an error treatment.
- If state is present but unknown, render a neutral "unknown" treatment.
- Do not render the full metadata JSON.
- Do not change QA status or trigger QA failure based on writer health in this
  change.

Suggested copy:

| State | Title | Description |
| --- | --- | --- |
| `normal` | `录制写入正常` | `未发现写入队列溢出或部分写入失败。` |
| `warning` | `录制写入告警` | `录制写入链路存在压力信号，建议关注。` |
| `critical` | `录制写入异常` | `这段录制的写入链路存在风险，建议复核 MCAP 完整性。` |
| unknown | `录制写入状态未知` | `录制诊断状态无法识别。` |

Suggested detail rows:

| Label | Source |
| --- | --- |
| `总体状态` | `writer_health.state` |
| `写入阻塞` | `writer_health.writer_stall_state` |
| `疑似阻塞` | `writer_health.writer_stall_suspected` |
| `部分失败` | `writer_health.writer_partial_failures` |
| `队列溢出` | `writer_health.writer_queue_overflows` |
| `诊断说明` | `writer_health.error`, only when non-empty |

## 6. Non-Goals

- Do not read or persist finish callback `writer_health`.
- Do not add a new database column.
- Do not add list-page display or filters.
- Do not render full episode metadata JSON in Synapse.
- Do not change QA status.
- Do not auto-fail QA based on writer health.
- Do not block upload completion when sidecar diagnostics are missing or
  malformed.

## 7. Backend Implementation Notes

Recommended helper responsibilities:

- Parse sidecar `writer_health` as a raw JSON object or a typed struct that
  preserves the known fields and `error`.
- Build episode metadata from existing metadata plus optional
  `recorder.writer_health`.
- Merge metadata idempotently:
  - parse existing metadata into `map[string]any`;
  - ensure `recorder` is a `map[string]any`;
  - set `recorder["writer_health"]`;
  - marshal back to JSON.
- On duplicate upload completion where the episode already exists, update the
  existing episode metadata only if sidecar `writer_health` is present.

Recommended API changes:

- Add `metadata` to the `Episode` response model.
- Add `metadata` to the detail query only.
- Keep list queries unchanged.

## 8. Test Plan

Backend tests:

- Sidecar with `writer_health` creates an episode whose
  `metadata.recorder.writer_health` matches the sidecar.
- Sidecar without `writer_health` creates an episode without
  `metadata.recorder.writer_health`.
- Existing episode plus sidecar `writer_health` backfills or overwrites only
  `metadata.recorder.writer_health`.
- Existing metadata fields, including `asset_id` and other `recorder` children,
  are preserved during backfill.
- `GET /api/v1/episodes/:id` returns metadata.
- `GET /api/v1/episodes` does not return metadata.

Frontend verification:

- `npm run build` passes.
- Episode detail does not show the recording diagnostics panel for old episodes
  without writer health.
- Episode detail shows normal, warning, critical, and unknown states correctly.
- Empty or null `error` is hidden.

