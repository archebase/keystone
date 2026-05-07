<!--
SPDX-FileCopyrightText: 2026 ArcheBase

SPDX-License-Identifier: MulanPSL-2.0
-->

# Device Registration API

**Scope:** Keystone Edge API for install-time robot device registration.

## 1. Overview

`POST /api/v1/devices/register` registers one robot device for an installation script and
returns a Keystone-generated `device_id`.

The API is intentionally separate from `POST /api/v1/robots`. The caller provides only a
factory display name and a robot type model. Keystone validates both values against
existing master data, allocates a human-readable ASCII device ID, inserts a row into
`robots`, and returns the generated ID.

This API does not require authentication in the first version. Deployment-level network
access control is expected to protect the endpoint.

## 2. Endpoint

| Method | Path | Auth | Caller |
|------|------|------|------|
| POST | `/api/v1/devices/register` | None | Robot-side `install.sh` |

## 3. Request

```json
{
  "factory": "Factory Shanghai",
  "robot_type": "SynGloves"
}
```

| Field | Required | Lookup target | Notes |
|------|------|------|------|
| `factory` | Yes | `factories.name` | UTF-8 is allowed; value is trimmed before lookup |
| `robot_type` | Yes | `robot_types.model` | UTF-8 is allowed; value is trimmed before lookup |

Both master data records must already exist and must not be soft-deleted.

## 4. Response

Success returns `201 Created`.

```json
{
  "device_id": "AB-F0001-T0003-000001",
  "factory": "Factory Shanghai",
  "factory_id": "1",
  "robot_type": "SynGloves",
  "robot_type_id": "3",
  "robot_id": "9"
}
```

| Field | Meaning |
|------|------|
| `device_id` | Generated device identity; the install script should persist this locally |
| `factory` | Resolved `factories.name` |
| `factory_id` | Resolved `factories.id`, encoded as a string for existing API style |
| `robot_type` | Resolved `robot_types.model` |
| `robot_type_id` | Resolved `robot_types.id`, encoded as a string for existing API style |
| `robot_id` | Inserted `robots.id`, encoded as a string for existing API style |

## 5. Error Responses

Errors follow Keystone's usual JSON shape:

```json
{
  "error": "factory not found"
}
```

| Status | Condition | Error |
|------|------|------|
| `400` | Invalid JSON body | `invalid request body` |
| `400` | Empty or missing `factory` | `factory is required` |
| `400` | Empty or missing `robot_type` | `robot_type is required` |
| `404` | No active factory with matching `factories.name` | `factory not found` |
| `404` | No active robot type with matching `robot_types.model` | `robot_type not found` |
| `500` | Allocation, insert, or transaction failure | `failed to register device` |

## 6. Device ID Format

Keystone generates ASCII-only IDs:

```text
AB-F{factory_id:04d}-T{robot_type_id:04d}-{sequence:06d}
```

Example:

```text
AB-F0001-T0003-000001
```

Rules:

- `factory_id` comes from `factories.id`.
- `robot_type_id` comes from `robot_types.id`.
- `sequence` is scoped to the `(factory_id, robot_type_id)` pair.
- Width markers are minimum zero-padding widths; larger values are not truncated.
- The generated ID does not include Chinese or other non-ASCII input text.

Example allocations:

| Factory ID | Robot type ID | Generated device IDs |
|------|------|------|
| `1` | `3` | `AB-F0001-T0003-000001`, `AB-F0001-T0003-000002` |
| `1` | `2` | `AB-F0001-T0002-000001` |
| `4` | `3` | `AB-F0004-T0003-000001` |

## 7. Side Effects

Every successful call creates one new `robots` row:

| Column | Value |
|------|------|
| `robot_type_id` | Resolved `robot_types.id` |
| `device_id` | Generated device ID |
| `factory_id` | Resolved `factories.id` |
| `asset_id` | `NULL` |
| `status` | `active` |
| `metadata` | `{}` |

The API is non-idempotent. Repeating the same request successfully creates another robot
and returns another `device_id`. The install script should call this endpoint only when no
local `device_id` already exists.

## 8. Concurrency

Concurrent requests with the same `factory` and `robot_type` are supported. Keystone uses
`device_id_sequences` to serialize allocation for each `(factory_id, robot_type_id)` pair.

Sequence initialization:

```sql
INSERT INTO device_id_sequences (factory_id, robot_type_id, next_sequence)
VALUES (?, ?, 1)
ON DUPLICATE KEY UPDATE updated_at = updated_at;
```

Sequence allocation:

```sql
SELECT next_sequence
FROM device_id_sequences
WHERE factory_id = ? AND robot_type_id = ?
FOR UPDATE;
```

The selected `next_sequence` is used in `device_id`, then Keystone increments
`next_sequence` in the same transaction before inserting the robot.

## 9. Install Script Usage

Example:

```bash
curl -fsS \
  -H "Content-Type: application/json" \
  -d '{"factory":"Factory Shanghai","robot_type":"SynGloves"}' \
  "${KEYSTONE_URL}/api/v1/devices/register"
```

Expected script behavior:

1. Read `factory` and `robot_type` from install parameters or environment variables.
2. Skip registration if a local `device_id` already exists.
3. Call `POST /api/v1/devices/register`.
4. Persist `device_id` locally before starting Axon services.
5. Use the persisted `device_id` for later Keystone and Axon connections.

## 10. Implementation Notes

Implementation files:

| File | Purpose |
|------|------|
| `internal/api/handlers/device_registration.go` | Request validation, transaction, sequence allocation, robot insertion |
| `internal/server/server.go` | Handler construction and route registration |
| `internal/storage/database/migrations/000002_device_id_sequences.up.sql` | Sequence table migration |
| `internal/storage/database/migrations/000002_device_id_sequences.down.sql` | Sequence table rollback |
| `internal/api/handlers/device_registration_test.go` | Focused handler and route tests |

Validation performed during implementation:

```bash
go test ./internal/api/handlers -run 'TestDeviceRegistration|TestFormatRegisteredDeviceID|TestDeviceRegistrationRoutes' -v
go test ./...
```

Manual API verification was performed against a local Keystone instance:

```json
{
  "device_id": "AB-F0001-T0003-000001",
  "factory": "Factory Shanghai",
  "factory_id": "1",
  "robot_type": "SynGloves",
  "robot_type_id": "3",
  "robot_id": "9"
}
```
