<!--
SPDX-FileCopyrightText: 2026 ArcheBase

SPDX-License-Identifier: MulanPSL-2.0
-->

# Multi-Tenancy Hierarchy Redesign

**Status:** Accepted  
**Migration:** `000004_multi_tenancy_hierarchy.up.sql`

---

## 1. Background and Motivation

Keystone is deployed **per-factory** (one Keystone instance per physical data-collection site).
Each factory may host **multiple organizations** (tenants) that operate their own robots,
data-collectors, inspectors, and orders independently.

The original schema modelled the hierarchy as:

```
Organization → Factory → Scene / Robot
```

This was designed for a cloud-side multi-factory view and does not fit the edge deployment
model, where a single factory is the fixed physical boundary and organizations are tenants
within it.

Key problems with the original model:

| Problem | Root cause |
|---------|-----------|
| `factories.organization_id` implies one org owns the factory; wrong for multi-tenant edge | Inverted hierarchy |
| `data_collectors` and `inspectors` have no org scope; cross-tenant isolation impossible | Missing FK |
| `station.go:203` TODO comment: cannot validate robot ↔ DC belong to same context | Missing org on DC |
| `orders.organization_id` derived via `scene → factory → organization`; fragile chain | Hierarchy inversion |

---

## 2. Target Hierarchy

```
Factory  (physical boundary, one per Keystone deployment)
├── Organization A  (tenant)
│   ├── DataCollectors
│   ├── Inspectors
│   └── Orders  ──────────→  Scene (shared factory resource)
└── Organization B
    ├── DataCollectors
    ├── Inspectors
    └── Orders  ──────────→  Scene

Factory
└── Robots  (physical assets, owned by the factory)
    └── Workstation = Robot × DataCollector
                      ↑            ↑
                  factory_id   organization_id
                  (from robot) (from data_collector)
```

### 2.1 Ownership rules

| Entity | Belongs to | Key field |
|--------|-----------|-----------|
| `organizations` | Factory | `factory_id` (new) |
| `scenes` | Factory | `factory_id` (unchanged) |
| `robots` | Factory | `factory_id` (unchanged) |
| `data_collectors` | Organization | `organization_id` (new) |
| `inspectors` | Organization | `organization_id` (new) |
| `orders` | Organization | `organization_id` (unchanged value, derivation path changes) |
| `workstations` | Robot × DataCollector | `factory_id` (denorm, from robot) + `organization_id` (denorm, new, from DC) |

### 2.2 Derivation chains

```
workstation.factory_id      = workstation.robot_id → robots.factory_id
workstation.organization_id = workstation.data_collector_id → data_collectors.organization_id

order.organization_id (on CREATE) = scene_id → scenes.factory_id
                                              → factories (this factory)
                                              → request must supply organization_id explicitly,
                                                validated against organizations.factory_id = this factory
```

Because `orders.organization_id` is now a first-class field supplied by the caller (not
silently derived from the factory chain), the API for `POST /orders` gains a required
`organization_id` parameter.

---

## 3. Schema Changes

### 3.1 `organizations` — add `factory_id`, drop `factory_count` computed field

```sql
ALTER TABLE organizations
    ADD COLUMN factory_id BIGINT NOT NULL AFTER id;

-- factory_count was a virtual count in the API response layer,
-- not a real column; no column drop needed.
-- The API response removes factoryCount field.
```

The `factory_id` FK is **not** enforced at the DB level via FOREIGN KEY to keep migration
simple and consistent with the rest of the schema (no FK constraints elsewhere).
Application-level validation is used instead (same pattern as `factories.organization_id`
today).

### 3.2 `factories` — drop `organization_id`

```sql
ALTER TABLE factories
    DROP COLUMN organization_id;
-- Also drop associated index idx_org on factories.
```

Existing `organization_id` values are migrated to `organizations.factory_id` in the
migration script before the column is dropped.

### 3.3 `data_collectors` — add `organization_id`

```sql
ALTER TABLE data_collectors
    ADD COLUMN organization_id BIGINT NOT NULL AFTER id;
```

Existing rows: assign to the first organization of the factory (migration seeds from
`organizations ORDER BY id LIMIT 1`). In production, operators should re-assign via API
after migration.

### 3.4 `inspectors` — add `organization_id`

```sql
ALTER TABLE inspectors
    ADD COLUMN organization_id BIGINT NOT NULL AFTER id;
```

Same migration seed strategy as `data_collectors`.

### 3.5 `workstations` — add `organization_id`

```sql
ALTER TABLE workstations
    ADD COLUMN organization_id BIGINT NOT NULL AFTER factory_id;
```

Populated on insert/update from `data_collectors.organization_id`. Kept as a denormalized
column (same pattern as `factory_id`) to avoid JOIN on every task/episode query.

`workstations.factory_id` is **retained** as a denormalized column; its source changes
from `factories ← organizations` to `robots → factories` — but since `robots.factory_id`
already pointed to the same factory, the stored values are identical.

---

## 4. API Contract Changes

### 4.1 `GET /organizations`

- Response: remove `factoryCount` field, add `factory_id` field.
- Filter param: no `factory_id` query param needed (only one factory per deployment).

### 4.2 `POST /organizations`

- Request: add required `factory_id` field.
- Validation: the referenced factory must exist and not be soft-deleted.

### 4.3 `PUT /organizations/:id`

- Request: `factory_id` is **immutable** after creation; reject if supplied.

### 4.4 `DELETE /organizations/:id`

- Block if the organization has associated `data_collectors`, `inspectors`, or `orders`.
  (Previously blocked on `factories`.)

