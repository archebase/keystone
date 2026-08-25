<!--
SPDX-FileCopyrightText: 2026 ArcheBase
SPDX-License-Identifier: MulanPSL-2.0
-->

# Keystone Mercury 数采计划增量同步优化方案

## 1. 文档目的

本文记录 Keystone Mercury 自动同步 Hilbert 数采计划（`dc_plan`）的性能优化方案，以及与 Hilbert 侧数采计划生命周期的约束。

目标是让每 5 分钟一次的同步满足以下原则：

- 首次同步或真正发生业务变化时，执行必要的本地投影和任务池维护；
- 上一轮已经同步、且没有业务变化的计划，下一轮只做轻量检查，不重复执行昂贵操作；
- 未指定采集设备的计划在同步时只落本地，`activate` 只负责创建/解析并激活 workstation；批量设备补绑交给异步 Binding Worker；
- Binding Worker 按计划串行执行“补绑设备 -> 更新本地计划 -> 创建/补齐该计划任务”，不先批量绑定再批量建任务；
- 一个 workspace 的慢同步不影响其他 workspace；
- 保留必要的修复能力，避免本地数据偶发不完整后永久不一致。

## 2. 已确认的业务事实

### 2.1 数采计划创建后不允许修改配置

生产业务确认：数采计划创建后，不允许修改计划配置字段。

不可变配置包括：

- `name`
- `description`
- `dc_factory_id`
- `dc_service_provider_id`
- `operator`
- `dc_project_id`
- `dc_task_id`
- `dc_type`
- `dc_date`
- `target_count`
- `target_duration`

Hilbert 当前代码仍保留 `/v1/data-collection/dc-plan/update` 接口。本次 Keystone 优化暂不修改 Hilbert，也暂不增加 Hilbert 侧的禁止更新限制；方案以生产业务约定“创建后不修改配置”为前提。

如果未来 Hilbert 侧重新开放或实际发生配置修改，Keystone 需要通过后续的完整校验或专门的配置变更处理发现并处理，不属于本次优化范围。

### 2.2 允许变化的字段

计划创建后，Keystone 需要继续关注：

- `status`：采集状态流转；
- `dc_device_id`：允许从 `NULL` 补绑为具体设备，不能重复改绑；
- `dc_device_name`：随设备补绑产生；
- `cur_count`：当前采集数量；
- `cur_duration`：当前采集时长；
- `updated_by`、`updated_time`：审计/运行字段。

其中设备绑定的业务语义是单向的：

```text
NULL -> device_id
```

Hilbert 已有 `patch-dc-device-id` 接口，并且只允许为空设备的计划进行补绑。

## 3. 当前实现和性能问题

当前 Keystone 每轮按 workspace 分页拉取完整计划快照，然后将 Go 结构重新 `json.Marshal` 后与本地 `raw_payload` 比较：

```text
raw_payload 不相同
    -> changedPlans
    -> workstation projection
    -> Ego Portal pending pool maintenance
```

完整 `raw_payload` 包含运行态字段，例如：

- `status`
- `cur_count`
- `cur_duration`
- `updated_by`
- `updated_time`
- `dc_device_id`

因此，运行态字段变化可能导致一条计划被当成“完整计划发生变化”，进而重复触发下游处理。

生产日志中，workspace 4 的表现为：

```text
358 个 dc_plan
211 个已 collected
147 个 active plan
147 个 workstation reused
147 个 pending pool plan
约 40 多秒
```

其中 211 个 collected 计划会被直接跳过投影；真正重复产生较大开销的候选对象是剩余 147 个 active plan。当前 workstation projection 和 pending pool maintenance 都是逐计划串行事务，包含多次数据库查询和事务提交。

## 4. 优化目标模型

同步仍然需要从 Hilbert 拉取完整分页快照，因为当前 Hilbert query 接口没有增量 cursor 或 `updated_since` 参数。优化重点放在 Keystone 本地处理：

