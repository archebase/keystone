<!--
SPDX-FileCopyrightText: 2026 ArcheBase

SPDX-License-Identifier: MulanPSL-2.0
-->

# Data Production Statistics Design

**Scope:** Synapse Admin data production statistics page and Keystone statistics API

## 1. Purpose

This document defines the product scope, metric definitions, API contract, and implementation plan for a new Synapse Admin statistics page that shows data production status over a selected time range.

The page is intended to answer:

- How much data was produced in the selected period?
- Was the production process stable and successful?
- How long did production take?
- How large was the produced data?
- Which device ID, device type, collector, or source caused volume changes or failures?

The core statistical dimensions are:

- **Count**: production volume and success/failure volume.
- **Duration**: production efficiency and latency.
- **Size**: produced data scale.

## 2. Background and Goals

### 2.1 Background

Keystone already manages production units such as Order, Batch, Task, and Episode. Synapse Admin needs a statistics page for administrators, operators, and maintainers to observe data production over time and quickly locate abnormal production behavior.

The current requirement is not a full BI system. The first version should provide a clear operational dashboard with reliable metric definitions and enough drill-down capability for investigation.

### 2.2 Goals

- Provide time-range based data production statistics in Synapse Admin.
- Define stable metric semantics for count, duration, and size.
- Support trend analysis by time granularity.
- Support breakdown by robot device ID, collector operator ID, and robot type.
- Support detail-level investigation and export.
- Keep API and data model extensible for future alerting and reporting.

### 2.3 Non-goals

- This page does not replace low-level logs or tracing systems.
- This page does not define a full custom report builder.
- This page does not expose raw data content.
- This document does not define Synapse visual styling details beyond page structure and expected states.

## 3. Users and Main Use Cases

### 3.1 Users

- **Admin / operator**
  - Checks daily, weekly, or monthly data production volume.
  - Exports statistics for operational reporting.
  - Confirms whether data production meets expectations.

- **Developer / maintainer**
  - Investigates production drops, high failure rates, or high duration.
  - Locates abnormal device ID, device type, collector, or source.
  - Jumps from statistics to logs or task details when available.

- **Project owner**
  - Reviews production trend and scale.
  - Compares production contribution across projects or sources.

### 3.2 Main Use Cases

- View data production summary for the last 7 days.
- Compare count, duration, and size trends over a selected period.
- Find top devices, collectors, or device types by produced count or failed count.
- Find devices or device types with high average or P95 duration.
- Inspect details for a specific time bucket and dimension.
- Export the current filtered result.

## 4. Admin Page Design

### 4.1 Menu and Route

Recommended Synapse Admin route:

```text
/admin/statistics/data-production
```

Recommended menu hierarchy:

```text
Statistics
└── Data Production
```

If Synapse Admin does not have a Statistics menu yet, this page can temporarily be placed under Data Management or System Monitoring.

### 4.2 Page Layout

```text
Data Production Statistics

[Time Range] [Granularity] [Status] [Robot Device ID] [Robot Type] [Collector Operator ID] [SOP] [Query] [Reset] [Export]

[Total Count] [Success Count] [Failed Count] [Success Rate]
[Average Duration] [P95 Duration] [Total Size] [Average Size]

[Count Trend] [Duration Trend] [Size Trend]

[Breakdown: Robot Device ID / Collector Operator ID / Robot Type]

[Detail Table]
```

### 4.3 Filters

Required filters for MVP:

| Filter | Description | Default |
|---|---|---|
| Time range | Statistics period | Last 7 days |
| Granularity | Time bucket: hour, day, week, month | Day |
| Robot device ID | Robot device identifier | All |
| Robot type | Robot type name, backed by `robot_type_id` | All |
| Collector operator ID | Data collector operator identifier | All |
| SOP | Standard operating procedure, backed by `sop_id` | All |
| Status | Success, failed, processing, discarded | All |

