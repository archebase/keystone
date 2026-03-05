# Fleet Manager Design

**Date:** 2026-03-04
**Status:** Design
**Target Version:** 0.1.0

## Overview

Fleet Manager is a Keystone Edge component that manages edge device data uploads. It communicates with [axon_transfer](https://github.com/ArcheBase/axon/tree/main/apps/axon_transfer) (a C++ upload daemon running on edge devices) via WebSocket to enable task scheduling, upload progress monitoring, and device state management.

This design is implemented in Go using `nhooyr.io/websocket`, based on the [axon fleet_manager_example/server.py](https://github.com/ArcheBase/axon/tree/main/apps/axon_transfer/scripts/fleet_manager_example/server.py).

## Architecture

```
┌─────────────────────────────────────────────────────────────────────┐
│                        Keystone Edge                                │
│                                                                     │
│  ┌─────────────────┐    ┌─────────────────┐    ┌───────────────┐    │
│  │  REST API       │    │  WebSocket Hub  │    │  Recorder RPC │    │
│  │  (Gin)          │    │  (nhooyr.io)    │    │  Forwarder    │    │
│  └────────┬────────┘    └────────┬────────┘    └───────┬───────┘    │
│           │                      │                    │             │
│           ▼                      ▼                    ▼             │
│  ┌─────────────────────────────────────────────────────────────┐    │
│  │                    FleetManager Service                     │    │
│  │  - Device Connection State (in-memory)                      │    │
│  │  - Event History (ring buffer)                              │    │
│  │  - Upload ACK Coordinator                                   │    │
│  └─────────────────────────────────────────────────────────────┘    │
│                              │                                      │
│                              ▼                                      │
│  ┌─────────────────────────────────────────────────────────────┐    │
│  │                    MySQL Database                           │    │
│  │  - episodes table (updated on upload_complete + ACK)        │    │
│  └─────────────────────────────────────────────────────────────┘    │
│                              │                                      │
│                              ▼                                      │
│  ┌─────────────────────────────────────────────────────────────┐    │
│  │                    S3 / MinIO                               │    │
│  │  - Verify uploaded files before ACK                         │    │
│  └─────────────────────────────────────────────────────────────┘    │
└─────────────────────────────────────────────────────────────────────┘
            │                                   │
            │ WebSocket                         │ HTTP
            ▼                                    ▼
┌─────────────────────┐              ┌─────────────────────┐
│ axon_transfer (C++) │              │ axon_recorder (C++)│
│ - WsClient          │              │ - RPC Server (8080)│
└─────────────────────┘              └─────────────────────┘
```

## Configuration

### FleetManagerConfig

```go
type FleetManagerConfig struct {
    WebSocketPort      string        // WebSocket listen port (default: "8090")
    MaxEventsPerDevice int           // Max events per device in ring buffer (default: 500)
    ReadTimeout        time.Duration // WebSocket read timeout (default: 60s)
    RecorderRPCPort    int           // Recorder RPC port (default: 8080)
}
```

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `KEYSTONE_FLEET_WS_PORT` | `8090` | WebSocket listen port |
| `KEYSTONE_FLEET_MAX_EVENTS` | `500` | Max events stored per device |
| `KEYSTONE_FLEET_READ_TIMEOUT` | `60` | WebSocket read timeout (seconds) |
| `KEYSTONE_FLEET_RECORDER_RPC_PORT` | `8080` | Fixed recorder RPC port |

## Data Models

### Connection State (In-Memory Only)

All device connection state is maintained **in-memory only**, not persisted to database.

```go
type DeviceConn struct {
    conn        *websocket.Conn
    deviceID    string
    remoteIP    string        // extracted from TCP connection
    connectedAt time.Time
    lastSeenAt  time.Time
    events      *ringbuffer.RingBuffer
    writeMu     sync.Mutex
}

type TransferHub struct {
    connections map[string]*DeviceConn
    mu          sync.RWMutex
}
```

### Recorder RPC URL

The Recorder RPC URL is automatically constructed:

- **IP**: Extracted from WebSocket TCP connection (`c.Request.RemoteAddr`)
- **Port**: Configured via `KEYSTONE_FLEET_RECORDER_RPC_PORT` (default: 8080)
- **Format**: `http://{remote_ip}:{port}`

Benefits:
1. No device-side configuration required
2. No SSRF risk (IP from TCP connection, not user-supplied)
3. Compatible with axon_transfer protocol

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

Fleet Manager uses **Verified ACK**: verify S3 files exist, update database, then send ACK.

```
axon_transfer          Fleet Manager          S3/MinIO       MySQL
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

### Implementation

```go
func (fm *FleetManager) handleUploadComplete(deviceID string, payload map[string]interface{}) {
    taskID := payload["data"]["task_id"].(string)
    
    // Step 1: Verify S3 files exist
    deviceConn := fm.hub.Get(deviceID)
    mcapKey := fm.buildS3Key(deviceConn.remoteIP, taskID, ".mcap")
    jsonKey := fm.buildS3Key(deviceConn.remoteIP, taskID, ".json")
    
    mcapExists, _ := fm.s3Client.HeadObject(mcapKey)
    jsonExists, _ := fm.s3Client.HeadObject(jsonKey)
    
    if !mcapExists || !jsonExists {
        log.Warnf("upload_complete for %s but S3 files not found, skipping ACK", taskID)
        return
    }
    
    // Step 2: Update episode
    var episode models.Episode
    if err := fm.db.Where("task_id = ?", taskID).First(&episode).Error; err == nil {
        fm.db.Model(&episode).Updates(map[string]interface{}{
            "cloud_synced":       true,
            "cloud_synced_at":    time.Now(),
            "cloud_mcap_path":    mcapKey,
            "cloud_sidecar_path": jsonKey,
        })
    }
    
    // Step 3: Send ACK
    fm.hub.Send(deviceID, map[string]interface{}{
        "type":    "upload_ack",
        "task_id": taskID,
    })
}
```

## REST API

### WebSocket Upgrade

```
GET /transfer/{device_id}
Upgrade: websocket
Connection: upgrade
```

### Endpoints

#### List Connected Devices

```
GET /api/v1/transfer/devices
```

#### Get Device Events

```
GET /api/v1/transfer/{device_id}/events?limit=100
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

#### Manual ACK

```
POST /api/v1/transfer/{device_id}/upload_ack
{"task_id": "task_abc123"}
```

### Recorder RPC Forwarding

Forward to recorder via HTTP:
- URL: `http://{deviceConn.remoteIP}:{config.RecorderRPCPort}/rpc/{path}`

#### Endpoints

- `GET /api/v1/transfer/{device_id}/recorder/status`
- `POST /api/v1/transfer/{device_id}/recorder/config`
- `POST /api/v1/transfer/{device_id}/recorder/begin`
- `POST /api/v1/transfer/{device_id}/recorder/finish`
- `POST /api/v1/transfer/{device_id}/recorder/pause`
- `POST /api/v1/transfer/{device_id}/recorder/resume`

## Implementation

### WebSocket Handler

```go
func (s *Server) handleTransferWS(c *gin.Context) {
    deviceID := c.Param("device_id")
    
    // Validate device exists
    var robot models.Robot
    if err := s.db.Where("device_id = ?", deviceID).First(&robot).Error; err != nil {
        c.JSON(404, gin.H{"error": "robot not found"})
        return
    }
    
    conn, err := websocket.Accept(c.Writer, c.Request, nil)
    if err != nil {
        return
    }
    defer conn.Close(websocket.StatusNormalClosure, "")
    
    // Extract client IP
    remoteIP := extractIP(c.Request.RemoteAddr)
    
    // Create DeviceConn (in-memory only)
    deviceConn := &DeviceConn{
        conn:        conn,
        deviceID:    deviceID,
        remoteIP:    remoteIP,
        connectedAt: time.Now(),
        lastSeenAt:  time.Now(),
        events:      ringbuffer.New(s.config.Fleet.MaxEventsPerDevice),
    }
    
    // Register in Hub
    s.hub.Connect(deviceID, deviceConn)
    defer s.hub.Disconnect(deviceID)
    
    // Read loop with timeout
    ctx := conn.CloseRead(context.Background())
    for {
        ctx, cancel := context.WithTimeout(ctx, s.config.Fleet.ReadTimeout)
        _, message, err := conn.Read(ctx)
        cancel()
        if err != nil {
            break
        }
        
        var msg map[string]interface{}
        json.Unmarshal(message, &msg)
        
        deviceConn.lastSeenAt = time.Now()
        s.hub.RecordEvent(deviceID, "inbound", msg)
        s.handleMessage(deviceID, msg)
    }
}
```

## Dependencies

```go
require (
    nhooyr.io/websocket v0.3.0
)
```

## Security Considerations

1. **Authentication**: Token-based auth (future enhancement)
2. **TLS**: Use WSS in production
3. **Rate Limiting**: Per-device rate limiting
4. **Input Validation**: Validate all JSON payloads
5. **Recorder RPC**: IP from TCP connection, port from config — no SSRF risk

## Reference

- [axon_transfer](https://github.com/ArcheBase/axon/tree/main/apps/axon_transfer)
- [axon-transfer-design.md](https://github.com/ArcheBase/axon/blob/main/docs/designs/axon-transfer-design.md)
