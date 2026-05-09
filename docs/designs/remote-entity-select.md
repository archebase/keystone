<!--
SPDX-FileCopyrightText: 2026 ArcheBase

SPDX-License-Identifier: MulanPSL-2.0
-->

# Remote Entity Select Design

**Scope:** Synapse Admin select controls and Keystone list APIs

## 1. Purpose

This document defines a staged development plan for replacing large static select
option lists in Synapse Admin with remote searchable selects.

The immediate problem is that many Admin pages load only the first paginated
resource page, usually `limit=50`, and then build select options from that
partial client-side list. Once resource tables contain more rows than the first
page, users cannot find valid organizations, orders, scenes, SOPs, devices,
collectors, or workstations in select controls.

The proposed solution is to use a reusable remote select pattern similar to the
robot device ID and collector operator ID selectors in the data production
statistics page:

- Search options by keyword on the server.
- Page option results with `limit` and `offset`.
- Load more options without requiring full-table preloading.
- Preserve selected option labels even when the selected item is not in the
  current result page.
- Keep existing API pagination limits and avoid increasing all list calls to
  very large limits.

## 2. Background

### 2.1 Current frontend behavior

Synapse Admin currently uses two main select patterns:

- `BaseSelect` with fully materialized `options`.
- Page-local custom searchable selects in
  `src/views/admin/statistics/DataProductionStatistics.vue` for robot device ID
  and collector operator ID.

Many pages call `fetchList()` without explicit large pagination. The generic
`useEntityCrud()` composable defaults to `limit=50`, and API modules also
default to `limit=50`. This means option lists frequently represent only the
first page of a resource.

The batch management page is the most visible example. It builds filters and
form options from:

- `organizations`
- `orders`
- `stations`
- `robots`
- `data_collectors`
- `sops`
- `scenes`
- `subscenes`

These resources can all exceed one page in realistic deployments.

### 2.2 Current backend behavior

Keystone list APIs already expose standardized pagination:

```json
{
  "items": [],
  "total": 0,
  "limit": 50,
  "offset": 0,
  "hasNext": false,
  "hasPrev": false
}
```

`ParsePagination()` caps `limit` at `100`. This is acceptable and should stay in
place. The missing piece is consistent keyword search across list endpoints.

Search support is currently uneven:

| Resource | Endpoint | Current useful filters | Current keyword search |
|---|---|---|---|
| Organization | `GET /organizations` | `factory_id` | No |
| Scene | `GET /scenes` | `factory_id` | No |
| Subscene | `GET /subscenes` | `scene_id` | No |
| Skill | `GET /skills` | none | No |
| SOP | `GET /sops` | none | No |
| Robot type | `GET /robot_types` | none | No |
| Robot | `GET /robots` | `factory_id`, `status`, `robot_type_id`, `connected` | Yes: `keyword`, `q`, `search`, `device_id` |
| Data collector | `GET /data_collectors` | `organization_id`, `status` | Yes: `keyword`, `q`, `search`, `operator_id` |
| Inspector | `GET /inspectors` | `organization_id`, `certification_level`, `status` | No |
| Station | `GET /stations` | `factory_id`, `organization_id`, `robot_type_id`, `status` | No |
| Order | `GET /orders` | `organization_id`, `scene_id`, `status`, `priority`, `ids` | No |
| Batch | `GET /batches` | several list filters | Not the main select source |

## 3. Goals

- Provide one reusable remote select implementation for Admin pages.
- Support single-select and multi-select use cases.
- Support server-side keyword search and paginated option loading.
- Preserve selected labels across pagination, refresh, and route state restore.
- Keep static enum selects as simple `BaseSelect` controls.
- Make option fetching explicit and resource-specific rather than relying on
  page-wide preloading.
- Improve large-data correctness without weakening backend pagination limits.

## 4. Non-goals

- Do not replace every select in one large refactor.
- Do not build a generic backend `/options` endpoint in the first phase.
- Do not remove `BaseSelect`; it remains appropriate for enum and small static
  option sets.
- Do not change table pagination behavior.
- Do not change creation/update payload contracts for existing resources.

## 5. UX Contract

### 5.1 Single select