```text
全量拉取
    -> 轻量分类变化
    -> 只对必要的计划执行下游动作
```

### 4.1 计划分类

建议将每轮同步结果分类为：

- `new_plans`：本地不存在的新计划；
- `status_changed_plans`：状态发生变化的计划；
- `collected_plans`：本轮变为 `collected` 的计划；
- `device_bound_plans`：设备从 `NULL` 变为具体设备的计划；
- `progress_changed_plans`：只有 `cur_count`/`cur_duration` 变化的计划；
- `missing_remote_plans`：本地存在但远端快照中消失的计划；
- `immutable_field_changes`：不可变配置字段出现变化的计划；
- `projection_plans`：确实需要 workstation projection 的计划；
- `pending_pool_plans`：确实需要 pending pool 维护的计划。

`raw_payload` 仍然保存，用于审计、排障和兼容，但不再直接决定是否执行所有下游动作。

## 5. 推荐同步规则

### 5.1 新计划

#### 新计划且已绑定设备

执行：

1. 写入本地 `dc_plan`；
2. 创建或复用 workstation；
3. 按设备类型维护 pending task pool。

#### 新计划且未绑定设备

执行：

1. 写入本地 `dc_plan`；
2. 标记为等待设备绑定；
3. 不创建正式 workstation；
4. 不创建没有真实 workstation 的正式 task pool；
5. 等待设备激活后由 Binding Worker 补绑。

workstation 的创建/解析由 `activate` 负责，但计划补绑和任务创建不在 `activate` 的同步请求中执行。

### 5.2 已存在且没有业务变化

如果以下关键字段均未变化：

- `status`
- `dc_device_id`
- 设备/操作员绑定关系

并且本地已有有效 workstation，则：

```text
只完成本地轻量比对和必要镜像更新；
不执行 workstation projection；
不执行 pending pool maintenance。
```

### 5.3 status 变为 collected

执行：

1. 更新本地计划状态；
2. 取消该计划下仍为 `pending` 的本地任务；
3. 跳过 workstation projection；
4. 跳过 pending pool maintenance。

`collected` 不应再计入 `BlockedCount`，建议单独统计为：

```text
skipped_collected_count
```

### 5.4 status 发生普通流转

例如：

```text
pending_collection -> collecting
```

通常只更新本地状态，不重复创建或复用 workstation。

只有当状态变化跨越 pending pool 的启用/禁用边界时，才维护任务池。

### 5.5 设备从 NULL 补绑为具体设备

设备补绑由异步 Binding Worker 完成，而不是由 `activate` 同步执行。

`activate` 只负责：

1. 认证数采员和设备；
2. 解析或创建当前设备对应的 workstation；
3. 激活 workstation session；
4. 快照当前 operator/workspace 下的未绑定计划；
5. 创建 Binding Job；
6. 立即返回 workstation token。

Binding Worker 对每个计划串行执行：

```text
Plan A:
    调 Hilbert patch-dc-device-id
    更新 Keystone dc_plan
    使用 activate 已确定的 workstation
    创建/补齐 Plan A 的正式 task/pool
    标记 Plan A 完成

Plan B:
    调 Hilbert patch-dc-device-id
    更新 Keystone dc_plan
    使用同一个 workstation
    创建/补齐 Plan B 的正式 task/pool
    标记 Plan B 完成
```

Worker 的正常路径不创建 workstation。Binding Job 应保存：

- `workspace_id`
- `operator_id`
- `device_id`
- `workstation_id`
- 计划 item 和处理进度

这样 100 个未绑定计划不会导致 `activate` 串行执行 100 次 Hilbert 请求；用户可以先成功登录，Worker 再逐个完成计划绑定和任务准备。

本地更新必须使用条件：

```sql
WHERE id = ?
  AND workspace_id = ?
  AND dc_device_id IS NULL
```

