<!--
SPDX-FileCopyrightText: 2026 ArcheBase

SPDX-License-Identifier: MulanPSL-2.0
-->

# Keystone Architecture

## Overview

**Keystone** is the edge backend for the ArcheBase ecosystem. It serves as the single source of truth for core business logic, state machines, and data models at the edge level. It is deployed per factory to manage data collection, QA validation, and sync to cloud.

---

## Edge Deployment Architecture

### Component Overview

```
┌──────────────────────────────────────────────────────────────────────────────┐
│                         EDGE LAYER (Per Factory)                             │
│                                                                              │
│  Synapse Clients                    Keystone Core                            │
│  ┌─────────────┐                    ┌───────────────────────────────────┐    │
│  │ Synapse #1  │───────┐            │ Keystone (Go + Gin)               │    │
│  │   (UI)      │       │            │                                   │    │
│  └─────────────┘       │            │ API + Control Plane               │    │
│                        ├───────────►│ • Scene Registry                  │    │
│  ┌─────────────┐       │            │ • Order Manager                   │    │
│  │ Synapse #2  │───────┤            │ • Task Scheduler                  │    │
│  │   (UI)      │       │            │ • Robot Fleet                     │    │
│  └─────────────┘       │            │ • Episode Registry                │    │
│                        │            │ • Quality Dashboard               │    │
│  ┌─────────────┐       │            │                                   │    │
│  │    ...      │───────┤            │ Managed Components                │    │
│  └─────────────┘       │            │ • MySQL (source of truth)         │    │
│                        │            │ • MinIO (MCAP + sidecar JSON)     │    │
│  ┌─────────────┐       │            └──────────────────┬────────────────┘    │
│  │ Synapse #N  │───────┘                               │                     │
│  │   (UI)      │                                       │                     │
│  └─────────────┘                                       │                     │
│                                                        │                     │
│  Axon                                                  │      QA             │
│  ┌─────────────┐                                       │   ┌──────────────┐  │
│  │  Axon #1    │───────┐                               ├──►│ Dagster Agent│  │
│  │ (Robot #1)  │       │                               │   │  (Python)    │  │
│  └─────────────┘       │                               │   │ • Trigger QA │  │
│                        ├───────────────────────────────┘   │ • Validate   │  │
│  ┌─────────────┐       │                                   │   MCAP       │  │
│  │  Axon #2    │───────┤                                   └──────────────┘  │
│  │ (Robot #2)  │       │                                                     │
│  └─────────────┘       │                                                     │
│                        │                                                     │
│  ┌─────────────┐       │                                                     │
│  │    ...      │───────┤                                                     │
│  └─────────────┘       │                                                     │
│                        │                                                     │
│  ┌─────────────┐       │                                                     │
│  │  Axon #N    │───────┘                                                     │
│  │ (Robot #N)  │                                                             │
│  └─────────────┘                                                             │
└──────────────────────────────────────────────────┬───────────────────────────┘
                                                   │
                                         ══════════╪══════════
                                          Sync Protocol
                                         (Approved episodes only)
                                         ══════════╪══════════
                                                   │
                                                   ▼
                                              Cloud S3
```

### Go Components

| Component | Responsibility |
|-----------|----------------|
| **Scene Registry** | CRUD for Scenes, Subscenes, Skills, Operations, SOPs |
| **Order Manager** | Create orders, generate tasks |
| **Task Scheduler** | Assign tasks to workstations, track progress |
| **Robot Fleet** | Register robots, track availability |
| **Workstation Manager** | Manage Robot + DataCollector pairings |
| **Episode Registry** | Index uploaded episodes, track QA status |
| **Quality Dashboard** | Visualize collection progress, QA metrics |

### Python Components (Edge Only — Minimal Footprint)

| Component | Responsibility |
|-----------|----------------|
| **Dagster Agent** | QA job triggering, status reporting to Keystone API |
| **MCAP Validator** | Lightweight validation: required topics, duration, gap detection, image integrity |

> **Design Principle**: The edge Python footprint is intentionally minimal. No Ray, no Daft, no LanceDB. Heavy processing runs exclusively in the cloud.

---

## Technology Stack

### Go Layer

| Technology | Purpose |
|------------|---------|
| **Go** | Primary language for all API and business logic |
| **Gin** | REST API framework (high-performance, Express-like) |
| **MySQL** | Relational data storage (source of truth at edge) |
| **GORM** | ORM access |

### Python Layer (Edge — Minimal)

| Technology | Purpose |
|------------|---------|
| **Dagster Agent** | QA pipeline job triggering |
| **MCAP library** | Lightweight file validation (no distributed compute) |

### Storage

| Technology | Purpose |
|------------|---------|
| **MinIO** | S3-compatible on-premise object storage for MCAP and sidecar JSON |

---

## Data Flow

### Collection Pipeline

