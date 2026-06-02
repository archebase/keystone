<!--
SPDX-FileCopyrightText: 2026 ArcheBase

SPDX-License-Identifier: MulanPSL-2.0
-->

# CLI Cloud Sync Sidepath Design

## 1. Overview

This document defines a sidepath for syncing one Keystone episode to cloud by
running the data-platform `dp` CLI from Keystone, while keeping the existing
`SyncWorker -> data-platform DataGateway` flow unchanged.

The sidepath is intended for controlled operations and emergency recovery, not
as the default production upload path.

Target flow:

```text
Synapse "CLI sync to cloud" button
        -> Keystone CLI sync API
        -> Keystone CLI sync runner
        -> download MCAP from Keystone MinIO to a temporary local file
        -> read sidecar JSON and flatten scalar metadata into --tag arguments
        -> dp --json data upload <temporary-file> --tag ...
        -> record dp result
        -> mark the episode cloud_synced on success
```

The existing cloud sync flow remains:

```text
Synapse normal sync action
        -> Keystone SyncWorker queue
        -> Keystone Go uploader
        -> data-platform DataGateway
        -> cloud object storage
```

## 2. Goals

- Add a Synapse action named `CLI sync to cloud` for a single episode.
- Keep the current `POST /api/v1/sync/episodes/:id` behavior unchanged.
- Keep the current `SyncWorker` queue, retry, backoff, and auto-scan behavior
  unchanged.
- Upload the episode MCAP through `dp data upload`.
- Read the episode sidecar JSON and pass scalar metadata through
  `dp data upload --tag`. Array fields are skipped in the first version so the
  existing `dp` CLI does not need to change its comma-separated tag parser.
- Persist CLI run audit data, including `fileId`, `logicalUploadId`, `uploadId`,
  `objectKey`, command duration, and sanitized error output.
- On successful CLI upload, update:
  - `episodes.cloud_synced = TRUE`
  - `episodes.cloud_synced_at`
  - `episodes.cloud_mcap_path`
  - `episodes.cloud_processed = FALSE`
- On successful CLI upload, append a normal `sync_logs.completed` row so the
  existing Cloud Sync Center summary can show the episode as synced.

## 3. Non-Goals

- Do not replace `SyncWorker`.
- Do not make CLI sync the default action.
- Do not add batch CLI sync in the first version.
- Do not retry CLI sync automatically.
- Do not let the existing `SyncWorker` process CLI pending or failed states.
- Do not upload the sidecar JSON object through the CLI sidepath in the first
  version. Its scalar content is still required as upload tags for the MCAP
  object.
- Do not expose `dp` command output containing secrets to the browser.

## 4. Recommended Architecture

Use a separate `cli_sync_runs` table for pending, in-progress, and failed CLI
runs. This avoids putting CLI `pending` or `failed` rows into `sync_logs`, which
would otherwise be visible to the existing `SyncWorker` polling queries.

Only after the CLI upload succeeds should Keystone append a `sync_logs` row with
`status = 'completed'`. That completed row is terminal and will not be retried
by the existing worker.

```text
api request
  -> insert cli_sync_runs(status='pending')
  -> background runner claims run
  -> cli_sync_runs(status='in_progress')
  -> read sidecar JSON tags
  -> run dp upload
  -> success:
       cli_sync_runs(status='completed', dp ids...)
       sync_logs(status='completed', destination_path=objectKey...)
       episodes.cloud_synced = TRUE
  -> failure:
       cli_sync_runs(status='failed', sanitized error...)
       no sync_logs write
       episodes unchanged
```

This keeps normal sync history authoritative while still allowing CLI success to
close the episode's cloud sync state.

## 5. Backend API

### 5.1 Trigger CLI Sync

```http
POST /api/v1/sync/episodes/:id/cli
```

Request body:

```json
{}
```

Response:

```json
{
  "status": "accepted",
  "episode_id": 123,
  "run_id": 456,
  "message": "episode accepted for CLI cloud sync"
}
```

Validation:

| Check | Response |
|---|---|
| CLI sync feature disabled | `503 Service Unavailable` |
| invalid episode id | `400 Bad Request` |
| episode missing or deleted | `404 Not Found` |
| `qa_status` is not `approved` or `inspector_approved` | `400 Bad Request` |
| `cloud_synced = TRUE` | `409 Conflict` |
| latest normal sync log is `pending` or `in_progress` | `409 Conflict` |
| existing CLI run is `pending` or `in_progress` | `409 Conflict` |
| CLI runner queue is full | `429 Too Many Requests` |

The endpoint must return after the run is queued. It must not hold the HTTP
request open for the entire upload.

### 5.2 Get Latest CLI Sync Run

```http
GET /api/v1/sync/episodes/:id/cli/status
```

Response:

```json
{
  "id": 456,
  "episode_id": 123,
  "status": "in_progress",
  "file_id": null,
  "logical_upload_id": null,
  "upload_id": null,
  "object_key": null,
  "file_size": null,
  "started_at": "2026-06-02T08:10:00Z",
  "completed_at": null,
  "error_message": null
}
```

The frontend uses this endpoint to show button state while the sidepath is
running. The normal sync summary remains sourced from `sync_logs`.

## 6. Data Model

### 6.1 New Table

```sql
CREATE TABLE IF NOT EXISTS cli_sync_runs (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    episode_id BIGINT NOT NULL,
    status ENUM('pending', 'in_progress', 'completed', 'failed') NOT NULL DEFAULT 'pending',
    source_path VARCHAR(1024),
    temp_path VARCHAR(1024),
    dp_config_path VARCHAR(1024),
    file_id VARCHAR(255),
    logical_upload_id VARCHAR(255),
    upload_id VARCHAR(255),
    bucket VARCHAR(255),
    object_key VARCHAR(1024),
    file_size BIGINT,
    oss_object_etag VARCHAR(255),
    duration_sec INT,
    error_message TEXT,
    stdout_json JSON DEFAULT NULL,
    started_at TIMESTAMP NULL,
    completed_at TIMESTAMP NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    INDEX idx_cli_sync_episode (episode_id),
    INDEX idx_cli_sync_status (status),
    INDEX idx_cli_sync_created (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

### 6.2 Why Not Store Pending CLI Runs In `sync_logs`

The existing worker polls latest `sync_logs.status = 'pending'` rows and
retryable `failed` rows. If CLI pending or failed rows are written to
`sync_logs`, the normal worker can claim them and run the regular data-gateway
upload path. That would mix the two channels and violate this design's goal.

For this reason:

- `cli_sync_runs` owns CLI pending, in-progress, and failed states.
- `sync_logs` receives a completed row only after CLI upload succeeds.
- `episodes.cloud_synced` is updated only after CLI upload succeeds.

### 6.3 Successful CLI Sync Log Row

On success, insert:

```sql
INSERT INTO sync_logs (
    episode_id,
    source_path,
    destination_path,
    status,
    bytes_transferred,
    duration_sec,
    attempt_count,
    started_at,
    completed_at
) VALUES (?, ?, ?, 'completed', ?, ?, 1, ?, ?);
```

Use `destination_path = dp.objectKey`. Store `dp.fileId` and
`dp.logicalUploadId` in `cli_sync_runs`.

## 7. CLI Runner

### 7.1 Command Construction

The runner must call `dp` without a shell:

```text
exec.CommandContext(ctx, dpBin,
  "--config", dpConfigPath,
  "--json",
  "data", "upload", tempFile,
  "--device", "<robot device id>",
  "--tag", "episode_id=<episode public id>",
  "--tag", "keystone_episode_id=<numeric id>",
  "--tag", "device_id=<robot device id>",
  "--tag", "sync_channel=keystone_cli",
  "--tag", "<flattened sidecar key=value>",
  "--hint", "source=keystone_cli_sync",
)
```

Do not build a single shell command string.
The device id is resolved from the episode workstation robot
(`robots.device_id`, falling back to `workstations.robot_serial`). The selected
`dp` config must contain a matching initialized device profile in `devices[]`.

### 7.2 Tags

Required tags:

| Tag | Value |
|---|---|
| `episode_id` | `episodes.episode_id` |
| `keystone_episode_id` | numeric `episodes.id` |
| `device_id` | `robots.device_id` resolved through the episode workstation |
| `sync_channel` | `keystone_cli` |

Required sidecar-derived tags:

| Source | Handling |
|---|---|
| sidecar JSON scalar fields | Flatten to string key/value pairs and pass as repeated `--tag key=value` arguments |
| sidecar JSON arrays | Skip in the first version |
| `topics_summary` | Exclude, matching the existing worker's filtering intent |
| nested objects | Flatten with dot notation |

Recommended tags:

| Tag | Value |
|---|---|
| `task_id` | `episodes.task_id`, when available |
| `factory_id` | `episodes.factory_id`, when available |
| `organization_id` | `episodes.organization_id`, when available |

The CLI sidepath uploads only the MCAP file body, but sidecar JSON metadata is
not optional. Scalar sidecar fields must be included as tags; array fields are
left out for the first version. If `sidecar_path` is missing, unreadable, or
malformed, the CLI run should fail before invoking `dp`. This is stricter than
the current worker's best-effort sidecar handling and prevents cloud objects
from being created without the metadata required for filtering.

The implementation must enforce a max tag count and max tag size so the CLI
command line cannot exceed OS limits.

### 7.3 Temporary File Handling

The runner downloads the MCAP from Keystone MinIO to a temporary file before
calling `dp`.

Requirements:

- Use a dedicated directory such as `/var/lib/keystone/cli-sync`.
- Create temporary files with mode `0600`.
- Delete the temporary file after success or failure unless
  `KEYSTONE_CLI_SYNC_KEEP_TEMP=true`.
- Refuse to start if the temp directory is not writable.
- Check free disk space before download when a disk watermark helper is
  available.

### 7.4 JSON Output Parsing

Expected `dp --json data upload` fields:

```json
{
  "logicalUploadId": "logical-1",
  "fileId": "file-1",
  "bucket": "bucket-a",
  "objectKey": "objects/file-1.mcap",
  "fileSize": 123456789,
  "ossObjectEtag": "etag",
  "identity": "api-key",
  "deviceId": null
}
```

The runner must validate that `fileId`, `logicalUploadId`, `objectKey`, and
`fileSize` are present before marking the run completed.

## 8. Configuration

Add a separate config group rather than reusing `SyncConfig`.

| Environment variable | Default | Description |
|---|---|---|
| `KEYSTONE_CLI_SYNC_ENABLED` | `false` | Enables the sidepath API and runner |
| `KEYSTONE_CLI_SYNC_DP_BIN` | `dp` | Path or binary name for the data-platform CLI |
| `KEYSTONE_CLI_SYNC_DP_CONFIG` | empty | SDK config JSON path passed to `dp --config` |
| `KEYSTONE_CLI_SYNC_TEMP_DIR` | `/var/lib/keystone/cli-sync` | Temporary MCAP staging directory |
| `KEYSTONE_CLI_SYNC_MAX_CONCURRENT` | `1` | Max concurrent CLI uploads |
| `KEYSTONE_CLI_SYNC_QUEUE_SIZE` | `16` | Max queued CLI runs |
| `KEYSTONE_CLI_SYNC_TIMEOUT_SEC` | `7200` | Per-run timeout |
| `KEYSTONE_CLI_SYNC_KEEP_TEMP` | `false` | Keeps staged files for debugging |
| `KEYSTONE_CLI_SYNC_MAX_TAGS` | `128` | Max tags passed to CLI |
| `KEYSTONE_CLI_SYNC_MAX_TAG_BYTES` | `65536` | Max total encoded tag bytes |

Startup validation when enabled:

- `dp` binary exists and is executable.
- `KEYSTONE_CLI_SYNC_DP_CONFIG` is set and readable.
- Temp directory exists or can be created.
- Temp directory is writable.

## 9. Frontend Behavior

### 9.1 Cloud Sync Center

Add a row action next to existing `Retry` and `History` actions:

```text
CLI sync to cloud
```

Show it only when the feature flag from config/status says CLI sync is enabled.

Disable it when:

- the row status is `pending` or `in_progress`;
- the row status is `completed`;
- the episode has an active CLI run;
- a row action is already running;
- the user does not have admin permission.

After clicking:

1. Call `POST /api/v1/sync/episodes/:id/cli`.
2. Show the row as `CLI queued` or `CLI syncing` using the CLI status endpoint.
3. Poll `GET /api/v1/sync/episodes/:id/cli/status`.
4. On CLI completion, refresh normal sync summaries.
5. On CLI failure, keep the normal sync row unchanged and show the sanitized CLI
   error.

### 9.2 Episode Detail

Add the same action for approved, unsynced episodes. This is important because
an approved unsynced episode may not yet have any `sync_logs` row and therefore
may not appear in the Cloud Sync Center table.

## 10. Security

- The trigger API must require admin authorization.
- `dp` must be launched through `exec.CommandContext`, never through a shell.
- Do not pass API keys on the command line.
- Store credentials only in the `dp` config file with restrictive permissions.
- Redact stdout, stderr, paths, and errors before returning anything to the
  frontend.
- Do not log full `dp` config contents.
- Do not log temporary object storage credentials or presigned URLs.
- Limit concurrent CLI runs to protect Keystone CPU, disk, and network.

## 11. Concurrency And Races

Keystone should prevent multiple active CLI runs for the same episode by checking
`cli_sync_runs.status IN ('pending', 'in_progress')` inside a transaction.

Before marking success, lock the `episodes` row and re-check `cloud_synced`.

If the normal SyncWorker synced the episode while the CLI run was uploading:

- mark the CLI run as completed with its `dp` result;
- do not overwrite `episodes.cloud_mcap_path`;
- do not insert a second `sync_logs.completed` row unless product explicitly
  wants duplicate completed history;
- include a `duplicate_after_upload` marker in `cli_sync_runs.stdout_json` or a
  dedicated metadata field if one is added later.

Residual risk: if `dp` upload succeeds but Keystone crashes before recording the
result, a later manual CLI retry can upload a duplicate object. This is accepted
for the sidepath's emergency-use scope. A future implementation can reduce this
by adding a data-platform idempotency key or a server-side upload lookup by
`episode_id`.

## 12. Rollout Plan

1. Add `cli_sync_runs` migration and model helpers.
2. Add CLI sync config with default disabled.
3. Add the backend runner with a fake `dp` executable test fixture.
4. Add `POST /sync/episodes/:id/cli` and latest status endpoint.
5. Add Synapse API wrapper methods.
6. Add Episode Detail button.
7. Add Cloud Sync Center row button and CLI status overlay.
8. Enable only in a staging environment.
9. Run one approved small MCAP through CLI sync and verify:
   - data-platform object is visible;
   - expected sidecar JSON scalar fields are visible as data-platform raw tags;
   - `cli_sync_runs` contains `fileId` and `logicalUploadId`;
   - `sync_logs` has a completed row;
   - `episodes.cloud_synced = TRUE`;
   - normal SyncWorker does not retry the episode.

## 13. Test Plan

Backend unit tests:

- rejects disabled feature;
- rejects non-approved episodes;
- rejects already cloud-synced episodes;
- rejects active normal sync rows;
- rejects active CLI runs;
- fails when sidecar JSON is missing, unreadable, or malformed;
- passes flattened sidecar JSON scalar fields as repeated `--tag` arguments;
- builds `dp` argv without a shell;
- parses valid `dp --json` output;
- rejects missing `fileId`, `logicalUploadId`, or `objectKey`;
- redacts stderr before API response;
- records failed CLI runs without writing `sync_logs`;
- records successful CLI runs and inserts one completed `sync_logs` row.

Backend integration tests:

- fake MinIO object is staged to temp file;
- fake `dp` executable receives the expected args;
- temp file is deleted after success and failure;
- success updates `episodes.cloud_synced`;
- normal sync summary sees the completed row after success.

Frontend tests:

- button is hidden when CLI sync config is disabled;
- button is disabled for completed, pending, and in-progress rows;
- click calls `triggerEpisodeCli`;
- active CLI status changes row action text;
- completed CLI run refreshes normal summaries;
- failed CLI run shows sanitized error and leaves normal row state unchanged.

## 14. Open Questions

- Should CLI failures appear in the Cloud Sync Center main table, or only as a
  per-episode CLI status/badge?
- Should a successful CLI sync always append `sync_logs.completed`, even when
  the latest normal row is already completed by a race?
- Does data-platform need an explicit idempotency key for `dp data upload` so
  crash-after-upload can be recovered without duplicate objects?
- Should the `dp` config use a site API key or a device profile for the Keystone
  edge site?
