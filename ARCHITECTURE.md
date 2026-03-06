# Keystone Architecture

## Overview

**Keystone** is the shared backend for the ArcheBase ecosystem. It serves as the single source of truth for core business logic, state machines, and data models. The same codebase is deployed in two distinct environments:

| Deployment | Stack | Role |
|------------|-------|------|
| **Edge** (per factory) | Go + minimal Python | Data collection control plane, lightweight QA |
| **Cloud** (centralized) | Go + full Python services | Full data processing pipeline, annotation, research |

This document focuses on the **Edge deployment** of Keystone. 

---

## Edge Deployment Architecture

### Component Overview

```
┌────────────────────────────────────────────────────────────────────┐
│                    EDGE LAYER (Per Factory)                        │
│                                                                    │
│  ┌─────────────┐  ┌─────────────────────────────┐  ┌───────────┐   │
│  │  Synapse    │  │  Keystone (Go + Gin)        │  │  MinIO    │   │
│  │  UI         │  │                             │  │  Storage  │   │
│  │  • Tasks    │──│  • Scene Registry           │  │  • MCAP   │   │
│  │  • QA       │  │  • Order Manager            │  │  • JSON   │   │
│  │  • Inspect  │  │  • Task Scheduler           │  │           │   │
│  └─────────────┘  │  • Robot Fleet              │  └───────────┘   │
│                   │  • Episode Registry         │        │         │
│  ┌─────────────┐  │  • Quality Dashboard        │        │         │
│  │  Axon       │  │                             │        │         │
│  │             │──│  MySQL (source of truth)    │        │         │
│  │  (Robot HW) │  └─────────────────────────────┘        │         │
│  └─────────────┘                │                        │         │
│                                 │                        │         │
│  ┌─────────────────────────────────────────────────────┐ │         │
│  │  Dagster Agent (Python - minimal)                   │─┘         │
│  │  • QA job triggering                                │           │
│  │  • MCAP validation (topics, duration, gaps, images) │           │
│  └─────────────────────────────────────────────────────┘           │
│                                                                    │
└────────────────────────────────────────────────────────────────────┘
                              │
                    ══════════╪══════════
                     Sync Protocol
                    (Approved episodes only)
                    ══════════╪══════════
                              │
                              ▼
                         Cloud S3
```

### Go Components (Edge + Cloud Shared)

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
┌──────────────┐    ┌──────────────┐    ┌──────────────┐    ┌──────────────┐
│   Keystone   │    │    Axon      │    │    Edge      │    │    MinIO     │
│   API        │───►│   Recorder   │───►│   Uploader   │───►│   Storage    │
│   (Go/Gin)   │    │              │    │              │    │              │
│  Task Config │    │  MCAP Write  │    │  MinIO       │    │  .mcap       │
│  + Callbacks │    │  + Metadata  │    │  Upload      │    │  .json       │
└──────────────┘    └──────────────┘    └──────────────┘    └──────────────┘
```

**Step-by-step**:

1. Data collector requests a task from Keystone API
2. Keystone returns task configuration
3. Axon starts recording, calls `POST /callbacks/start`
4. Keystone updates task status → `in_progress`
5. Axon records MCAP file, uploads MCAP + sidecar JSON to MinIO
6. Axon calls `POST /callbacks/finish`
7. Keystone updates task status → `completed`, creates episode with status `pending_qa`

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

Only **approved episodes** (QA score ≥ 90% or inspector-approved) are synced to cloud:

```
Edge MinIO ══(push)══► Cloud S3
```

Sync is handled via `POST /api/v1/sync/episode` with:
- Sidecar JSON (metadata)
- MCAP file (raw sensor data)
- SHA-256 checksums for integrity validation

---

## Edge Storage Layout

```
s3://edge-{factory_id}/
└── batches/
    └── {batch_id}/
        └── tasks/
            └── {task_id}/
                ├── data.mcap       ← Raw sensor data (ROS topics, 2–8 GB)
                └── sidecar.json    ← Quick-parse metadata for filtering
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

1. Edge sync worker resumes
2. Queued episodes uploaded (`POST /api/v1/sync/episode`)
3. Cloud processes each asynchronously (returns `202 Accepted`)
4. Cloud DB becomes eventually consistent with edge

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