Single-select controls should support:

- Placeholder text.
- Clear selection when the field is optional.
- Keyword search with debounce.
- Loading state while searching.
- Empty state when no option matches.
- "Load more" when `hasNext` is true.
- Selected label display even if the selected option is not in the current
  option page.

### 5.2 Multi select

Multi-select controls should support:

- Search and page loading.
- Checkbox-style options.
- Selected chips.
- Remove a selected item from its chip.
- Clear all selected items.
- Collapsed chip preview when many values are selected.

### 5.3 Keyboard and focus behavior

Remote selects replace native `<select>` behavior, so the component must provide
basic keyboard and focus support:

- `Enter` selects the active option.
- `Escape` closes the dropdown without changing the current value.
- `ArrowDown` and `ArrowUp` move the active option.
- `Tab` leaves the control and closes the dropdown.
- Clicking outside closes the dropdown.
- Closing the dropdown returns focus to the search/input trigger.
- The trigger and dropdown should expose meaningful ARIA labels, active option
  state, selected state, and disabled state.

### 5.4 Dependent selects

Some select controls depend on another selected value:

| Parent | Child | Required behavior |
|---|---|---|
| Factory | Organization | Clear child when parent changes unless selected child still belongs to parent |
| Factory | Scene | Query scenes with `factory_id` |
| Scene | Subscene | Query subscenes with `scene_id`; disable until scene is selected |
| SOP slug | SOP version | Search/group by slug; version list is limited to selected slug |
| Organization | Order | Query orders with `organization_id` |
| Organization | Station | Query stations with `organization_id` |

Dependent selects must clear stale values before submitting forms.

## 6. Frontend Design

### 6.1 New component

Add a reusable component:

```text
synapse-worktree1/src/components/form/RemoteSelect.vue
```

Recommended props:

| Prop | Type | Notes |
|---|---|---|
| `modelValue` | `String \| Number \| Array` | Bound value |
| `multiple` | `Boolean` | Enables chip and checkbox behavior |
| `label` | `String` | Optional field label |
| `placeholder` | `String` | Search/input placeholder |
| `disabled` | `Boolean` | Disable input and dropdown |
| `required` | `Boolean` | Display required marker only |
| `error` | `String` | Validation error |
| `hint` | `String` | Helper text |
| `options` | `Array<{ value, label, meta? }>` | Current loaded options |
| `selectedOptions` | `Array<{ value, label, meta? }>` | Hydrated selected options; source of truth for displayed selected labels and chips |
| `loading` | `Boolean` | Shows loading state |
| `hasMore` | `Boolean` | Shows load-more row |
| `search` | `String` | Controlled search text |
| `clearable` | `Boolean` | Allow clearing optional fields |
| `maxVisibleChips` | `Number` | Multi-select chip preview limit |

Recommended emits:

| Emit | Payload | Notes |
|---|---|---|
| `update:modelValue` | selected value(s) | Standard Vue model update |
| `update:search` | search text | Enables debounced remote search |
| `open` | none | Parent/composable can load first page |
| `load-more` | none | Parent/composable loads next page |
| `clear` | none | Optional convenience event |

The component should focus only on rendering and interaction. It should not know
about Keystone resource names or API modules.

Rendering contract:

- The component displays selected labels from `selectedOptions`, not by searching
  only inside the current `options` page.
- The dropdown list renders `displayOptions`, which is the de-duplicated merge of
  `selectedOptions` and the current remote `options` page.
- For multi-select controls, chips are rendered from `selectedOptions`.
- If `selectedOptions` is empty for a non-empty value, the component may display
  the raw value temporarily, but the composable must attempt hydration.

### 6.2 New composable

Add a resource-aware composable:

```text
synapse-worktree1/src/composables/useRemoteOptions.js
```

Recommended API:

```js
const {
  options,
  selectedOptions,
  displayOptions,
  loading,
  hasMore,
  search,
  open,
  reload,
  loadMore,
  setSelectedFromValues,
  clear
} = useRemoteOptions({
  fetchPage,
  fetchByValues,
  mapOption,
  pageSize: 50,
  searchParam: 'keyword',
  baseParams: () => ({ organization_id: selectedOrg.value || undefined })
})
```

