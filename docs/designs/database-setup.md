# Database Setup Design

## Overview

Keystone Edge uses MySQL as its primary database, managed through an embedded migration system. The schema is version-controlled and automatically applied on service startup, eliminating any dependency on external initialization scripts.

---

## Migration Strategy

Migrations are managed by [`golang-migrate`](https://github.com/golang-migrate/migrate) and embedded directly into the binary via Go's `embed.FS`. On startup, the service checks the `schema_migrations` table and applies any pending migrations automatically.

This approach ensures:
- **Portability**: No external SQL files or Docker volume mounts required.
- **Idempotency**: Already-applied migrations are skipped safely.
- **Auditability**: Every schema change is a versioned, reversible file.

---

## Schema Design

The schema is organized into five logical domains.

### Environmental Hierarchy

Represents the physical and logical deployment structure.

```
organizations → factories → scenes → subscenes
```

- **`organizations`**: Top-level tenant. Identified by a unique `slug`.
- **`factories`**: Belong to an organization. Store location and timezone.
- **`scenes`**: Belong to a factory. Hold a layout template for robot positioning.
- **`subscenes`**: Belong to a scene. Bound to a specific `robot_type`.

### Capabilities & Procedures

Defines what robots can do and how tasks are structured.

- **`skills`**: Atomic robot actions (e.g., `pick`, `place`, `wipe`). Versioned.
- **`subscene_skills`**: Many-to-many mapping of skills to subscenes.
- **`sops`**: Standard Operating Procedures — an ordered sequence of skills stored as a JSON array.

### Operational Resources

The physical entities involved in a data collection session.

- **`robot_types`**: Robot model definitions, including ROS topics and capabilities (JSON).
- **`robots`**: Individual robot instances, identified by `device_id`.
- **`data_collectors`**: Human operators who run collection sessions.
- **`workstations`**: A pairing of a robot and a data collector at a factory. Contains denormalized fields (`robot_name`, `collector_name`, etc.) to avoid joins on hot query paths.
- **`inspectors`**: QA reviewers with certification levels (`level_1`, `level_2`, `senior`).

### Production Units

The lifecycle of a data collection job.

```
orders → batches → tasks → episodes → operations
```

- **`orders`**: A collection request with a target count and priority (`low / normal / high / urgent`).
- **`batches`**: A group of tasks assigned to a workstation.
- **`tasks`**: A single execution unit. Uses an integer `version` field for optimistic locking to prevent concurrent update conflicts.
- **`episodes`**: The recorded output of a task — an MCAP file with its sidecar. Tracks the full QA lifecycle and cloud sync state.
- **`operations`**: The individual skill steps within a task, ordered by `sequence_order`.

#### Episode QA States

```
pending_qa → qa_running → approved
                       → needs_inspection → inspector_approved
                                         → rejected
                       → failed
```

### Audit & Monitoring

Append-only tables for observability.

- **`state_transitions`**: Records every state change for tasks, episodes, orders, etc., along with the trigger source.
- **`api_logs`**: HTTP request logs with response time and status code.
- **`sync_logs`**: Tracks file sync operations to cloud storage.
- **`qa_checks`**: Individual QA check results with weighted scores (`DECIMAL(4,3)`).
- **`inspections`**: Human review decisions with reasons and failed tags.

---

## Key Design Decisions

### Denormalization for Query Performance

Several tables (`workstations`, `tasks`, `episodes`) carry denormalized copies of foreign-key data (e.g., `factory_id`, `organization_id`, `scene_name`). This avoids multi-table joins on the most frequent read paths — task listing and episode filtering by factory or organization.

### Soft Deletes

All business entities include a `deleted_at TIMESTAMP NULL` column with an index. Queries filter on `deleted_at IS NULL` rather than physically removing rows, preserving referential integrity and audit history.

### Optimistic Locking on Tasks

The `tasks` table includes a `version INT` column. Updates must include a `WHERE version = ?` clause and increment the version, preventing lost updates under concurrent access.

### Embedded Migrations

Migration SQL files live in `internal/storage/database/migrations/` and are compiled into the binary. There is no runtime dependency on the filesystem or Docker volumes for schema management.

---

## Connection Pool Configuration

| Parameter | Environment Variable | Default |
|-----------|----------------------|---------|
| Max open connections | `KEYSTONE_DB_MAX_OPEN_CONNS` | 25 |
| Max idle connections | `KEYSTONE_DB_MAX_IDLE_CONNS` | 5 |
| Connection max lifetime | `KEYSTONE_DB_CONN_MAX_LIFETIME` | 300s |

The DSN includes `multiStatements=true` to allow migration files to contain multiple SQL statements.