Hilbert 调用不得放在一个长时间持有的本地数据库事务中。每个计划应具备独立的重试、冲突和完成状态。

如果 Worker 发现 activate 创建的 workstation 已被删除、设备或 workspace 不匹配，不应在正常绑定路径中偷偷创建新 workstation；应暂停当前 item、记录错误并等待 repair 或重新 activate。

### 5.6 只有采集进度变化

如果只有以下字段变化：

- `cur_count`
- `cur_duration`

则：

1. 更新本地镜像；
2. 不重复执行 workstation projection；
3. 只有当剩余任务数量确实发生需要调整的变化时，才维护 pending pool。

如果 pending pool 的数量策略允许，则不因每次进度变化都执行任务池检查。

### 5.7 远端计划消失

继续使用当前快照语义：

```text
本地 active plan 不在远端完整快照中
    -> 本地软删除
```

同时根据现有任务策略取消或保留相关本地任务，避免产生孤儿任务。

## 6. 未指定设备计划的任务池方案

### 6.1 不提前创建正式 pending task

当前正式 `tasks` 依赖真实 `workstation_id`。而未指定设备的计划没有确定：

- robot；
- device；
- workstation；
- 设备类型对应的执行上下文。

因此不建议为这类计划提前创建正式 pending task pool。

### 6.2 activate 与 Binding Worker 的职责

推荐职责边界：

```text
activate：
    认证数采员和设备
    创建/解析 workstation
    激活 workstation
    创建 Binding Job
    立即返回 token

Binding Worker：
    一个计划一个计划处理
    Hilbert 补绑设备
    更新本地 dc_plan
    使用已存在的 workstation
    创建/补齐该计划 task/pool
```

Binding Worker 不应该在正常流程中重新创建 workstation。workstation 已由 `activate` 确定，Worker 只引用 `job.workstation_id`。

如果 workstation 在 Worker 执行期间失效，应该将 item 标记为可重试或阻塞，并等待修复，不应无条件生成新的 workstation。

### 6.3 Binding Worker 的简化实现

第一版不增加 Binding Job 和 Job Item 数据表，不做持久化进度记录。

`activate` 成功后，在 Keystone 进程内提交一个异步 Binding Job，由 Binding Worker 按计划顺序逐个处理：

```text
Plan A:
    Hilbert 补绑
    更新 Keystone dc_plan
    使用已有 workstation 创建/补齐 Plan A 的 task/pool
    进入下一个计划
```

第一版只需要在内存中维护：

- 待处理计划列表；
- 当前处理位置；
- 成功数量；
- 失败数量；
- 当前错误。

如果 Keystone 在处理过程中重启，未完成的 Binding Job 会丢失，需要数采员重新 activate 或通过后续同步/修复流程重新触发。这是第一版为了降低实现复杂度接受的限制。

后续如果需要支持进程重启恢复、可观测进度、自动重试和管理端查看，再增加持久化 Job/Item 模型。

### 6.4 单计划处理顺序

每个 item 必须按以下顺序串行完成：

```text
1. 读取并校验本地计划
2. 如果已绑定当前设备，直接进入任务准备
3. 如果仍未绑定，调用 Hilbert patch-dc-device-id
4. 条件更新本地 dc_plan
5. 使用 job.workstation_id 创建/补齐该计划 task/pool
6. 继续下一个 item
```

不采用：

```text
先绑定 100 个计划
    -> 再统一创建 100 个计划的 task
```

这样每个计划绑定成功后立即可用，单个计划失败不影响后续计划。

### 6.5 不提前创建正式 pending task

未绑定设备的计划在同步阶段只保存 `dc_plan`，不创建没有真实 workstation 的正式 task。

Binding Worker 在单个计划补绑成功后，再使用已存在的 workstation 创建正式 task/pool。

如果业务需要在设备绑定前展示待领取数量，可以增加独立的候选池，但不要直接把未绑定计划写入正式 `tasks` 表。