Responsibilities:

- Own `limit`, `offset`, `hasMore`, and request sequence handling.
- Debounce search input.
- Merge loaded options with selected options without duplicates.
- Keep selected labels after pagination changes.
- Expose `displayOptions` as `mergeOptions(selectedOptions, options)`.
- Hydrate selected values through `fetchByValues` or the resource-specific
  fallback rules in section 6.4.
- Reset options when `baseParams()` changes.
- Ignore stale responses from earlier searches.

### 6.3 Resource adapters

Add small adapter helpers instead of duplicating mapping logic in each page:

```text
synapse-worktree1/src/composables/remoteOptionAdapters.js
```

Example adapters:

| Adapter | Value | Label |
|---|---|---|
| `organizationOption` | `id` | `name || slug || id` |
| `sceneOption` | `id` | `name || id` |
| `subsceneOption` | `id` | `name || id` |
| `orderOption` | `id` | `name || id`, optionally include scene name |
| `stationOption` | `id` | station name + robot serial + collector operator ID |
| `robotOption` | `device_id` or `id` depending on caller | device ID + robot type |
| `collectorOption` | `operator_id` or `id` depending on caller | operator ID + name |
| `sopOption` | `id` | slug + version |
| `robotTypeOption` | `id` | name + model |
| `inspectorOption` | `id` | inspector ID + name |

Value choice must match the downstream filter or form payload. Adapters must be
separate when the same resource is used with different value semantics:

- `robotDeviceOption`: value is `device_id`, used by filters that submit
  `device_id`.
- `robotIdOption`: value is numeric `id`, used by forms that submit `robot_id`.
- `collectorOperatorOption`: value is `operator_id`, used by filters that submit
  `collector_operator_id`.
- `collectorIdOption`: value is numeric `id`, used by forms that submit
  `data_collector_id`.

### 6.4 Selected label resolution

Remote selects need a way to display labels for selected values that are not in
the current page.

MVP rule: selected label hydration is required for every remote select.

Hydration order:

1. If selected value exists in `options`, use that option.
2. If selected option was restored from page state, use the persisted label.
3. If the value is a numeric ID, call `fetchByValues(values)`.
4. Fallback to `{ value, label: value }`.

`fetchByValues(values)` has this required MVP behavior:

- If the endpoint supports an `ids` filter, use one list request.
- Otherwise, call `GET /:id` for each selected ID. This fallback is acceptable
  because selected value counts are small in the initial rollout.
- For multi-select controls, cap fallback hydration at 50 selected values and
  display raw values for any overflow until a batch lookup is added.

For resources selected by non-ID unique strings, such as robot `device_id` or
collector `operator_id`, hydration uses keyword search with the selected value
and then keeps only exact matches on the unique field. Persisted `{ value,
label }` remains the fallback if the exact search returns no row.

This MVP does not require every list API to support `ids`. A generic selected
option lookup endpoint can be added later if per-ID hydration becomes too slow.

### 6.5 Batch page value mapping

`BatchList.vue` is the first page where value semantics must be fixed before
implementation. The following mapping is the contract for Phase 3:

| Area | Control | Value | Label | Query or payload field | Dependencies |
|---|---|---|---|---|---|
| Filter row | Organization | `organizations.id` | `organization.name || slug` | `organization_id` | none |
| Filter row | Order | `orders.id` | `order.name`, optionally scene name | `order_id` | optional `organization_id` |
| Filter row | Scene | `scenes.id` | `scene.name` | `scene_id` | optional factory context |
| Filter row | Device ID | `robots.device_id` | `device_id`, optionally robot type | `device_id` | optional `robot_type_id` |
| Filter row | Collector operator ID | `data_collectors.operator_id` | `operator_id + name` | `collector_operator_id` | optional `organization_id` |
| Create/edit form | Order | `orders.id` | `order.name`, optionally scene name | `order_id` | optional `organization_id` |
| Create/edit form | Workstation | `workstations.id` | station name + robot serial + collector operator ID | `workstation_id` | selected order organization when available |
| Create/edit form | SOP slug | `sops.slug` | `slug` | local grouping only | none |
| Create/edit form | SOP version | `sops.version` | `version` | resolves selected `sop_id` from slug + version | selected SOP slug |
| Create/edit form | Scene | `scenes.id` | `scene.name` | local subscene filter only | selected order scene when order is chosen |
| Create/edit form | Subscene | `subscenes.id` | `subscene.name` | `task_groups[].subscene_id` | selected scene |

