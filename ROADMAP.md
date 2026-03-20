<!--
SPDX-FileCopyrightText: 2026 ArcheBase

SPDX-License-Identifier: MulanPSL-2.0
-->

# Keystone Roadmap

This document outlines the development roadmap for the Edge Keystone project.

**Keystone** is the edge backend for the ArcheBase ecosystem. It serves as the single source of truth for core business logic, state machines, and data models at the edge level. It is deployed per factory to manage data collection, QA validation, and sync to cloud.

---

## Version 0.1.0 (Done)

First Release Implementation Order:

1. **Code standards, testing & CI** 
  - ✅coding style(`gofmt`)
  - ✅linters(`golangci-lint`)
  - ✅Unit tests for core logic (handlers, services, models);
  - ✅Integration tests for API endpoints;
  - ✅CI pipeline: format check, lint, unit tests, integration tests
2. **Database Migration (Initial Schema)** 
  - ✅create initial tables and SQL migration files
  - ✅Auto-run pending migrations on server start
3. **Upload Service**
  - ✅ Upload lifecycle management
  - ✅ WebSocket long-lived connection and protocol interaction with axon_transfer
  - ✅ Upload-related REST API endpoints
  - ✅ Device connection state management(refactor when connection with axon_recorder is ready)
  - ✅ Replace traditional database operations with sqlx
4. **task Scheduler**
  - ✅ support `GET /tasks/{id}/config` with mocked data
  - ✅ handle `/callbacks/start` & `/callbacks/finish`
  - ✅ task lifecycle management
5. **episodes Query and Filter**
  - ✅ episodes Query & Filter
6. **Workstation Management**
  - ✅ factory CRUD
  - ✅ robot_types CRUD
  - ✅ robots CRUD
  - ✅ data_collectors CRUD
  - ✅ workstations pairing and status management
7. **record service**
  - ✅ WebSocket long-lived connection with axon_recorder
  - ✅ refactor device connection state management(monitor both axon_recorder and axon_transfer)

---

## Version 0.2.0 (Working on)

Second Release Implementation Order:

1. **Authentication & Authorization**
  - JWT authentication
  - Role-based access control (RBAC)
  - API private key management
2. **Scene & Skill Management**
  - scene and subscene CRUD
  - skill and sop CRUD
3. **Order & Task Management**
  - order CRUD
  - batch CRUD
  - task dispatch
4. **upload queue**
  - Upload prioritization and bandwidth throttling
5. **QA Pipeline**
  - QA job triggering(dagster agent)(python)
  - basic QA checks(topics, duration, gaps, image integrity)(python)
  - awaiting inspection queue for auto-check failed episodes
  - Inspector CRUD & Inspection API
6. **cloud sync**
  - Sync worker: push approved episodes to cloud S3
  - Edge-to-Cloud Sync API
7. **API rate limiting and webhooks**
  - API rate limiting
  - webhooks for task status changes

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

1. **0.1.0 scope**: Code standards & CI → Database Migration → Role Management → Workstation Management → Task Management → Callback Endpoints & Episode Management → Transfer Server (WebSocket Upload Control)
2. **Tests**: Unit and integration tests for API and callbacks
3. **Documentation**: API examples and deployment guide
4. **Bug fixes**: Any bug reports will be addressed promptly

See CONTRIBUTING.md (if present) for guidelines.
