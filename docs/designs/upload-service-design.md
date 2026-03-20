<!--
SPDX-FileCopyrightText: 2026 ArcheBase

SPDX-License-Identifier: MulanPSL-2.0
-->

# Upload Service Design

## Overview

Upload Service is a Keystone Edge component that manages edge device data uploads. It communicates with [axon_transfer](https://github.com/ArcheBase/axon/tree/main/apps/axon_transfer) (a C++ upload daemon running on edge devices) via WebSocket to enable **upload lifestyle management** and **device connection state tracking**.

## Architecture

```
┌─────────────────────────────────────────────────────────────────────┐
│                        Keystone Edge                                │
│                                                                     │
│  ┌─────────────────┐    ┌─────────────────┐                         |
│  │  REST API       │    │  WebSocket Hub  │                         |
│  │  (Gin)          │    │                 │                         |
│  └────────┬────────┘    └────────┬────────┘                         │
│           │                      │                                  │
│           ▼                      ▼                                  │
│  ┌─────────────────────────────────────────────────────────────┐    │
│  │                    Upload Service                           │    │
│  │  - Device Connection State                                  │    │
│  │  - upload lifestyle management                              │    │
│  └─────────────────────────────────────────────────────────────┘    │
│            │                                  │                     |
│            ▼                                  ▼                     │
│  ┌─────────────────────┐          ┌─────────────────────┐           │
│  │   MySQL Database    │          │     S3 / MinIO      │           │
│  │ - episodes table    │          │ - File storage      │           │
│  │   (metadata)        │          │ - Verify files      │           │
│  └─────────────────────┘          └─────────────────────┘           │
└─────────────────────────────────────────────────────────────────────┘
                            ▲
                            │
                            │ WebSocket
                            ▼
                  ┌─────────────────────┐
                  │ axon_transfer (C++) │
                  │ - WsClient          │
                  └─────────────────────┘
```

## Configuration

### TransferConfig

```go
type TransferConfig struct {
    WebSocketPort      string        // WebSocket listen port (default: "8090")
    MaxEventsPerDevice int           // Max events per device in ring buffer (default: 500)
    ReadTimeout        time.Duration // WebSocket read timeout (default: 60s)
}
```

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `KEYSTONE_AXON_TRANSFER_WS_PORT` | `8090` | WebSocket listen port |
| `KEYSTONE_AXON_TRANSFER_MAX_EVENTS` | `500` | Max events stored per device |
| `KEYSTONE_AXON_TRANSFER_READ_TIMEOUT` | `60` | WebSocket read timeout (seconds) |

## Data Models

### Connection State

All device connection state is maintained **in-memory only**, not persisted to database.

```go
type TransferConn struct {
    conn        *websocket.Conn
    deviceID    string
    remoteIP    string
    connectedAt time.Time
    lastSeenAt  time.Time
    events      *ringbuffer.RingBuffer
    writeMu     sync.Mutex
}

type TransferHub struct {
    connections map[string]*TransferConn
    mu          sync.RWMutex
}
```

## WebSocket Protocol

### Connection Endpoint

```
ws://{host}:{port}/transfer/{device_id}
```

### Messages: Server → Device

#### upload_request

```json
{
  "type": "upload_request",
  "task_id": "task_abc123",
  "priority": 1
}
```

#### upload_all

```json
{
  "type": "upload_all"
}
```

#### cancel

```json
{
  "type": "cancel",
  "task_id": "task_abc123"
}
```

#### status_query

```json
{
  "type": "status_query"
}
```

#### upload_ack

```json
{
  "type": "upload_ack",
  "task_id": "task_abc123"
}
```

### Messages: Device → Server

#### connected

```json
{
  "type": "connected",
  "timestamp": "2026-03-04T01:00:00.000Z",
  "data": {
    "version": "0.1.0",
    "device_id": "ROBOT-001",
    "pending_count": 3,
    "uploading_count": 0,
    "waiting_ack_count": 1,
    "failed_count": 1
  }
}
```

#### upload_started

```json
{
  "type": "upload_started",
  "timestamp": "2026-03-04T01:00:05.000Z",
  "data": {
    "task_id": "task_abc123",
    "files": ["task_abc123.mcap", "task_abc123.json"],
    "total_bytes": 4294967296
  }
}
```

#### upload_progress

```json
{
  "type": "upload_progress",
  "timestamp": "2026-03-04T01:00:10.000Z",
  "data": {
    "task_id": "task_abc123",
    "bytes_uploaded": 1073741824,
    "total_bytes": 4294967296,
    "percent": 25
  }
}
```

#### upload_complete

```json
{
  "type": "upload_complete",
  "timestamp": "2026-03-04T01:05:00.000Z",
  "data": {
    "task_id": "task_abc123",
    "bytes_uploaded": 4294967296,
    "duration_ms": 295000
  }
}
```

#### upload_failed

```json
{
  "type": "upload_failed",
  "timestamp": "2026-03-04T01:05:00.000Z",
  "data": {
    "task_id": "task_abc123",
    "reason": "S3 connection refused after 5 retries",
    "retry_count": 5
  }
}
```

#### upload_not_found

```json
{
  "type": "upload_not_found",
  "timestamp": "2026-02-28T10:00:00.100Z",
  "data": {
    "task_id": "task_abc123",
    "detail": "No MCAP file matching task_abc123 in /data/recordings"
  }
}
```

#### status

```json
{
  "type": "status",
  "timestamp": "2026-03-04T01:00:00.200Z",
  "data": {
    "pending_count": 3,
    "uploading_count": 1,
    "waiting_ack_count": 2,
    "waiting_ack_task_ids": ["task_abc123", "task_def456"],
    "completed_count": 42,
    "failed_count": 1,
    "pending_bytes": 12884901888,
    "bytes_per_sec": 8912896
  }
}
```

## Verified ACK Design

### Verified ACK Flow

Transfer server uses **Verified ACK**: verify S3 files exist, update database, then send ACK.

```
axon_transfer          Transfer Server          S3/MinIO       MySQL
    │                        │                    │              │
    │ upload_complete        │                    │              │
    │─────────────────────►  │                    │              │
    │                        │                    │              │
    │                        │ HeadObject         │              │
    │                        │─────────────────►  │              │
    │                        │◄─────────────────  │              │
    │                        │                    │              │
    │                        │ UPDATE episodes    │              │
    │                        │───────────────────────────────►   │
    │                        │                    │              │
    │   upload_ack           │                    │              │
    │◄─────────────────────  │                    │              │
```

## REST API

### Endpoints

#### List Connected Devices

```
GET /api/v1/transfer/devices
```

#### Request Upload

```
POST /api/v1/transfer/{device_id}/upload_request
{"task_id": "task_abc123", "priority": 1}
```

#### Upload All Pending

```
POST /api/v1/transfer/{device_id}/upload_all
```

#### Cancel Upload

```
POST /api/v1/transfer/{device_id}/cancel
{"task_id": "task_abc123"}
```

#### Query Status

```
POST /api/v1/transfer/{device_id}/status_query
```

#### Manual ACK

```
POST /api/v1/transfer/{device_id}/upload_ack
{"task_id": "task_abc123"}
```

## Security Considerations

1. **Authentication**: Token-based auth (future enhancement)
2. **TLS**: Use WSS in production
3. **Rate Limiting**: Per-device rate limiting
4. **Input Validation**: Validate all JSON payloads