Recommended behavior:

- If the selected range is today, use hour granularity by default.
- If the selected range is longer than 90 days, recommend week or month granularity.
- Time range should be converted to an explicit start and end timestamp before calling the API.
- Export must use the same filters as the current page.

### 4.4 Metric Cards

Recommended first-version cards:

| Group | Metric | Notes |
|---|---|---|
| Count | Total count | Total production attempts or completed production records, according to the metric definition in section 5 |
| Count | Success count | Successfully produced and usable data |
| Count | Failed count | Failed or discarded production records |
| Count | Success rate | `success_count / total_count` |
| Duration | Average duration | Completed records only |
| Duration | P95 duration | Completed records only, recommended for stability analysis |
| Size | Total size | In bytes from API, formatted by UI |
| Size | Average size | `total_size_bytes / count` |

If the UI has limited space, the minimum cards should be:

- Total count
- Success rate
- Failed count
- Average duration
- P95 duration
- Total size

### 4.5 Trends

The page should provide three trend views:

- **Count trend**
  - Total count
  - Success count
  - Failed count
  - Success rate

- **Duration trend**
  - Average duration
  - P95 duration
  - Max duration

- **Size trend**
  - Total size
  - Average size
  - Max size

The UI may use tabs for the first version:

```text
[Count] [Duration] [Size]
```

The trend chart should render a continuous local-time axis. If a bucket has no
records, the frontend should display that bucket with zero values instead of
removing it from the chart.

### 4.6 Breakdown Tables

Breakdown dimensions:

- Source
- Robot device ID
- Collector operator ID
- Robot type

Common columns:

| Column | Description |
|---|---|
| Dimension name | Source name, robot device ID, collector operator ID, or robot type name |
| Total count | Total production count |
| Success count | Successful production count |
| Failed count | Failed production count |
| Success rate | Success percentage |
| Average duration | Average production duration |
| P95 duration | P95 production duration |
| Total size | Produced data size |
| Average size | Average data size |

### 4.7 Detail Table

Recommended columns:

| Column | Description |
|---|---|
| Time | Event time or bucket time |
| Source | Data source, robot, device, or service |
| Data type | Data category |
| Task | Task or pipeline name |
| Status | Success, failed, processing, discarded |
| Count | Produced count |
| Duration | Production duration |
| Size | Produced size |
| Error reason | Error code or short message |
| Action | View task, logs, or failure details when available |

The table should support:

- Pagination
- Sorting
- Filter preservation
- Empty state
- Loading state
- Error state

## 5. Metric Definitions

Metric definitions must be confirmed before implementation. The page should not ship with ambiguous statistical semantics.

### 5.1 Time Attribution

Recommended default:

- Success and failed records are counted by `finished_at`.
- Processing records are counted by `started_at`.
- If a record has no `finished_at`, it should not be included in completed duration metrics.

This avoids ambiguity when a production task starts on one day and finishes on another day.

### 5.2 Count Metrics

| Metric | Definition |
|---|---|
| Total count | Number of production records or produced data items in the selected time range |
| Success count | Number of records that finished successfully and are usable downstream |
| Failed count | Number of records that failed, were discarded, or did not enter the next stage |
| Processing count | Number of records that started but have not reached a terminal state |
| Success rate | `success_count / total_count` |
| Failure rate | `failed_count / total_count` |

Implementation note:

- If one record represents one batch, use the record's `count` field.
- If one record represents one data item, use `count = 1`.
- The API should return numeric values, not formatted strings.

### 5.3 Duration Metrics

| Metric | Definition |
|---|---|
| Duration | `finished_at - started_at` for one completed production record |
| Total duration | Sum of all completed production durations |
| Average duration | `total_duration_ms / completed_record_count` |
| P95 duration | 95th percentile duration |
| Max duration | Longest completed production duration |

Rules:

- Duration metrics should only include records with both `started_at` and `finished_at`.
- Processing records should not be included in average, P95, or max duration.
- Failed records may be included if they have both start and finish timestamps. If included, the API should also support filtering by status.
- The backend should return milliseconds. The frontend formats values as ms, s, min, or h.

### 5.4 Size Metrics

| Metric | Definition |
|---|---|
| Size | Byte size of one produced record, file, object, or batch |
| Total size | Sum of produced data size in the selected time range |
| Success size | Sum of successfully produced data size |
| Failed size | Sum of failed partial data size, only if reliably recorded |
| Average size | `total_size_bytes / count` |
| Max size | Largest single record or batch size |

Rules:

- The backend should return bytes.
- The frontend should format bytes as B, KB, MB, GB, or TB.
- If failed partial size is not reliable, first version should only show success size and total size based on successful records.

## 6. API Design

All paths below are proposed Keystone Admin API paths. Final path prefixes should follow the existing Keystone API versioning and router conventions.

### 6.1 Common Query Parameters

| Parameter | Type | Required | Description |
|---|---|---|---|
| `start_time` | RFC3339 string | Yes | Inclusive start time |
| `end_time` | RFC3339 string | Yes | Exclusive end time |
| `granularity` | string | Trend only | `hour`, `day`, `week`, `month` |
| `timezone_offset` | string | Trend and export only | Browser timezone offset such as `+08:00` |
| `source_id` | string | No | Data source filter |
| `robot_device_id` | string | No | Robot device ID filter |
| `robot_type_id` | string | No | Robot type filter |
| `collector_operator_id` | string | No | Data collector operator ID filter |
| `sop_id` | string | No | SOP filter |
| `status` | string | No | Production status filter |
| `limit` | int | Breakdown and detail only | Page size |
| `offset` | int | Breakdown and detail only | Page offset |
| `sort_by` | string | Detail only | Sort field |
| `sort_order` | string | Detail only | `asc` or `desc` |

### 6.2 Summary

```http
GET /api/v1/admin/statistics/data-production/summary
```

Response:

```json
{
  "count": {
    "total": 1240381,
    "success": 1210902,
    "failed": 29479,
    "processing": 128,
    "success_rate": 0.9762
  },
  "duration": {
    "total_ms": 473823800,
    "avg_ms": 382,
    "p95_ms": 1420,
    "max_ms": 98321
  },
  "size": {
    "total_bytes": 9248332231,
    "success_bytes": 9173321001,
    "failed_bytes": 75011230,
    "avg_bytes": 7456,
    "max_bytes": 104857600
  },
  "compare": {
    "previous_total_count": 1103480,
    "count_change_rate": 0.124
  }
}
```

### 6.3 Trend

```http
GET /api/v1/admin/statistics/data-production/trend?granularity=day&timezone_offset=%2B08:00
```

Trend buckets should be grouped by the browser local timezone represented by
`timezone_offset`, for example `+08:00`. The `+` sign must be URL encoded as
`%2B` when written directly in a query string. The response `time` is still an
RFC3339 UTC instant for the local bucket start, so the frontend can render it in
browser local time.

Response:

```json
{
  "granularity": "day",
  "items": [
    {
      "time": "2026-05-01T00:00:00Z",
      "count": {
        "total": 182001,
        "success": 179000,
        "failed": 3001,
        "success_rate": 0.9835
      },
      "duration": {
        "avg_ms": 361,
        "p95_ms": 1390,
        "max_ms": 78221
      },
      "size": {
        "total_bytes": 1402388123,
        "avg_bytes": 7705,
        "max_bytes": 104857600
      }
    }
  ]
}
```

### 6.4 Breakdown

```http
GET /api/v1/admin/statistics/data-production/breakdown?dimension=source&limit=20&offset=0
```

Supported dimensions:

- `source`
- `robot_device`
- `collector`
- `robot_type`

Response:

```json
{
  "dimension": "source",
  "total": 156,
  "limit": 20,
  "offset": 0,
  "hasNext": true,
  "hasPrev": false,
  "items": [
    {
      "id": "source-001",
      "name": "Robot A",
      "count": {
        "total": 12000,
        "success": 11840,
        "failed": 160,
        "success_rate": 0.9867
      },
      "duration": {
        "avg_ms": 420,
        "p95_ms": 1600,
        "max_ms": 83120
      },
      "size": {
        "total_bytes": 817323112,
        "avg_bytes": 68110,
        "max_bytes": 92310122
      }
    }
  ]
}
```

### 6.5 Details

```http
GET /api/v1/admin/statistics/data-production/details
```

Response:

```json
{
  "total": 1024,
  "items": [
    {
      "id": "record-001",
      "time": "2026-05-01T10:15:20Z",
      "source_id": "source-001",
      "source_name": "Robot A",
      "robot_device_id": "AB-F0001-T0001-000001",
      "robot_type_id": "1",
      "robot_type_name": "UR5",
      "task_id": "task-001",
      "task_name": "Capture Task 001",
      "data_type": "episode",
      "status": "success",
      "count": 1,
      "duration_ms": 430,
      "size_bytes": 10485760,
      "error_code": "",
      "error_message": ""
    }
  ]
}
```

### 6.6 Export

```http
GET /api/v1/admin/statistics/data-production/export
```

Rules:

- Export must use the same query parameters as the page.
- First version can export CSV.
- CSV export should format `time` in the browser's local timezone by passing `timezone_offset`.
- CSV `id` should use the database `episodes.episode_id` value.
- CSV columns are `id`, `time`, `设备ID`, `设备型号`, `数采员工号`, `数采员姓名`, `task_id`, `sop`, `时长`, and `大小`.
- CSV `duration` should use a readable format such as `2381.65秒 (0.66h)`.
- CSV `size` should use a readable format such as `41632802361 字节 (41.63GB)`.
- Large exports should be asynchronous if they may exceed request timeout.
- Export permission must be checked separately from view permission.

## 7. Data Model Options

### 7.1 Option A: Real-time Aggregation

Aggregate from existing Task, Episode, upload, or production record tables.

Pros:

- Faster first implementation.
- Less schema and job complexity.
- Good for MVP and low data volume.

Cons:

- Long time-range queries may be slow.
- Aggregation may affect online database performance.
- P95 calculation can be expensive without proper support.

Recommended when:

- Data volume is small or medium.
- The page is used by a limited number of admins.
- MVP validation speed is more important than query performance.

### 7.2 Option B: Pre-aggregated Statistics Table

Maintain hourly or daily production statistics.

Suggested table:

```text
data_production_stats_hourly
```

Suggested fields:

```text
id
time_bucket
source_id
source_name
robot_device_id
robot_type_id
robot_type_name
collector_operator_id
status
total_count
success_count
failed_count
processing_count
total_duration_ms
avg_duration_ms
p95_duration_ms
max_duration_ms
total_size_bytes
success_size_bytes
failed_size_bytes
avg_size_bytes
max_size_bytes
created_at
updated_at
```

Pros:

- Fast queries.
- Lower impact on online production tables.
- Easier to support reports and alerting later.

Cons:

- Requires aggregation job or write-time statistics update.
- Requires backfill and correction strategy.
- More complex implementation.

Recommended when:

- Data volume is large.
- Admins frequently query long time ranges.
- Statistics will be used for reports, alerts, or SLA tracking.

### 7.3 Recommended First Implementation

Use real-time aggregation first if current data volume is manageable. Add the following guardrails:

- Add indexes for time, status, source, robot device ID, collector operator ID, and robot type filters.
- Limit maximum query range for hour granularity.
- Use pagination for details.
- Log slow queries.
- Keep the API response shape compatible with future pre-aggregated implementation.