```
┌──────────────┐
│    Axon      │
│  (Recorder)  │
└──────┬───────┘
       │ 1. GET /tasks (request a task)
       ▼
┌──────────────┐
│   Keystone   │
│   API        │
│   (Go/Gin)   │
└──────┬───────┘
       │ 2. return task config
       ▼
┌──────────────┐
│    Axon      │
│  (Recorder)  │
└──────┬───────┘
       │ 3. POST /callbacks/start
       ▼
┌──────────────┐
│   Keystone   │
│   API        │
│              │
│  task status │
│ → in_progress│
└──────┬───────┘
       │ 4. (ack)
       ▼
┌──────────────┐
│    Axon      │
│  (Recorder)  │
│              │
│  MCAP Write  │
│ + Metadata   │
│  (Local)     │
└──────┬───────┘
       │ 5. record MCAP to local storage
       │    (recording in progress...)
       │ 6. POST /callbacks/finish
       ▼
┌──────────────┐
│   Keystone   │
│   API        │
│              │
│              │
│ add to upload│
│   queue      │
└──────┬───────┘
       │ 7. trigger upload (cmd via websocket)
       ▼
┌──────────────┐      8. upload MCAP + sidecar JSON      ┌──────────────┐
│    Axon      │ ──────────────────────────────────────► │    MinIO     │
│  (Uploader)  │                                         │   Storage    │
│              │                                         │              │
│    upload    │                                         │ .mcap        │
│              │                                         │ .json        │
└──────┬───────┘                                         └──────────────┘
       │ 9. websocket msg: upload_complete
       ▼
┌──────────────┐
│   Keystone   │
│   API        │
│              │
│ episode      │
│ status →     │
│ pending_qa   │
└──────────────┘
```

**Step-by-step**:

1. Data collector requests a task from Keystone API
2. Keystone returns task configuration
3. Axon starts recording, calls `POST /callbacks/start`
4. Keystone updates task status → `in_progress`
5. Axon records MCAP file to local storage
6. Axon calls `POST /callbacks/finish` to notify Keystone recording is complete
7. Keystone triggers upload according to its strategy
8. Axon uploads MCAP + sidecar JSON to MinIO
9. Axon notify keystone upload is done by websocket
10. Keystone creates episode status → `pending_qa`

### QA Pipeline

```
MinIO ──► Dagster Agent ──► MCAP Validator ──► Keystone API ──► Decision
```

**QA Checks**:

| Check | Description |
|-------|-------------|
| **Required Topics** | All expected ROS topics present |
| **Duration** | Recording meets minimum duration |
| **No Large Gaps** | No significant data gaps in timeline |
| **Image Integrity** | Camera frames are valid and decodable |

**QA Decision Flow**:

```
New Recording Complete
        │
        ▼
  Run QA Checks
  (topics, duration, gaps, images)
        │
        ▼
  Calculate Score (0.0 – 1.0)
        │
   ┌────┴────┐
   │         │
Score ≥ 0.90  Score < 0.90
   │         │
   ▼         ▼
Auto-Approve  Route to Inspector
(approved)   (needs_inspection)
   │              │
   │         ┌────┴────┐
   │    Approve       Reject
   │         │         │
   │    inspector_  rejected
   │    approved
   │         │
   └────┬────┘
        ▼
  Ready for Cloud Sync
```

### Sync to Cloud

Only **approved episodes** (QA score >= 90% or inspector-approved) are eligible for cloud sync:

```
Edge MinIO ══(push)══► Cloud S3
```

Keystone should default to manual cloud sync scheduling. `KEYSTONE_SYNC_ENABLED`
controls whether cloud sync capability and the worker are available.
`KEYSTONE_SYNC_AUTO_SCAN_ENABLED` controls whether the worker periodically
discovers newly eligible approved unsynced episodes, and its default is
`false`. With the default setting, recorded data remains in edge MinIO until an
admin manually syncs one episode or explicitly scans and queues eligible
episodes.

Sync upload is executed by the edge `SyncWorker` through the cloud data gateway
with:
- Sidecar JSON metadata converted to raw tags
- MCAP file (raw sensor data)
- SHA-256 checksums for integrity validation

---

## Edge Storage Layout

### MinIO Storage
```

s3://edge-{factory_id}/
└── {factory_id}/
    └── {device_id}/
        └── {date}/
            ├── {task_id}.mcap       ← Raw sensor data (ROS topics, 2–8 GB)
            └── {task_id}.json       ← Quick-parse metadata for filtering

```

---

## Database Schema (Core Tables)

The edge MySQL database is the **source of truth** for all operational data.

```
organizations
    └── factories
            ├── scenes
            │       └── subscenes
            │               └── subscene_skills ──── skills
            ├── robots ──── robot_types
            └── workstations
                    ├── robots (assigned)
                    ├── data_collectors (assigned)
                    └── batches
                            ├── orders
                            └── tasks
                                    ├── sops
                                    └── episodes
                                            └── qa_checks
```

### Key Tables

| Table | Description |
|-------|-------------|
| `organizations` | Top-level tenant |
| `factories` | Physical factory locations |
| `scenes` | Operational zones within a factory |
| `subscenes` | Sub-zones with specific robot type requirements |
| `robots` | Registered robot fleet |
| `workstations` | Robot + DataCollector pairings |
| `orders` | Data collection orders with target episode counts |
| `batches` | Groups of tasks assigned to a workstation |
| `tasks` | Individual recording assignments |
| `episodes` | Completed recordings with QA status |
| `qa_checks` | Individual QA check results per episode |

