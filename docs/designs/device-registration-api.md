<!--
SPDX-FileCopyrightText: 2026 ArcheBase

SPDX-License-Identifier: MulanPSL-2.0
-->

# Device Registration API

**Scope:** Keystone Edge API for install-time robot device registration.

## 1. Overview

`POST /api/v1/devices/register` registers one robot device for an installation script and
returns a Keystone-generated `device_id` and a one-time plaintext WebSocket client token.

The API is intentionally separate from `POST /api/v1/robots`. The caller provides only a
factory display name and a robot type model. Keystone validates both values against
existing master data, allocates a human-readable ASCII device ID, inserts a row into
`robots`, inserts a hashed recorder WebSocket client token, and returns the generated ID
plus plaintext token once.

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
  "robot_id": "9",
  "ws_client_auth_token": "kws_v1_3Z2iX5lFh7mYxLQd9P0sAqzF2Z3w4R5t6U7v8W9x0Y",
  "callback_allowlist": {
    "allowed_host": "192.168.1.20:9999",
    "allowed_path_prefix": "/api/v1/callbacks/"
  }
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
| `ws_client_auth_token` | One-time plaintext token for Axon recorder WebSocket client authentication |
| `callback_allowlist.allowed_host` | Host and optional port that Axon recorder is allowed to call for Keystone callbacks |
| `callback_allowlist.allowed_path_prefix` | Callback path prefix allowed for recorder HTTP callbacks |

Keystone does not return `ws_client_auth_token_file`. Axon owns local file path policy. If
Keystone returns `ws_client_auth_token` without a file field, `axon_config register` writes
the token to its default `/var/lib/axon/secrets/ws_client.token`, or to the path supplied by
the `--ws-client-token-file` option.

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
| `500` | Allocation, robot insert, token insert, or transaction failure | `failed to register device` |

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

Every successful call also creates one active `ws_client_auth_tokens` row for the inserted
robot. If token generation or token insertion fails, the whole registration transaction
rolls back and no robot is created.

The API is non-idempotent. Repeating the same request successfully creates another robot
and returns another `device_id`. The install script should call this endpoint only when no
local `device_id` already exists.

## 8. WebSocket Client Token

Keystone signs no long-lived JWT here. It generates an opaque random token:

```text
kws_v1_<base64url-random>
```

Generation rules:

- Generate 32 cryptographically random bytes.
- Encode with URL-safe base64 without padding.
- Prefix with `kws_v1_`.
- Hash the complete token string with SHA-256.
- Store only the SHA-256 hex digest.
- Return plaintext only in the successful registration response.

Keystone must not log the plaintext token, persist the plaintext token, or echo it in error
responses. Swagger examples should use placeholders, not generated secrets.

The token table stores only `robot_id`, not `device_id`. `robots.device_id` remains the
single source of truth. If a robot's `device_id` is later changed and Axon is updated to use
the new device ID, the existing token can still authenticate that robot. If the old token
should stop working, a future revoke or rotate flow must revoke it explicitly.

Migration files:

```text
internal/storage/database/migrations/000007_ws_client_auth_tokens.up.sql
internal/storage/database/migrations/000007_ws_client_auth_tokens.down.sql
```

Table shape:

```sql
CREATE TABLE IF NOT EXISTS ws_client_auth_tokens (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    robot_id BIGINT NOT NULL,
    token_hash CHAR(64) NOT NULL,
    token_version VARCHAR(16) NOT NULL DEFAULT 'kws_v1',
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    last_rotated_at TIMESTAMP NULL,
    last_used_at TIMESTAMP NULL,
    revoked_at TIMESTAMP NULL,
    UNIQUE INDEX idx_ws_client_token_hash (token_hash),
    INDEX idx_ws_client_robot_active (robot_id, revoked_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

The table intentionally does not define a database foreign key. Existing Keystone schema
style keeps these relationships application-managed, and this avoids introducing migration
ordering and SQLite fixture complexity.

This version does not provide a rotate endpoint. `last_rotated_at` and `revoked_at` are
reserved for a future explicit token rotation or revocation flow.

## 9. Recorder WebSocket Authentication

This token is required only for the Axon recorder WebSocket in this implementation. Axon
transfer WebSocket remains unchanged because Axon transfer does not currently send a
Bearer token during its WebSocket handshake.

Keystone validates the recorder WebSocket before `websocket.Accept`.

Accepted token transport:

```http
Authorization: Bearer kws_v1_...
```

Unsupported token transports:

```text
?token=...
X-API-Key: ...
Sec-WebSocket-Protocol: ...
```

Validation query shape:

```sql
SELECT t.id
FROM ws_client_auth_tokens t
JOIN robots r ON r.id = t.robot_id
WHERE r.device_id = ?
  AND t.token_hash = ?
  AND t.revoked_at IS NULL
  AND r.status = 'active'
  AND r.deleted_at IS NULL