If data volume is already large, implement hourly pre-aggregation from the beginning.

## 8. Permissions and Security

Recommended permission points:

```text
statistics:data-production:view
statistics:data-production:export
statistics:data-production:detail
```

Rules:

- Backend must enforce permissions; frontend menu hiding is not sufficient.
- Admin users can view global statistics.
- Tenant or organization scoped users can only view scoped data if multi-tenancy is enabled.
- Export should require explicit export permission.
- Detail views should not expose raw data content unless separately authorized.
- Error messages should be shortened and sanitized for table display.

## 9. Edge Cases

- `total_count = 0`: success rate should be null or 0 according to frontend convention; avoid division by zero.
- Missing `finished_at`: exclude from completed duration metrics.
- Missing `size_bytes`: exclude from size average or treat as 0 only if the data source guarantees that missing means zero.
- Long-running processing record: count as processing by `started_at`, not completed count.
- Cross-day task: success or failure should be attributed by `finished_at`.
- Retried upload or duplicate callback: statistics must be idempotent and should not double count.
- Soft-deleted tasks or records: default behavior should match existing Keystone list APIs; usually exclude soft-deleted records.

## 10. Implementation Plan

### Phase 1: Requirement Confirmation

Outputs:

- Confirm metric source tables.
- Confirm time attribution rules.
- Confirm whether one record means one data item or one batch.
- Confirm size source and reliability.
- Confirm page route and menu placement.
- Confirm permission names.

### Phase 2: Backend API

Tasks:

- Add query parameter validation.
- Implement summary endpoint.
- Implement trend endpoint.
- Implement breakdown endpoint.
- Implement detail endpoint.
- Add export endpoint if included in MVP.
- Add permission checks.
- Add database indexes or aggregation table.
- Add API tests for metric calculation.

### Phase 3: Synapse Admin UI

Tasks:

- Add menu item and route.
- Implement filter bar.
- Implement metric cards.
- Implement count, duration, and size trend charts.
- Implement breakdown tables.
- Implement detail table.
- Implement loading, empty, and error states.
- Implement export action.

### Phase 4: Validation and Release

Tasks:

- Prepare test data with success, failed, processing, missing size, and long-duration cases.
- Verify metric calculations against database queries.
- Verify time range and granularity behavior.
- Verify permissions.
- Verify export output.
- Test long range query performance.
- Release behind admin-only access.

## 11. MVP Scope

P0:

- Time range and granularity filter.
- Robot device ID, robot type, collector operator ID, SOP, and status filters.
- Summary cards for count, duration, and size.
- Count trend, duration trend, and size trend.
- Robot device ID, collector operator ID, and robot type breakdown.
- Detail table.
- View permission.

P1:

- CSV export.
- P95 duration if not available in P0.

P2:

- Failure reason aggregation.
- Automatic refresh.
- Alert thresholds.
- Custom report configuration.
- Scheduled report export.
- Drill-down from trend point to filtered details.

## 12. Acceptance Criteria

- Admin can open the Data Production Statistics page from Synapse Admin.
- Admin can query statistics by selected time range.
- The page shows count, duration, and size metrics.
- The page shows trends using the selected granularity.
- The page shows robot device ID, collector operator ID, robot type breakdown, and detail table.
- Empty, loading, and error states are handled.
- Backend APIs enforce permissions.
- Export uses the same filters as the page if export is implemented.
- Metrics are calculated according to this document.
- Large time-range queries do not significantly slow down normal Keystone operations.

## 13. Open Questions

- Which Keystone table is the authoritative source for production records: Task, Episode, upload record, or a dedicated production event table?
- Is `finished_at` available for all success and failed records?
- Is `size_bytes` available for both successful and failed records?
- Should failed records contribute to total size in the first version?
- Should statistics be organization-scoped, project-scoped, workstation-scoped, or global only?
- Should export be synchronous CSV or asynchronous report generation?
