<!--
SPDX-FileCopyrightText: 2026 ArcheBase

SPDX-License-Identifier: MulanPSL-2.0
-->

# Device Registration API Design

**Scope:** Keystone Edge API for install-time robot device registration.

## 1. Purpose

This document defines a dedicated Keystone API for registering a robot device during
installation. The API is intended for an `install.sh` script running on a robot-side
machine.

The registration flow accepts a human-facing robot type and factory, validates them
against existing Keystone records, generates a Keystone-owned ASCII `device_id`, creates a
`robots` row immediately, and returns the generated `device_id` to the script.

This API is intentionally separate from `POST /api/v1/robots`.

## 2. Goals and Non-goals

### 2.1 Goals

- Allow an installation script to register a device without knowing internal database IDs.
- Accept Chinese or other Unicode values for factory and robot type inputs.
- Resolve inputs through existing Keystone master data:
  - request `factory` maps to `factories.name`
  - request `robot_type` maps to `robot_types.model`
- Generate a stable, human-readable, ASCII-only `device_id`.
- Create a `robots` record in the same transaction as ID allocation.
- Support multiple devices registering the same factory and robot type concurrently.
- Keep every successful request non-idempotent: each success registers one new robot.

### 2.2 Non-goals

- This API does not replace admin robot management APIs.
- This API does not allow clients to provide or override `device_id`.
- This API does not create factories or robot types. They must already exist.
- This API does not deduplicate repeated calls from the same device.
- This API does not define Synapse UI behavior.

## 3. API Contract

### 3.1 Endpoint

| Method | Path | Caller |
|------|------|------|
| POST | `/api/v1/devices/register` | Robot installation script |

This endpoint must not reuse the `POST /api/v1/robots` route or request contract. The
implementation may share lower-level service code for database insertion, but the public
handler and validation rules should stay separate.

### 3.2 Request

```json
{
  "factory": "上海一厂",
  "robot_type": "搬运机器人"
}
```

Field semantics:

| Field | Required | Source of truth | Notes |
|------|------|------|------|
| `factory` | Yes | `factories.name` | May be Chinese or any UTF-8 display name |
| `robot_type` | Yes | `robot_types.model` | May be Chinese or any UTF-8 model string |

Validation rules:

- Trim surrounding whitespace before lookup.
- Reject empty fields with `400 Bad Request`.
- Look up only rows where `deleted_at IS NULL`.
- `factories.name` and `robot_types.model` are already unique among non-deleted rows in
  the current schema; the API should rely on those constraints.

### 3.3 Successful Response

`201 Created`

```json
{
  "device_id": "AB-F0003-T0012-000001",
  "factory": "上海一厂",
  "factory_id": "3",
  "robot_type": "搬运机器人",
  "robot_type_id": "12",
  "robot_id": "42"
}
```

Response field semantics:

| Field | Notes |
|------|------|
| `device_id` | Generated Keystone device identity; the install script should persist this locally |
| `factory` | Resolved `factories.name` |
| `factory_id` | Resolved `factories.id`, encoded as a string to match existing API style |
| `robot_type` | Resolved `robot_types.model` |
| `robot_type_id` | Resolved `robot_types.id`, encoded as a string to match existing API style |
| `robot_id` | Inserted `robots.id`, encoded as a string to match existing API style |

The installation script should treat `device_id` as the only required value for later Axon
configuration. Other fields are useful for logs and troubleshooting.

### 3.4 Error Responses

Error responses should follow existing Keystone style:

```json
{
  "error": "factory not found"
}
```

Recommended errors:

| Status | Condition | Error |
|------|------|------|
| `400` | Invalid JSON | `invalid request body` |
| `400` | Missing `factory` | `factory is required` |
| `400` | Missing `robot_type` | `robot_type is required` |
| `404` | No active factory by `factories.name` | `factory not found` |
| `404` | No active robot type by `robot_types.model` | `robot_type not found` |
| `500` | Sequence allocation failure | `failed to register device` |
| `500` | Robot insertion failure | `failed to register device` |

