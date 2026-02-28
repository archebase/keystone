# Keystone Roadmap

This document outlines the development roadmap for the Edge Keystone project.

**Edge Keystone** is deployed per factory (`https://keystone.factory.internal`) and used by **Synapse**: robot/data collector management, task orchestration, local storage (MinIO), Axon integration, edge QA. Typically Go-only.

---

## Version 0.1.0 (In Planning)

First Release Implementation Order:

1. **Code standards, testing & CI** 
  - coding style(`gofmt` / `goimports`)
  - linters(golangci-lint)
  - unit/integration tests
    - Unit tests for core logic (handlers, services, models);
    - Integration tests for API endpoints;
  - Test coverage goal
  - CI pipeline: format check, lint, unit tests, integration tests (with DB)
  - Docs: CONTRIBUTING.md, ARCHITECTURE.md, detailed design docs
2. **Database Migration (Initial Schema)** 
  - create initial tables and SQL migration files
  - Auto-run pending migrations on server start
3. **Role-Based Access Control** 
  - System, Data Collector, Production Manager for now
  - JWT claim `role` validated on every protected route
4. **Workstation Management**
  - robots CRUD
  - workstations pairing
5. **Task Scheduler** 
  - task lifecycle management and config for Axon
6. **Callback Endpoints & Episode Management**
  - receive Axon Recorder notifications and record episodes
  - handle `/callbacks/start`、`/callbacks/finish`
  - episodes GET、PATCH
7. **WebSocket: Axon Uploader ↔ Keystone** 
  - long-lived connection for real-time upload control and status

---

## Version 0.2.0 (Next)

Second Release Implementation Order:
1. **add new role Data Inspector**
  - approve or reject episodes
2. **Complete the remaining APIs.** 
  - Organization and factory CRUD
  - Scene & Subscene CRUD
  - Skill & sop CRUD
  - Batch & order CRUD
3. **API rate limiting and webhooks**
4. **MCAP validator**
  - QA job triggering(dagster agent)(python)
  - basic QA checks(topics, duration, gaps, image integrity)(python)
  - awaiting inspection queue for auto-check failed episodes
  - Inspection API
5. **cloud sync**
  - Sync worker: push approved episodes to cloud S3
  - Edge-to-Cloud Sync API
6. **audit logging**

---

## Version 0.3.0

Third Release — Observability, Resilience & Production Readiness:

1. **Quality Dashboard**
   - Aggregate QA metrics per factory, scene, and workstation
   - Episode pass/fail rates, auto-approval rates, inspection queue depth
2. **Production Dashboard**
   - Real-time collection metrics: tasks/hour, upload success rate, active workstations
   - Episode throughput visualization
3. **Sync Resilience**
   - Persistent sync queue with exponential backoff retry (max 30s interval)
   - Idempotency guarantee: return `409 Conflict` with `synced_at` for duplicate `episode_id`
   - Sync queue depth monitoring and alerting
   - Consistency window tracking (`approved_at` → `synced_at` latency)
4. **Monitoring & Alerting**
   - Structured metrics export (Prometheus-compatible)
   - Key metrics: upload queue depth, QA failure rate, sync queue depth, episode throughput
   - Alert thresholds:
     - Upload queue > 5 files → Warning
     - QA failure rate > 10% → Warning
     - No uploads in 30 min → Critical
     - Sync queue > 100 episodes → Warning
5. **Multi-Factory Isolation**
   - Per-factory storage prefix enforcement (`s3://edge-{factory_id}/`)
   - Factory-scoped JWT claims and access control
   - Cross-factory data isolation validation
6. **Performance Optimization**
   - Throughput target: support 20–200 robots per factory (up to 24 TB/hour ingestion)
   - Upload latency p95 < 5 minutes
   - Database query optimization for high-volume episode indexing

---

## Contribution Priorities

We welcome contributions. Priority areas:

1. **0.1.0 scope**: Code standards & CI → Database Migration → Role Management → Workstation Management → Task Management → Callback Endpoints & Episode Management → Uploader↔Keystone
2. **Tests**: Unit and integration tests for API and callbacks
3. **Documentation**: API examples and deployment guide
4. **Bug fixes**: Any bug reports will be addressed promptly

See CONTRIBUTING.md (if present) for guidelines.