LIMIT 1;
```

The `?` values are:

1. `device_id` from the recorder WebSocket URL.
2. SHA-256 hex digest of the Bearer token.

Successful validation updates `last_used_at` best-effort once during the handshake. Failure
to update `last_used_at` is logged but does not reject an otherwise valid connection.

If `RecorderHandler` has no database handle, Keystone rejects recorder WebSocket
connections with `503 Service Unavailable`; it must not bypass authentication.

Authentication failure handling:

| Condition | Status | Response |
|------|------|------|
| Missing `Authorization` | `401` | `{"error":"unauthorized"}` |
| Non-Bearer authorization | `401` | `{"error":"unauthorized"}` |
| Invalid token format | `401` | `{"error":"unauthorized"}` |
| Token hash not found | `401` | `{"error":"unauthorized"}` |
| Token belongs to another robot | `401` | `{"error":"unauthorized"}` |
| Token revoked | `401` | `{"error":"unauthorized"}` |
| Robot deleted or not active | `401` | `{"error":"unauthorized"}` |
| Database unavailable in handler | `503` | `{"error":"service unavailable"}` |

`401` responses include:

```http
WWW-Authenticate: Bearer
```

Logs may include `device_id` and a broad reason such as `missing bearer token` or
`invalid token`, but must not include token plaintext or distinguish "not found" from
"belongs to another device".

## 10. Concurrency

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

Token insertion is part of the same transaction. If inserting the token row fails, the
robot insert and device sequence increment are rolled back with the transaction.

## 11. Install Script Usage

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
5. If `ws_client_auth_token` is present, write it to the Axon ws client token file.
6. Use the persisted `device_id` and token file for later Keystone recorder WebSocket
   connections.

`axon_config register` already implements the token-file behavior. If the response omits
`ws_client_auth_token_file`, it writes the token to `/var/lib/axon/secrets/ws_client.token`
or the path supplied by `--ws-client-token-file`.

## 12. Implementation Notes

Implementation files:

| File | Purpose |
|------|------|
| `internal/api/handlers/device_registration.go` | Request validation, transaction, sequence allocation, robot insertion |
| `internal/api/handlers/ws_client_auth.go` | Token generation, hashing, storage, and recorder WebSocket validation |
| `internal/server/server.go` | Handler construction and route registration |
| `internal/storage/database/migrations/000002_device_id_sequences.up.sql` | Sequence table migration |
| `internal/storage/database/migrations/000002_device_id_sequences.down.sql` | Sequence table rollback |
| `internal/storage/database/migrations/000007_ws_client_auth_tokens.up.sql` | WebSocket client token table migration |
| `internal/storage/database/migrations/000007_ws_client_auth_tokens.down.sql` | WebSocket client token table rollback |
| `internal/api/handlers/device_registration_test.go` | Focused handler and route tests |
| `internal/api/handlers/recorder_ws_auth_test.go` | Recorder WebSocket token authentication tests |

TDD coverage should include:

1. Register success returns `ws_client_auth_token` with `kws_v1_` prefix.
2. Register success stores only token hash, not plaintext.
3. Token table insert failure rolls back robot creation.
4. Recorder WebSocket without Authorization returns `401`.
5. Recorder WebSocket with wrong Bearer token returns `401`.
6. Recorder WebSocket with correct token and device ID connects.
7. Token for robot A cannot connect as robot B.
8. Deleted or non-active robot cannot authenticate.

Out of scope for this implementation:

- Axon transfer WebSocket token authentication.
- Token query parameters, `X-API-Key`, or `Sec-WebSocket-Protocol`.
- Token rotation API.
- Register endpoint authentication.

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
  "robot_id": "9",
  "ws_client_auth_token": "kws_v1_example",
  "callback_allowlist": {
    "allowed_host": "192.168.1.20:9999",
    "allowed_path_prefix": "/api/v1/callbacks/"
  }
}
```