### 4.5 `GET /factories` / `GET /factories/:id`

- Response: remove `organization_id` field.
- `POST /factories` / `PUT /factories/:id`: drop `organization_id` from request body.

### 4.6 `POST /data_collectors`

- Request: add required `organization_id` field.
- Validation: organization must exist, `organization.factory_id` must match system factory.

### 4.7 `GET /data_collectors`

- Add optional filter param `organization_id`.
- Response items: add `organization_id` field.

### 4.8 `POST /inspectors`

- Same pattern as `data_collectors`.

### 4.9 `GET /inspectors`

- Add optional filter param `organization_id`.
- Response items: add `organization_id` field.

### 4.10 `POST /stations`

- No new request field required. `organization_id` is **derived** from the selected
  `data_collector_id` at creation time and stored as a denormalized column.
- **New validation**: the robot's factory and the data_collector's organization must both
  belong to the same factory (i.e., `data_collectors.organization_id →
  organizations.factory_id == robots.factory_id`).
- Response: add `organization_id` field.

### 4.11 `POST /orders`

- Request: add required `organization_id` field (was previously silently derived).
- Validation: `organization_id` must belong to the same factory as the order's scene
  (`scene_id → scenes.factory_id == organizations.factory_id`).

---

## 5. Handler-level Logic Changes

### 5.1 `organization.go`

| Operation | Change |
|-----------|--------|
| `CreateOrganization` | Accept and validate `factory_id`; store it |
| `ListOrganizations` | Return `factory_id`; remove `factory_count` subquery |
| `GetOrganization` | Return `factory_id`; remove `factory_count` subquery |
| `UpdateOrganization` | Reject `factory_id` in body (immutable) |
| `DeleteOrganization` | Change dependency check: count `data_collectors` + `inspectors` + `orders`; remove `factories` count check |

### 5.2 `factory.go`

| Operation | Change |
|-----------|--------|
| All | Remove `organization_id` from all queries, requests, and responses |
| `DeleteFactory` | Remove `organization_id`-related child checks (orgs are no longer factory children in the deletion direction) |

### 5.3 `data_collector.go`

| Operation | Change |
|-----------|--------|
| `CreateDataCollector` | Accept and validate `organization_id` |
| `ListDataCollectors` | Accept optional `organization_id` filter; return `organization_id` in response |
| `GetDataCollector` | Return `organization_id` |
| `UpdateDataCollector` | `organization_id` is **immutable** after creation; reject if supplied |
| Workstation sync | When `name`/`operator_id` changes, also sync `workstations.organization_id` if `organization_id` were ever mutable — but since it is not, no extra sync needed |

### 5.4 `inspector.go`

| Operation | Change |
|-----------|--------|
| `CreateInspector` | Accept and validate `organization_id` |
| `ListInspectors` | Accept optional `organization_id` filter; return `organization_id` in response |
| `GetInspector` | Return `organization_id` |
| `UpdateInspector` | `organization_id` immutable |

### 5.5 `station.go`

| Operation | Change |
|-----------|--------|
| `CreateStation` | After loading DC, read `dc.organization_id`; validate that `organizations[dc.organization_id].factory_id == robot.factory_id`; store `organization_id` in workstation row |
| `UpdateStation` | If DC changes, re-derive and re-validate `organization_id`; update `workstations.organization_id` |
| `ListStations` / `GetStation` | Return `organization_id` in response |

### 5.6 `order.go`

| Operation | Change |
|-----------|--------|
| `CreateOrder` | Accept explicit `organization_id` from request; validate against scene's factory |
| `ListOrders` | Accept optional `organization_id` filter |

---

## 6. Denormalization Strategy (unchanged pattern)

The system already uses intentional denormalization in `workstations`, `tasks`, and
`episodes` to avoid JOIN chains on hot query paths.

| Column | Table | Source | Sync trigger |
|--------|-------|--------|-------------|
| `factory_id` | `workstations` | `robots.factory_id` | workstation create / robot.factory_id update |
| `organization_id` | `workstations` | `data_collectors.organization_id` | workstation create / DC swap on update |
| `factory_id` | `tasks` | `workstations.factory_id` | task create (batch dispatch) |
| `organization_id` | `tasks` | `workstations.organization_id` | task create (batch dispatch) |
| `factory_id` | `episodes` | `tasks.factory_id` | episode create (upload ACK) |
| `organization_id` | `episodes` | `tasks.organization_id` | episode create (upload ACK) |

No additional sync paths are introduced. The existing sync logic for `tasks` and `episodes`
already reads `workstation.factory_id` / `workstation.organization_id`; adding
`organization_id` to workstations makes the `organization_id` sync path consistent with
`factory_id`.

---

## 7. Migration Script Notes

Migration `000004` must execute in this order:

1. **Backfill**: copy `factories.organization_id` values into `organizations.factory_id`
   (reverse the existing FK direction).
2. **Add** `organizations.factory_id` column (populated in step 1).
3. **Drop** `factories.organization_id` column and its index.
4. **Add** `data_collectors.organization_id` — seed with first org of the factory.
5. **Add** `inspectors.organization_id` — same seed.
6. **Add** `workstations.organization_id` — derive from `data_collectors.organization_id`
   via the existing `workstations.data_collector_id` FK.

Down migration reverses all steps.

---

## 8. Out of Scope

- Frontend (synapse) changes — tracked separately.
- RBAC / JWT claims changes (planned for v0.2.0 RBAC milestone).
- `axon` changes — axon is unaware of org/factory hierarchy; no changes required.