## 4. Device ID Format

### 4.1 Format

Use an ASCII-only format based on internal IDs and a grouped sequence:

```text
AB-F{factory_id:04d}-T{robot_type_id:04d}-{sequence:06d}
```

Example:

```text
AB-F0003-T0012-000001
```

### 4.2 Rationale

- The request may contain Chinese factory or robot type values, but `device_id` remains
  ASCII-safe for shell scripts, config files, URLs, object keys, logs, and third-party tools.
- `factories.name` is a display name and may change. Using `factories.id` keeps existing
  device IDs stable after renames.
- `robot_types.model` may contain Unicode or punctuation. Using `robot_types.id` avoids
  transliteration and normalization problems.
- The ID is still human-readable enough for support workflows because it exposes the
  factory ID, robot type ID, and allocation sequence.

### 4.3 Sequence Scope

The sequence is scoped by `(factory_id, robot_type_id)`.

Example allocations:

| Factory | Robot type | Generated device IDs |
|------|------|------|
| `F0003` | `T0012` | `AB-F0003-T0012-000001`, `AB-F0003-T0012-000002` |
| `F0003` | `T0013` | `AB-F0003-T0013-000001` |
| `F0004` | `T0012` | `AB-F0004-T0012-000001` |

This keeps the trailing number meaningful within a factory/type pair and prevents one busy
factory or robot type from consuming a global sequence.

## 5. Data Model Changes

### 5.1 New Sequence Table

Add a sequence table dedicated to device ID allocation:

```sql
CREATE TABLE IF NOT EXISTS device_id_sequences (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    factory_id BIGINT NOT NULL,
    robot_type_id BIGINT NOT NULL,
    next_sequence BIGINT NOT NULL DEFAULT 1,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE INDEX idx_factory_robot_type (factory_id, robot_type_id),
    INDEX idx_factory (factory_id),
    INDEX idx_robot_type (robot_type_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

`next_sequence` stores the next value to allocate. The transaction reads it, formats the
device ID with that value, then increments it.

### 5.2 Existing Table Usage

The registration flow uses existing tables as follows:

| Table | Usage |
|------|------|
| `factories` | Resolve request `factory` by `name` |
| `robot_types` | Resolve request `robot_type` by `model` |
| `device_id_sequences` | Allocate the next sequence for `(factory_id, robot_type_id)` |
| `robots` | Insert the registered robot immediately |

Inserted `robots` values:

| Column | Value |
|------|------|
| `robot_type_id` | Resolved `robot_types.id` |
| `device_id` | Generated ID |
| `factory_id` | Resolved `factories.id` |
| `asset_id` | `NULL` |
| `status` | `active` |
| `metadata` | `{}` or registration metadata if needed later |

The existing soft-delete-aware uniqueness constraint on `robots.device_id` remains the
final protection against duplicate device IDs.

## 6. Registration Flow

The API should run the full registration in one database transaction:

```text
1. Parse and validate JSON request.
2. Trim `factory` and `robot_type`.
3. Resolve factory:
   SELECT id, name FROM factories
   WHERE name = ? AND deleted_at IS NULL
4. Resolve robot type:
   SELECT id, model FROM robot_types
   WHERE model = ? AND deleted_at IS NULL