When an order is selected in create/edit forms, the form should derive the scene
from that order and clear any task group subscene that does not belong to the
derived scene.

## 7. Backend API Design

### 7.1 Standard query parameters

All large resource list endpoints used by remote selects should accept:

| Query | Meaning |
|---|---|
| `limit` | Page size, still capped by `ParsePagination()` |
| `offset` | Page offset |
| `keyword` | Canonical search term |
| `q` | Alias of `keyword` |
| `search` | Alias of `keyword` |

Resource-specific aliases can remain for compatibility:

- Robots: `device_id`
- Data collectors: `operator_id`

New code should use `keyword` unless there is a strong reason to use a
resource-specific alias.

### 7.2 Search fields by resource

| Resource | Search fields |
|---|---|
| Organization | `name`, `slug`, `description` |
| Factory | `name`, `slug`, `location` |
| Scene | `name`, `description` |
| Subscene | `name`, `description` |
| Skill | `slug`, `description`, `version` |
| SOP | `slug`, `description`, `version` |
| Robot type | `name`, `model`, `manufacturer`, `end_effector` |
| Robot | `device_id`, `asset_id` |
| Data collector | `name`, `operator_id`, `email` |
| Inspector | `name`, `inspector_id`, `email` |
| Station | `name`, `robot_serial`, `collector_operator_id`, `robot_name`, `collector_name` |
| Order | `name`, joined `scene.name`, joined `organization.name` |

### 7.3 Backend implementation notes

- Reuse `firstNonEmptyQuery(c, "keyword", "q", "search", "...")`.
- Keep the count query and data query filters identical.
- Always use parameterized `LIKE ?`; do not concatenate user input into SQL.
- Escape user `%` and `_` if exact literal matching becomes important. For the
  first phase, current `LIKE "%keyword%"` behavior is acceptable.
- For keyword searches, order exact matches first, prefix matches second,
  substring matches third, and then use `id DESC` as the final stable tie-breaker.
  For non-search list requests, keep the existing `ORDER BY id DESC`.
- Ensure joined search fields do not change tenant filtering semantics.
- Regenerate Swagger docs after API query parameters are added.

Recommended SQL ordering shape:

```sql
ORDER BY
  CASE
    WHEN primary_field = ? THEN 0
    WHEN primary_field LIKE ? THEN 1
    WHEN secondary_field = ? THEN 2
    WHEN secondary_field LIKE ? THEN 3
    ELSE 4
  END,
  id DESC
```

The concrete fields differ by resource. The important contract is that an exact
or prefix match should not be buried below unrelated newer rows.

## 8. Rollout Plan

### Phase 1: Shared frontend primitive

Deliver:

- `RemoteSelect.vue`
- `useRemoteOptions.js`
- `remoteOptionAdapters.js`
- Replace the custom robot and collector search-select logic in
  `DataProductionStatistics.vue` with the shared primitive.

Acceptance criteria:

- Existing statistics page behavior is preserved.
- Robot and collector selects still support search, load more, selected chips,
  and restored labels.
- No other Admin pages are changed in this phase.

### Phase 2: Backend search consistency

Deliver keyword search for endpoints that are needed by batch and common Admin
filters:

- `GET /factories`
- `GET /organizations`
- `GET /orders`
- `GET /scenes`
- `GET /subscenes`
- `GET /skills`
- `GET /sops`
- `GET /robot_types`
- `GET /stations`
- `GET /inspectors`

Acceptance criteria:

- Each endpoint supports `keyword`, `q`, and `search`.
- Existing filters still compose with keyword search.
- Pagination metadata remains correct.
- Keyword searches rank exact and prefix matches ahead of substring matches.
- Tests cover at least one positive and one filtered-empty search case per
  changed handler group where practical.

### Phase 3: Batch page replacement