### Episode QA Status Values

| Status | Description |
|--------|-------------|
| `pending_qa` | Uploaded, awaiting QA validation |
| `auto_approved` | QA score ≥ 90%, ready for sync |
| `needs_inspection` | QA score < 90%, routed to inspector |
| `inspector_approved` | Manually approved by inspector, ready for sync |
| `rejected` | Rejected by inspector, not synced |

---

## Distributed System Properties

### CAP Theorem

Keystone at edge is designed as an **AP system** (Availability + Partition Tolerance) with eventual consistency:

| Property | Choice | Rationale |
|----------|--------|-----------|
| **Consistency** | Eventual | Edge and cloud may temporarily differ during network partition |
| **Availability** | High | Edge remains fully operational during network outage |
| **Partition Tolerance** | High | Data collection continues when edge–cloud network fails |

### Data Ownership

The edge is the **source of truth** for all operational data:

```
EDGE (Source of Truth)          CLOUD (Eventually Consistent Replica)
──────────────────────          ──────────────────────────────────────
• Organizations                 • Organizations (metadata only)
• Factories                     • Episodes (after sync)
• Scenes / Subscenes            • Batches
• Robots / Workstations         • Orders / Tasks
• DataCollectors
• Tasks / Episodes
• Batches / Orders
```

**Data flow is strictly unidirectional**: Edge → Cloud. The cloud never sends control signals or data modifications back to the edge.

### Network Partition Behavior

**During outage**:

| Component | Edge | Cloud |
|-----------|------|-------|
| MySQL | ✓ Writable | ✓ Writable (cloud-only tables) |
| MinIO | ✓ Writable | — |
| Data Collection | ✓ Fully operational | — |
| Sync | ✗ Queued for retry | ✗ No new data from edge |

**After network restored**:

1. Edge sync worker resumes and drains persisted `pending` sync rows.
2. Already queued or retryable sync jobs continue according to retry policy.
3. If `KEYSTONE_SYNC_AUTO_SCAN_ENABLED=true`, newly eligible approved unsynced
   episodes are discovered and queued automatically.
4. If automatic discovery is disabled, new local episodes remain unsynced until
   an admin manually syncs one episode or explicitly scans eligible episodes.
5. Cloud processes each upload asynchronously and cloud DB becomes eventually
   consistent with edge.

### Sync Guarantees

| Guarantee | Mechanism |
|-----------|-----------|
| **No Data Loss** | Episodes stored in MinIO before sync; exponential backoff retry (max 30s) |
| **Exactly-Once Delivery** | Idempotent sync: cloud returns `409 Conflict` for duplicate `episode_id` |
| **Data Integrity** | SHA-256 checksum validation on MCAP and sidecar JSON |
| **Order Preservation** | `approved_at` timestamp maintains chronological order |
| **Monotonic Reads** | Once synced to cloud, data is never removed |

**Idempotency example**:

```http
POST /api/v1/sync/episode
Authorization: Bearer <token>
{
  "episode_id": "550e8400-e29b-41d4-a716-446655440000",
  "checksum": { "mcap": "a1b2c3d4e5f6..." }
}

# First attempt  → 202 Accepted
# Retry (same ID) → 409 Conflict { "synced_at": "2024-01-15T10:30:00Z" }
```

---

## Multi-Factory Support

Each factory runs an **independent** Keystone edge instance:

- Dedicated edge storage prefix: `s3://edge-{factory_id}/`
- Independent Go backend + MySQL
- Optional connection to cloud for sync
- No cross-factory data sharing at the edge

---

## Throughput Targets

| Metric | Target |
|--------|--------|
| Robots per factory | 20–200 |
| Episodes per workstation per hour | 30 |
| Episode size (p90) | ~4 GB |
| Upload latency (p95) | < 5 minutes |
| Peak data ingestion (small factory) | 2.4 TB/hour (20 robots) |
| Peak data ingestion (large factory) | 24 TB/hour (200 robots) |

---

## Security Model

| Channel | Mechanism |
|---------|-----------|
| Device → Edge | TLS 1.3, JWT authentication |
| Edge → Cloud | Server-side encryption, IAM policies |
| Storage access | Devices write only to `{factory_id}/{device_id}/{date}` prefix |
| Processing | VPC isolation, least-privilege access |

---

## Monitoring

### Key Metrics

| Layer | Metrics |
|-------|---------|
| **Collection** | Tasks/hour, upload success rate, queue depth |
| **QA** | QA pass rate, auto-approval rate, inspection queue depth |
| **Sync** | Sync queue depth, retry count, sync latency |

### Alerting Thresholds

| Condition | Severity | Action |
|-----------|----------|--------|
| Upload queue > 5 files | Warning | Investigate network or storage |
| QA failure rate > 10% | Warning | Review data quality |
| No uploads in 30 min | Critical | Check device health |
| Sync queue > 100 episodes | Warning | Check cloud connectivity |