5. Initialize the sequence row if it does not exist.
6. Lock the sequence row with SELECT ... FOR UPDATE.
7. Allocate sequence = next_sequence.
8. Update next_sequence = next_sequence + 1.
9. Format device_id.
10. Insert robots row.
11. Commit.
12. Return device_id and resolved IDs.
```

## 7. Concurrency Semantics

Multiple devices may run `install.sh` at the same time with identical payloads.

Example concurrent payload:

```json
{
  "factory": "上海一厂",
  "robot_type": "搬运机器人"
}
```

Expected result:

```text
AB-F0003-T0012-000001
AB-F0003-T0012-000002
AB-F0003-T0012-000003
```

The sequence row for `(factory_id, robot_type_id)` must be locked with
`SELECT ... FOR UPDATE` before reading and incrementing `next_sequence`. Concurrent
requests for the same pair should serialize at the sequence row. Requests for different
pairs can allocate independently.

The first request for a new `(factory_id, robot_type_id)` pair must be safe under
concurrency. A recommended pattern for MySQL:

```sql
INSERT INTO device_id_sequences (factory_id, robot_type_id, next_sequence)
VALUES (?, ?, 1)
ON DUPLICATE KEY UPDATE updated_at = updated_at;
```

Then lock the row:

```sql
SELECT next_sequence
FROM device_id_sequences
WHERE factory_id = ? AND robot_type_id = ?
FOR UPDATE;
```

## 8. Idempotency Decision

This API is non-idempotent by design.

Each successful request creates a new `robots` row and returns a new `device_id`, even when
the request body is identical to a previous request. This matches the current requirement:
several devices may legitimately register with the same factory and robot type at the same
time.

Consequence:

- If `install.sh` succeeds but the response is lost, retrying may create another robot.
- Keystone will not attempt to determine whether two identical requests came from the same
  physical machine.
- The installation script should call the API only when no local `device_id` exists.
- After receiving `device_id`, the script should persist it locally before starting Axon
  services.

If retry-safe registration is required later, add an explicit client-generated
idempotency key or installation ID. It should not be inferred from `factory` and
`robot_type`.

## 9. Authentication

Because this endpoint is intended for `install.sh`, it should not require a full admin user
workflow. A dedicated registration credential is recommended.

Recommended first version:

```http
Authorization: Bearer <registration-token>
```

Configuration:

```text
KEYSTONE_DEVICE_REGISTRATION_TOKEN=...
```

Behavior:

- Missing or invalid token returns `401 Unauthorized`.
- The token grants only device registration, not general robot administration.
- A future version may support factory-scoped tokens, but the first version can use one
  deployment-level token.

## 10. Install Script Contract

The installation script should:

1. Accept `factory` and `robot_type` from installation parameters or environment variables.
2. Call `POST /api/v1/devices/register`.
3. Read `device_id` from the JSON response.
4. Persist `device_id` in the local Axon/Keystone configuration.
5. Skip registration on future runs if a local `device_id` already exists.

Example request:

```bash
curl -fsS \
  -H "Authorization: Bearer ${KEYSTONE_DEVICE_REGISTRATION_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"factory":"上海一厂","robot_type":"搬运机器人"}' \
  "${KEYSTONE_URL}/api/v1/devices/register"
```

## 11. Implementation Notes

- Add a new handler, for example `DeviceRegistrationHandler`, instead of extending
  `RobotHandler.CreateRobot`.
- Keep the public request payload independent from `CreateRobotRequest`.
- Put sequence allocation and robot insertion in a transaction.
- Use UTC timestamps, matching existing Keystone handler behavior.
- Log runtime failures with a `[DEVICE]` or `[ROBOT]` component prefix.
- Regenerate Swagger docs after implementation with:

```bash
swag init -g internal/server/server.go -o docs
```

## 12. Test Plan

Recommended Keystone tests:

- Reject missing or invalid JSON body.
- Reject empty `factory`.
- Reject empty `robot_type`.
- Return `404` for unknown factory name.
- Return `404` for unknown robot type model.
- Create a `robots` row when registration succeeds.
- Return ASCII-only `device_id` for Unicode factory/type inputs.
- Allocate sequential IDs for repeated registrations with the same pair.
- Allocate independent sequences for different factory/type pairs.
- Simulate concurrent registration for the same pair and assert no duplicate `device_id`.
- Verify repeated identical successful requests create distinct robots.
- Reject missing or invalid registration token if authentication is enabled.

## 13. Open Questions

- Should the registration token be enabled in the first implementation or deferred to a
  deployment-level network control?
- Should the response include the full inserted robot object, or only the registration
  fields listed in this document?
- Should future versions add stable ASCII codes to `factories` and `robot_types` for more
  readable IDs like `AB-SH01-AMR-000001`?
