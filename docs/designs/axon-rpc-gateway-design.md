# Axon RPC Gateway Design

**Status:** Implemented | **Version:** 0.2.0

## Overview

Keystone receives connections from Axon Recorder via WebSocket and provides HTTP APIs for external clients to send RPC commands to the Recorder. The Recorder actively connects to Keystone and uses a persistent connection to receive commands, return execution results, and report status updates.

## Architecture

```
Client → HTTP API → RecorderHandler → RecorderHub → Axon WS Server → Recorder
                                                        ↓
                                              WebSocket Bidirectional Communication
```

- **HTTP API**: Receives external RPC requests
- **RecorderHandler**: Parses requests, validates parameters, transforms responses
- **RecorderHub**: Manages connections, sends commands, waits for responses
- **Axon WS Server**: WebSocket server, listening on port 8091

## Components

### 1. Configuration

`AxonRPCConfig` defined in [`internal/config/config.go`](internal/config/config.go:105):

| Field | Description | Default |
|-------|-------------|---------|
| WSPort | WebSocket listen port | 8091 |
| PingInterval | Heartbeat interval (seconds) | 30 |
| ResponseTimeout | RPC response timeout (seconds) | 15 |

### 2. RecorderHub

Defined in [`internal/services/recorder_hub.go`](internal/services/recorder_hub.go:96), manages all Recorder connections:

- `Connect/Disconnect`: Connection registration and cleanup
- `Get/ListDevices`: Query connection status
- `SendRPC`: Send commands and wait for responses
- `HandleResponse`: Match requests with responses

## HTTP API

Routes mounted under `/api/v1/recorder`:

| Method | Path | Description |
|--------|------|-------------|
| GET | `/devices` | List online devices |
| GET | `/:device_id/state` | Query device state |
| GET | `/:device_id/stats` | Query device statistics |
| POST | `/:device_id/rpc` | Generic RPC call |
| POST | `/:device_id/config` | Configure task |
| POST | `/:device_id/begin` | Start recording |
| POST | `/:device_id/finish` | Finish recording |
| POST | `/:device_id/pause` | Pause |
| POST | `/:device_id/resume` | Resume |
| POST | `/:device_id/cancel` | Cancel |
| POST | `/:device_id/clear` | Clear |
| POST | `/:device_id/quit` | Quit |

WebSocket path: `ws://host:8091/recorder/:device_id`

---

## Request and Response

### 1. Generic RPC Call

**POST** `/api/v1/recorder/:device_id/rpc`