## 7. 数据库和代码改造建议

### 7.1 增加本地计划摘要查询

将当前按计划逐条查询已有 workspace 的方式改为一次加载当前 workspace 的计划摘要：

```sql
SELECT
    id,
    status,
    dc_device_id,
    target_count,
    cur_count,
    cur_duration,
    raw_payload,
    deleted_at
FROM dc_plan
WHERE workspace_id = ?
```

按 `id` 建立内存 map，再完成变化分类。

避免当前类似下面的 N 次查询：

```go
for _, plan := range plans {
    SELECT workspace_id FROM dc_plan WHERE id = ?
}
```

### 7.2 将 raw_payload 与下游触发条件解耦

建议保留：

```text
raw_payload：完整镜像和排障
关键字段：驱动同步动作
```

下游动作只由以下事件触发：

- 新计划；
- 设备补绑；
- 本地 projection 缺失；
- 状态关闭或状态跨越任务池边界。

### 7.3 修正 projection 统计

建议将当前摘要：

```go
type DCPlanWorkstationProjectionSummary struct {
    TotalPlans   int
    CreatedCount int
    ReusedCount  int
    BlockedCount int
}
```

调整为：

```go
type DCPlanWorkstationProjectionSummary struct {
    TotalPlans            int
    CreatedCount          int
    ReusedCount           int
    SkippedCollectedCount int
    WaitingForDeviceCount int
    BlockedCount          int
}
```

分类建议：

```text
collected -> SkippedCollectedCount
DCDeviceID == nil -> WaitingForDeviceCount
真正查询/事务错误 -> BlockedCount
```

### 7.4 保留 repair 查询

每轮仍然执行轻量 repair 查询：

```text
active + 已绑定设备 + 缺少有效 workstation
```

只对查询出的缺失投影计划执行 repair，不对所有 active plan 重复执行 projection。

## 8. 配置不可变约定

本次优化依赖生产业务约定：数采计划创建后不修改配置字段。第一版不修改 Hilbert 的 Update 接口，也不在 Keystone 中增加低频完整配置校验。

如果未来需要支持配置修改，必须单独设计配置变更同步和下游投影修复，不能直接依赖本方案的不可变假设。

## 9. 日志和指标

每个 workspace 的同步结果建议至少输出：

```text
workspace_id
fetched_count
new_count
status_changed_count
collected_count
device_bound_count
progress_changed_count
immutable_field_change_count
projection_count
waiting_for_device_count
pending_pool_count
remote_deleted_count
fetch_duration_ms
upsert_duration_ms
projection_duration_ms
pending_pool_duration_ms
total_duration_ms
```

示例：

```text
[DC_PLAN] workspace sync classified:
  workspace_id=4
  fetched=358
  new=0
  status_changed=0
  collected=0
  device_bound=0
  progress_changed=0
  immutable_field_changes=0
  projection=0
  waiting_for_device=0
  pending_pool=0
  duration_ms=3200
```

对于首次同步或真实变化：

```text
[DC_PLAN] workspace sync classified:
  workspace_id=4
  fetched=359
  new=1
  status_changed=0
  collected=0
  device_bound=0
  projection=1
  pending_pool=1
```

## 10. 实施顺序

### 阶段一：测量

先增加阶段耗时和分类计数，不改变业务结果：

- fetch；
- validate；
- upsert；
- repair selection；
- workstation projection；
- pending pool maintenance；
- total。

目标是确认当前 workspace 4 的约 40 秒具体花费。

### 阶段二：变化分类

引入内部 change set：

- new；
- status changed；
- collected；
- device bound；
- progress changed；
- immutable field changed。

让 `raw_payload` 不再直接触发所有下游处理。

### 阶段三：activate 与 Binding Worker 解耦

修改 `activate`：

- 只创建/解析并激活 workstation；
- 不调用 Hilbert `patch-dc-device-id`；
- 不创建计划 task/pool；
- 在进程内提交异步 Binding Job；
- 立即返回 workstation token。