Replace large-resource selects in:

```text
synapse-worktree1/src/views/admin/batches/BatchList.vue
```

Priority controls:

- Filter row: organization, order, scene, device ID, collector operator ID.
- Create/edit forms: order, workstation, SOP slug/version, scene, subscene.

Acceptance criteria:

- With more than 100 rows in each resource table, users can find rows outside
  the first page.
- Controls follow the value, label, payload, and dependency contract in section
  6.5.
- Changing a parent select clears stale dependent child values.
- Batch creation still submits numeric IDs where required by Keystone.
- Filter controls still submit the same query parameters as before.

### Phase 4: Common Admin pages

Replace high-risk large option selects in:

- `admin/collectors`
- `admin/inspectors`
- `admin/orders`
- `admin/robots`
- `admin/sops`
- `admin/workstations`
- `admin/tasks`, if it uses large resource filters

Acceptance criteria:

- Static enum selects remain `BaseSelect`.
- Large entity selects use `RemoteSelect`.
- Pages do not preload full resource lists just to build select options.

### Phase 5: Cleanup

Deliver:

- Remove duplicated custom search-select code from pages.
- Remove obsolete page-wide option preload calls.
- Document resource adapter conventions in component comments or `src/README.md`
  if needed.

## 9. Testing Strategy

### 9.1 Backend

Recommended Go tests:

- List handlers still return default pagination.
- `keyword` narrows results.
- Existing filters compose with `keyword`.
- Invalid numeric filters still return `400`.
- `total`, `hasNext`, and `hasPrev` reflect the filtered result set.
- Exact and prefix matches sort before substring-only matches.

Recommended commands:

```bash
cd keystone-worktree1
go test ./internal/api/handlers/... -run 'List|Search' -v
go test ./internal/api/handlers/... -v
```

### 9.2 Frontend

Synapse has no configured test runner. Manual verification should cover:

- Search returns matching options after debounce.
- Load-more appends options without duplicates.
- Selected label persists after closing and reopening the dropdown.
- Selected label displays correctly when the selected row is outside page 1.
- Multi-select chips can be added and removed.
- Dependent selects reset stale child values.
- Forms submit the same payload shape as before.
- Filter URLs/API params remain unchanged.
- `Enter`, `Escape`, `ArrowUp`, `ArrowDown`, `Tab`, and click-outside behavior
  match section 5.3.

Recommended manual data setup:

- At least 120 organizations, orders, scenes, robots, collectors, and stations.
- At least 80 SOP rows with multiple versions.
- At least 3 subscenes per test scene.

## 10. Risks and Mitigations

| Risk | Impact | Mitigation |
|---|---|---|
| Replacing all selects at once causes regressions | High | Roll out by page and preserve `BaseSelect` for static enums |
| Selected label cannot be resolved after restore | Medium | Persist `{ value, label }`, pass `selectedOptions`, and hydrate through `fetchByValues` |
| Backend search is inconsistent across endpoints | Medium | Standardize on `keyword/q/search` and document search fields |
| Dependent selects submit stale child IDs | High | Clear child values when parent filter changes |
| Remote select makes too many requests | Medium | Debounce search and ignore stale responses |
| Large joins slow down search | Medium | Keep search fields indexed where practical; avoid unnecessary joins |

## 11. Open Questions

- Should all resource list APIs eventually support an `ids` filter for faster
  selected-label hydration, or is per-ID fallback sufficient?
- Should station labels include status, or only stable identity fields such as
  station name, robot serial, and collector operator ID?
- Should SOP remote selection expose slug/version as two coordinated controls,
  or a single `slug@version` selector for forms?

## 12. Recommended First Implementation Slice

The recommended first slice is:

1. Implement `RemoteSelect.vue` and `useRemoteOptions.js`.
2. Migrate robot and collector selects in data production statistics to prove
   compatibility with the existing behavior.
3. Add keyword search to `organizations`, `orders`, `scenes`, `subscenes`,
   `skills`, `sops`, and `stations`.
4. Replace the batch page filter row with remote selects.
5. Replace the batch create/edit form selects.

This slice directly targets the current dropdown completeness problem while
keeping the change set reviewable.
