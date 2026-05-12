<!--
SPDX-FileCopyrightText: 2026 ArcheBase

SPDX-License-Identifier: MulanPSL-2.0
-->

# Paginated List Completeness Fix

**Scope:** Synapse Admin/operator pages and Keystone paginated list APIs

## 1. Problem

Some Synapse pages assume a large `limit` returns all rows, or fetch only the
first paginated page and then perform client-side grouping. Keystone
`ParsePagination()` caps `limit` at `100`, so any request such as `limit=500` or
`limit=2000` still returns at most 100 rows.

The visible failure is that `SOP 管理` can omit existing SOPs such as
`kitchen-move-water-bottle` when that SOP is not in the first `/sops` response
page.

## 2. Root Cause

- `SOP 管理` and `技能管理` fetch one backend page, then group rows by `slug` on
  the client and paginate the grouped result.
- Several pages preload related entities with fixed large limits for label
  maps, filter options, or dashboard summaries. These calls silently truncate at
  the backend max limit.
- `批次管理` has two post-action refresh paths that call the generic
  `fetchList()` without current pagination/filter parameters.

## 3. Fix Strategy

Keep standard table pagination server-side. Only use full pagination loops where
the page needs a complete local set:

- Client-side grouped management pages:
  - `src/views/admin/sops/SopList.vue`
  - `src/views/admin/skills/SkillList.vue`
- Local label/lookup caches:
  - `src/views/admin/tasks/TaskList.vue`
  - `src/views/admin/tasks/TaskDetail.vue`
  - `src/views/admin/batches/BatchDetail.vue`
  - `src/views/admin/orders/OrderDetail.vue`
  - `src/features/production/useDashboardData.js`
  - `src/views/admin/statistics/DataProductionStatistics.vue`
- Form option preloads that previously used the backend max page size:
  - `src/views/admin/collectors/CollectorForm.vue`
  - `src/views/admin/inspectors/InspectorForm.vue`
  - `src/views/admin/orders/OrderForm.vue`
  - `src/views/admin/orders/OrderList.vue`
  - `src/views/admin/organizations/OrganizationList.vue`
  - `src/views/admin/organizations/OrganizationDetail.vue`
- Operator batch selection:
  - `src/views/RobotControl.vue`

Use a reusable `fetchAllList()` helper in `useEntityCrud()` for entity CRUD
pages, and a small `fetchAllPages()` utility for API modules that expose
`list(params)`.

## 4. Implementation Checklist

- Add `fetchAllList(params)` to `useEntityCrud()`:
  - request pages with `limit=100`;
  - advance by the response `limit`;
  - stop on `hasNext=false` or an empty page;
  - update `items`, `totalCount`, `hasNext`, and `hasPrev`.
- Add `fetchAllPages(listFn, params)` for direct API modules.
- Change SOP and skill management to load all rows before client-side grouping.
- Replace fixed large-limit preloads with `fetchAllList()` or
  `fetchAllPages()`.
- Change batch cancel/recall refreshes to `fetchBatchesPage()` so current
  filters and pagination are preserved.
- Replace fixed `limit=100/200/500/1000/2000` preload calls with full-page
  loops where the UI needs complete local option or label maps.

## 5. Non-goals

- Do not raise Keystone's global `maxLimit`; the backend cap protects list APIs.
- Do not rewrite normal list tables to client-side pagination.
- Do not change API response shapes.

## 6. Validation

- Build Synapse with `npm run build`.
- Manually verify that SOP/skill list counts are based on all pages.
- Manually verify that filtered batch pages stay filtered after cancel/recall.