Request body:
```json
{
  "action": "config",
  "params": {
    "task_config": { ... }
  }
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| action | string | Yes | RPC action |
| params | object | No | Action parameters |

Success response (200):
```json
{
  "type": "rpc_response",
  "request_id": "550e8400-e29b-41d4-a716-446655440000",
  "success": true,
  "message": "ok",
  "data": { ... }
}
```

### 2. Configure Task

**POST** `/api/v1/recorder/:device_id/config`

Request body:
```json
{
  "task_config": {
    "task_id": "task-001",
    "device_id": "robot-001",
    "data_collector_id": "collector-001",
    "order_id": "order-001",
    "operator_name": "alice",
    "scene": "warehouse_pickup",
    "subscene": "aisle_a",
    "skills": ["pick", "place"],
    "factory": "factory-shanghai",
    "topics": ["/imu/data", "/camera0/rgb"],
    "start_callback_url": "http://127.0.0.1:9999/api/v1/tasks/start",
    "finish_callback_url": "http://127.0.0.1:9999/api/v1/tasks/finish",
    "user_token": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9...",
    "started_at": "2026-03-13T10:00:00Z"
  }
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| task_config.task_id | string | Yes | Unique task identifier |
| task_config.device_id | string | No | Recorder device identifier |
| task_config.data_collector_id | string | No | Data collector identifier |
| task_config.order_id | string | No | Order/job identifier |
| task_config.operator_name | string | No | Operator name |
| task_config.scene | string | No | Recording scene label |
| task_config.subscene | string | No | Recording subscene label |
| task_config.skills | string[] | No | Skill tags |
| task_config.factory | string | No | Factory identifier |
| task_config.topics | string[] | No | Topics to record |
| task_config.start_callback_url | string | No | Recording start callback URL |
| task_config.finish_callback_url | string | No | Recording finish callback URL |
| task_config.user_token | string | No | User JWT token |
| task_config.started_at | string | No | Recording start time (ISO8601) |

### 3. Start Recording

**POST** `/api/v1/recorder/:device_id/begin`

Request body:
```json
{
  "task_id": "task-001"
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| task_id | string | Yes | Task identifier |

### 4. Finish Recording

**POST** `/api/v1/recorder/:device_id/finish`

Request body:
```json
{
  "task_id": "task-001"
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| task_id | string | Yes | Task identifier |

### 5. Pause/Resume/Cancel/Clear/Quit

**POST** `/api/v1/recorder/:device_id/pause`  
**POST** `/api/v1/recorder/:device_id/resume`  
**POST** `/api/v1/recorder/:device_id/cancel`  
**POST** `/api/v1/recorder/:device_id/clear`  
**POST** `/api/v1/recorder/:device_id/quit`

Request body: empty

### 6. Query Device State

**GET** `/api/v1/recorder/:device_id/state`

Success response (200):
```json
{
  "current_state": "recording",
  "task_id": "task-001",
  "updated_at": "2026-03-14T10:30:00Z",
  "raw": { ... }
}
```

| Field | Type | Description |
|-------|------|-------------|
| current_state | string | Current state (ready, recording, paused, finished) |
| task_id | string | Associated task ID |
| updated_at | time | Last update time |
| raw | object | Raw state data |

### 7. Query Device Statistics

**GET** `/api/v1/recorder/:device_id/stats`

Returns statistics obtained via RPC call.

### 8. List Online Devices

**GET** `/api/v1/recorder/devices`

Success response (200):
```json
{
  "devices": [
    {
      "device_id": "robot-001",
      "remote_ip": "192.168.1.100",
      "connected_at": "2026-03-14T10:00:00Z",
      "last_seen_at": "2026-03-14T10:30:00Z",
      "state": {
        "current_state": "recording",
        "task_id": "task-001",
        "updated_at": "2026-03-14T10:30:00Z"
      }
    }
  ]
}
```

| Field | Type | Description |
|-------|------|-------------|
| devices[].device_id | string | Device identifier |
| devices[].remote_ip | string | Remote IP |
| devices[].connected_at | time | Connection time |
| devices[].last_seen_at | time | Last active time |
| devices[].state | object | Current state snapshot |

### Error Response

| HTTP Status Code | Description |
|------------------|-------------|
| 400 | Invalid request parameters |
| 404 | Device not connected |
| 504 | RPC response timeout |
| 500 | Internal error |

Error response format:
```json
{
  "error": "recorder not connected"
}
```

## WebSocket Message Protocol

### Keystone → Recorder

**RPC Request:**
```json
{
  "type": "rpc_request",
  "request_id": "550e8400-e29b-41d4-a716-446655440000",
  "action": "begin",
  "params": {
    "task_id": "task-001"
  }
}
```

### Recorder → Keystone

**RPC Response:**
```json
{
  "type": "rpc_response",
  "request_id": "550e8400-e29b-41d4-a716-446655440000",
  "success": true,
  "message": "Recording started",
  "data": {
    "state": "recording",
    "task_id": "task-001"
  }
}
```

**State Update:**
```json
{
  "type": "state_update",
  "timestamp": "2026-03-14T10:30:00.000Z",
  "data": {
    "current_state": "recording",
    "previous_state": "ready",
    "task_id": "task-001"
  }
}
```

**Error Response:**
```json
{
  "type": "rpc_response",
  "request_id": "550e8400-e29b-41d4-a716-446655440000",
  "success": false,
  "message": "Invalid task config"
}
```

### Supported Actions

| Action | Description | Parameters |
|--------|-------------|------------|
| `config` | Configure task | `task_config` |
| `begin` | Start recording | `task_id` |
| `finish` | Finish recording | `task_id` |
| `pause` | Pause | - |
| `resume` | Resume | - |
| `cancel` | Cancel | - |
| `clear` | Clear | - |
| `quit` | Quit | - |
| `get_state` | Query state | - |
| `get_stats` | Query statistics | - |

## Relationship with Existing Components

Reuses Keystone's existing HTTP +独立 WebSocket + Hub connection pool pattern:

- Add `axon_rpc` configuration section in config layer
- Add `RecorderHub` in service layer (parallel to `TransferHub`)
- Add separate Axon WebSocket Server in service startup layer

## Risks

1. **Synchronous Wait Timeout**: HTTP requests synchronously wait for WebSocket responses, controlled by `ResponseTimeout`
2. **Connection Replacement**: When the same `device_id` reconnects, the new connection replaces the old one
3. **State Consistency**: `GET /state` returns an in-memory snapshot, not guaranteed to match the device's real-time state