新增 Binding Worker：

- 使用内存中的待处理计划列表；
- 一个计划一个计划串行处理；
- 每个计划按“Hilbert 补绑 -> 本地更新 -> 使用已有 workstation 创建 task/pool”顺序执行；
- 单个计划失败不阻塞后续计划；
- 第一版不保证 Keystone 重启后的任务恢复。

### 阶段四：按需投影和任务池

只对必要计划执行：

- 新增且有设备；
- Binding Worker 刚刚完成设备补绑；
- 本地缺失 projection；
- 状态跨越任务池边界。

无设备计划不创建正式 task；设备补绑成功后由 Binding Worker 创建该计划的正式 task/pool。

### 阶段五：批量读取和减少事务

- workspace 内计划一次性加载；
- 内存 map 比较；
- 减少逐计划查询；
- Binding Worker 每个计划独立处理，避免一个长事务包住全部 Hilbert 请求；
- 后续根据耗时决定是否批量化 pending pool。

### 阶段六：后续可选的 Hilbert 生命周期限制

本次不修改 Hilbert 的 Update 接口。若未来需要从服务端强制保证“创建后不可修改”，再单独在 Hilbert 增加生命周期限制和对应测试。

## 11. 验收标准

### 正常无变化轮次

对于已同步且无状态/设备变化的 workspace：

```text
projection_count = 0
pending_pool_count = 0
blocked_count = 0
```

且不产生逐计划 workstation/pending pool 事务。

### 新计划

- 有设备：创建或复用 workstation，并准备正式 task pool；
- 无设备：只落本地计划，不创建正式 task；
- activate 只创建/解析 workstation，并创建异步 Binding Job；
- Binding Worker 逐计划完成设备补绑和 task/pool 准备；
- activate 耗时不随未绑定计划数量线性增长；
- Worker 使用 activate 已确定的 workstation，不在正常路径重复创建 workstation。

### 状态变成 collected

- 本地状态更新；
- pending task 被取消；
- 不创建或复用 workstation；
- 不维护 pending pool。

### 设备补绑

- `NULL -> device_id` 能被 Binding Worker 识别；
- 本地设备字段更新；
- Worker 使用 activate 已确定的 workstation；
- 每个计划按“补绑 -> 本地更新 -> task/pool”顺序完成；
- 正式 pending task pool 正确创建或补齐；
- 单个计划失败不影响其他计划；
- 第一版不要求 Worker 重启后从未完成 item 自动恢复；

### 数据一致性

- 远端消失的计划仍能被本地软删除；
- workspace 之间错误隔离；
- 单个 workspace 超时不影响其他 workspace；
- 第一版允许 Binding Worker 因 Keystone 重启而丢失未完成任务，后续通过重新 activate 或 repair 触发；
- 本次以生产业务约定的“计划创建后配置不变”为前提，不处理 Hilbert 侧配置修改。

## 12. 最终方案摘要

最终同步模型：

```text
每 5 分钟：
    全量拉取 Hilbert dc_plan 快照
        -> 按 plan ID 做轻量本地比较
        -> 新计划：初始化投影
        -> status 变化：只处理状态相关动作
        -> NULL -> device_id：补绑并初始化投影
        -> 只有进度变化：更新镜像，不重做 workstation projection
        -> 无变化：不做下游操作
        -> 远端消失：软删除
```

未指定设备的计划：

```text
同步阶段只保存 dc_plan，不创建正式 pending task；
activate 只创建/解析并激活 workstation，同时创建 Binding Job；
Binding Worker 逐计划补绑设备；
每个计划补绑成功后，使用已有 workstation 创建该计划 task/pool。
```

这套方案在保持最终一致性和现有设备补绑业务的前提下，能够避免每轮重复处理已经稳定的数采计划，也避免 activate 被 100 次串行 Hilbert 调用阻塞。
