<!--
SPDX-FileCopyrightText: 2026 ArcheBase

SPDX-License-Identifier: MulanPSL-2.0
-->

# Synapse 生产大屏任务拆解

**Scope:** Keystone `worktree2` 与 Synapse `worktree2` 后续开发任务

## 1. 目的

本文档把 `synapse-production-big-screen.md` 中的设计和技术方案拆成可执行开发任务。它不重复设计愿景，只定义开发顺序、依赖关系、修改范围、验收标准和推荐 PR 切分。

后续开发应同时参考：

- `docs/designs/synapse-production-big-screen.md`
- 本任务拆解文档

## 2. 拆分原则

- 先数据契约，再前端数据层，再页面布局，最后动画和验收。
- 每个任务尽量能独立验证，避免一个 PR 同时大改后端、数据层、布局和动画。
- 第一版以 MVP 为目标，不新增告警表，不做独立告警栏，不做视频转码服务，不引入 WebSocket 和重型动画库。
- 前端优先消费聚合 API；只有 API 不可用或字段暂缺时使用集中 fallback。
- 改动优先复用现有文件和模式，避免新建过多平行体系。

## 3. 推荐 PR 切分

| PR | 任务 | 仓库 | 是否阻塞后续 | 目标 |
|---|---|---|---|---|
| PR-1 | 后端 overview 聚合 API | `keystone-worktree2` | 是 | 建立大屏数据契约 |
| PR-2 | 前端 overview 数据层 | `synapse-worktree2` | 是 | 建立 API client、adapter、fallback |
| PR-3 | 大屏响应式基础布局 | `synapse-worktree2` | 是 | 页面能展示完整数据区域 |
| PR-4 | Video Flight Stage MVP | `synapse-worktree2` | 否 | 实现预览轮播状态机和 fallback |
| PR-5 | 图表、设备、任务流打磨 | `synapse-worktree2` | 否 | 补齐生产指挥中心信息密度 |
| PR-6 | 电视视口和稳定性验收 | `synapse-worktree2`，必要时 `keystone-worktree2` | 否 | 修正响应式、性能和边界问题 |

如果需要更快出第一版展示，PR-3 可以先接 mock/fallback 数据并与 PR-2 并行，但最终必须回到 overview API 契约。

## 4. 任务清单

### T1. 后端 overview API 契约与基础响应

**仓库:** `keystone-worktree2`

**目标:** 在现有 `ProductionDashboardHandler` 中新增面向大屏的 `GET /api/v1/production/dashboard/overview`。

**依赖:** 无。

**建议修改范围:**

- `internal/api/handlers/production_dashboard.go`
- `internal/api/handlers/production_dashboard_test.go`
- `docs/docs.go`、`docs/swagger.*`，如果项目要求同步 Swagger

**实现要点:**

- 复用现有 `resolveProductionDashboardScope()`、查询参数解析、只读事务和 scope 过滤。
- 新增 overview response structs。
- 第一版返回完整结构，即使部分数组为空。
- `summary`、`trend`、`task_status_distribution`、`devices`、`stations`、`recent_tasks` 优先实现。
- 第一版不返回独立 `alerts` 字段，不建设告警栏；设备不在线、工位离线、任务失败、接口降级等异常内联展示在顶部状态、设备/工位状态和任务流中。
- `previews.video_url` 可以为空，但如果非空必须是真实可播放视频 URL；为空时返回 episode/task 元信息和可解析的 `preview_url`，保证前端可复用数据预览页播放 MCAP 图像帧。

**验收标准:**

- `GET /api/v1/production/dashboard/overview` 需要 JWT，允许 `admin` 和 `data_collector`。
- `data_collector` 自动限定到绑定工位。
- 空数据返回 200，数值为 0，数组为空。
- 参数错误返回 400，未认证返回 401。
- 响应包含 `generated_at`、`scope`、`summary`、`trend`、`task_status_distribution`、`quality`、`devices`、`stations`、`recent_tasks`、`previews`。
- `previews.video_url` 为空时仍有 `title`、`task_name`、`robot_name`、`station_name`、`status`、`created_at`；有 MCAP 时返回 `preview_url`；`video_url` 非空时必须是真实可播放视频。

**验证命令:**

```bash
go test ./internal/api/handlers/... -run ProductionDashboard -v
gofmt -w internal/api/handlers/production_dashboard.go internal/api/handlers/production_dashboard_test.go
```

**风险:**

- 今日任务和全量任务口径容易混淆；接口字段名必须明确。
- 设备在线率需要确认规则，第一版可以沿用 workstation status。

### T2. 后端 overview 查询补齐

**仓库:** `keystone-worktree2`

**目标:** 在 T1 基础上补齐更有展示价值的字段，不引入新表。

**依赖:** T1。

**建议修改范围:**

- `internal/api/handlers/production_dashboard.go`
- `internal/api/handlers/production_dashboard_test.go`

**实现要点:**

- overview `trend` 改为数据生产数量趋势，按 `episodes` 数据生产记录聚合，不再使用任务状态桶作为趋势口径。
- `devices` 只返回在线/不在线汇总，不返回 `items`；设备在线以 recorder hub 与 transfer hub 均联通为准。
- `stations.summary` 返回工位管理状态汇总：`active` 执行中、`inactive` 待命中、`break` 休息中、`offline` 离线。
- `recent_tasks` 覆盖最近完成、失败、进行中的任务。
- `quality.recent_failures` 如果查询成本可控则实现，否则保留空数组。

**验收标准:**

- overview 中 `trend` 每个点包含 `date`、`total`，`total` 表示当天数据生产记录数量。
- `recent_tasks` 按最近更新时间倒序，受 `recent_limit` 限制。
- 所有 limit 参数有上限，避免大屏接口返回过大。

**验证命令:**

```bash
go test ./internal/api/handlers/... -run ProductionDashboard -v
go test ./... ./cmd/keystone-edge/...
```

**风险:**

- `sync_logs` 或 episode join 较复杂时，异常状态可以先只通过任务流和设备状态表达，避免阻塞前端。

### T3. 前端 overview API client 与数据 adapter

**仓库:** `synapse-worktree2`

**目标:** 增加前端大屏数据入口，优先消费 overview API。

**依赖:** T1，或临时使用 mock 契约并在 T1 合入后对齐。

**建议修改范围:**

- `src/api/productionDashboard.js`
- `src/features/production/useProductionBigScreenData.js`
- 可选：`src/features/production/productionBigScreenMock.js`

**实现要点:**

- `useProductionDashboardApi()` 增加 `overview(params)`。
- 新增 composable 管理 `loading`、`error`、`lastUpdatedAt`、`usingFallback`、`overview`。
- adapter 统一补默认值，避免模板写大量空值判断。
- API 失败时保留上一份成功数据；无历史数据时使用集中 mock fallback。
- 定时刷新默认 15s 或 30s，后续可配置。

**验收标准:**

- API 成功、失败、空数据均返回稳定的数据结构。
- fallback 数据集中定义，不散落在 Vue 模板。
- composable `onUnmounted` 时清理 polling timer。
- 不打断 Video Flight Stage 当前播放状态，数据层只更新队列数据。

**验证命令:**

```bash
npm run build
```

**风险:**

- 真实 API 字段和 mock 字段不一致会造成返工；adapter 必须成为唯一入口。

### T4. 大屏响应式基础布局

**仓库:** `synapse-worktree2`

**目标:** 改造 `/admin/dashboard` 对应页面，建立生产指挥中心基础布局。此阶段先不追求复杂视频动画。

**依赖:** T3。

**建议修改范围:**

- `src/views/FullscreenDashboard.vue`
- 可选：`src/components/production/BigScreenKpiStrip.vue`
- 可选：`src/components/production/BigScreenStatusRail.vue`
- 可选：`src/components/production/BigScreenTaskFeed.vue`
- 可选：`src/components/production/BigScreenTrendPanel.vue`
- `src/styles/dashboard-light.css` 或新建大屏专用 CSS 文件

**实现要点:**

- 保留 `/admin/dashboard` 路由。
- 建立顶部状态栏、KPI、中央舞台占位、趋势、设备状态和任务流；不设置独立告警栏。
- 使用 CSS Grid、Flexbox、`clamp()`、`minmax()`、`aspect-ratio`。
- 核心 KPI 和视频舞台在所有断点优先展示。
- `1366x768` 下可减少列表条数，但不能横向滚动或文字重叠。

**验收标准:**

- `1920x1080` 下首屏能看到 KPI、中央舞台、设备健康和异常状态。
- `3840x2160` 下内容不显得过小或过空。
- `1366x768` 下不横向滚动，不出现明显文字重叠。
- 没有视频数据时中央舞台显示稳定占位。

**验证命令:**

```bash
npm run build
```

**手动检查视口:**

- `3840x2160`
- `2560x1440`
- `1920x1080`
- `1600x900`
- `1366x768`

**风险:**

- 如果一次拆太多组件，会增加 PR 审查成本；优先按职责拆少量组件。

### T5. Video Flight Stage MVP

**仓库:** `synapse-worktree2`

**目标:** 实现视频/预览轮播舞台状态机和克制飞行动效。

**依赖:** T3、T4。

**建议修改范围:**

- `src/components/production/VideoFlightStage.vue`
- `src/views/FullscreenDashboard.vue`
- 大屏相关 CSS

**实现要点:**

- 状态机：`loading`、`entering`、`playing`、`leaving`、`error`。
- 只播放当前条目；带真实 `video_url` 时使用 `<video>`，无 `video_url` 但有 `preview_url` 时复用数据预览页 MCAP reader 播放图像帧。
- 轮播按媒体槽位实现：`preview item` 只代表业务数据，`media slot` 才持有 `<video>`、MCAP reader、object URL 和播放时间轴。
- 当前条进入播放后，只准备队列中的下一条，不做多条前瞻预载，也不维护播放器池。
- MCAP 正常切换时，旧 current 如果仍在轮播队列中，可以进入有上限的 warm handoff cache；下一次成为 next 时按稳定 `media_identity` 直接复用，避免短队列反复重新 presign、读取 metadata 和起播帧。
- next slot 生命周期为 `idle -> preparing -> armed -> activating -> current`；`activating` 只能交换槽位所有权，不能重新设置 `src`、不能 `load()`、不能 seek 到 0。
- next 的 armed 条件必须明确：真实视频完成 metadata 和首帧准备，并具备可平滑起播的缓冲或触发 `canplaythrough`；MCAP 完成 presign、metadata、图像 topic 选择和有上限的起播帧窗口准备；poster 图片完成 `load`。
- 使用稳定 `media_identity` 判断资源是否变化；不能把带签名、过期时间、token 或随机数的完整访问 URL 当作媒体身份。
- overview 刷新时，current `media_identity` 未变化不得重置当前播放；next `media_identity` 未变化继续保留准备状态，变化时取消旧 next 并准备新 next。
- 当前条播放结束后先检查 next ready 状态；若未 ready，current 停在最后一帧或最后一张 MCAP 帧，不清空舞台。
- 从 current 结束起最多等待 next 10 秒；10 秒内 next ready 则切换，10 秒后仍未 ready 则从头重播 current，并继续准备同一个 next。
- 真实视频 next 只能静默预载，不能调用 `play()`，不能产生声音；切换后直接激活已准备好的 media slot 播放。
- `replay current` 是唯一允许 seek 到 0 的路径，只能用于 next 等待 10 秒仍未 armed 后的当前条重播。
- MCAP next 准备阶段只读取 metadata 和有上限的起播帧窗口，armed 后可后台补齐剩余有限帧；warm handoff cache 也必须有数量上限，不得无上限缓存大量帧。
- MCAP 播放不得用取模循环反复播放开头帧；到达当前已准备帧序列末尾时，如果补齐仍在进行则停最后一帧等待，补齐失败或等待超时后进入 next 检查。
- MCAP 播放速度按 image message 的录制 timestamp 驱动，优先使用 `logTime` 计算相邻帧延迟；`duration_seconds / frame_count` 只能作为 timestamp 不可用时的 fallback。
- MCAP timestamp 在前端必须以字符串或 `BigInt` 保存，避免 JavaScript `Number` 丢失纳秒级精度；timestamp 缺失、重复、倒退或异常 gap 需要有保守 fallback 和最小/最大延迟 clamp。
- `video_url` 缺失且 MCAP 不可用时才使用 `poster_url` 或轻量等待状态，主舞台不显示大段任务文字。
- `video_url` 非空时必须是真实可播放视频 URL，不能使用 MCAP presigned URL、poster、内部对象路径或 mock URL 冒充。
- 动画使用 `transform`、`opacity`、低强度 `filter` 和 `perspective`。
- 支持 `prefers-reduced-motion`，小屏或 reduced motion 下使用淡入淡出。
- 舞台固定 `aspect-ratio`，动画不改变周边布局。

**验收标准:**

- 有真实 `video_url` 时可以飞入、播放、结束后飞出并切换下一条。
- 无 `video_url` 但有 `preview_url` 时可以自动播放 MCAP 图像帧；不渲染 `<video>`，但舞台必须有真实视觉内容。
- MCAP 图像帧播放速度应接近录制速度；不能因为 episode `duration_seconds` 或固定 fps 让正常数据明显加速/减速。
- 从第二条开始，当前条播放期间应已经开始准备 next media slot；网络正常时不应出现明显黑屏、长时间加载或大段文字占位。
- 下一条未 ready 时不应强切空白舞台，应保留当前最后画面最多 10 秒；仍未 ready 时重播当前条。
- 已 armed 的真实视频 next 激活时不应重复播放开头帧；激活过程不得重建播放器、重设 `src` 或主动 seek 到 0。
- 预载失败不影响当前播放；轮到该条时进入正常播放/降级链路。
- 视频错误进入 `error` fallback，并继续后续轮播。
- 数据刷新不重置当前播放状态。
- 组件卸载时清理 timer、current/next video listeners、hidden next video、current/next/warm cache MCAP reader 相关消息状态和 object URL。

**验证命令:**

```bash
npm run build
```

**风险:**

- 电视浏览器自动播放限制；第一版只对真实视频默认 muted autoplay，失败时优先降级到 MCAP 图像帧，再退回 poster / 轻量等待状态。

### T6. 图表、设备、任务流打磨

**仓库:** `synapse-worktree2`

**目标:** 将大屏从“可展示布局”打磨为有信息密度的生产指挥中心。

**依赖:** T3、T4，可与 T5 部分并行。

**建议修改范围:**

- `src/components/production/BigScreenTrendPanel.vue`
- `src/components/production/BigScreenStatusRail.vue`
- `src/components/production/BigScreenTaskFeed.vue`
- `src/features/production/chartTheme.js`
- 大屏相关 CSS

**实现要点:**

- 趋势区域对齐后台“数据生产统计”页面的趋势分析：使用 ECharts，固定近 7 天，时间粒度为天，默认展示每日数据生产“总数量”柱状图。
- 趋势数据需要补齐 7 个日期桶；API 缺失某一天时显示 0，避免布局和横轴随数据稀疏程度变化。
- 设备状态只清楚区分在线、不在线；不再表达 busy、idle、abnormal 等生产状态。
- 工位状态按后台工位管理状态表达：`active` 执行中、`inactive` 待命中、`break` 休息中、`offline` 离线。
- 设备不在线和工位离线使用灰色，不使用红色；红色只保留给任务失败、质检失败等生产异常。
- 设备状态区不再渲染具体设备、工位、机器人、操作员或当前任务明细；改为设备状态与工位状态摘要、在线率、状态分布条和抽象状态矩阵。
- overview 不再返回 `devices.items` 或 `stations.items`；UI 使用 `devices.summary` 与 `stations.summary` 计算摘要和矩阵数量。
- 可以为设备/工位状态加入克制 CSS 动效：执行中状态灯低幅度呼吸、状态分布条宽度过渡、6 到 8 秒一次的淡巡检扫描、刷新成功短脉冲；必须支持 `prefers-reduced-motion`。
- 禁止大面积霓虹、持续快速闪烁、跑马灯、雷达式强动效，以及任何会改变 grid track、面板高度或造成滚动的动画。
- 任务流显示最近任务，不做后台表格。
- 异常状态内联展示在顶部状态、设备状态和任务流中，不新增告警栏。
- 图表和列表不抢中央舞台视觉中心。
- 本轮一屏化改造将 `/admin/dashboard` 作为沉浸式大屏路由处理，隐藏常规后台侧栏，页面根容器使用 `100dvh` 锁高，并让外层内容区 `overflow: hidden`；不能通过页面纵向滚动或 `overflow-y: auto` 兜底。
- 大屏主体采用三行布局：顶部状态栏、KPI 条、主驾驶舱 grid。主驾驶舱再分为设备状态、中央 Video Flight Stage + 趋势、最近任务流三列，所有列都使用 `minmax(0, 1fr)` 和 `min-height: 0` 允许内部压缩。
- `1366x768` 与 `1600x900` 下优先保留顶部状态、8 个 KPI、Video Flight Stage、趋势、设备状态和最近任务流；设备/任务列表可减少条目，质量指标压缩为任务流内的窄条，不再占用独立面板。
- 70 寸电视或浏览器页面缩放放大时，按 125% 到 165% 页面缩放后的等效视口兜底验证，例如 `1536x864`、`1280x720`、`1152x648`；这些尺寸仍应保持三列驾驶舱，不应触发普通后台或移动端纵向堆叠。
- 顶部状态栏在紧凑和缩放等效视口下仍保留完整日期时间 `YYYY/MM/DD HH:mm:ss` 与全屏按钮；不得改成短时间、隐藏时间、隐藏按钮，或把按钮缩小到远距离不可用。
- 趋势区域不再使用 completed / in_progress / pending / failed 堆叠条；本轮改为与数据生产统计一致的近七天按天 ECharts 数量趋势，但不带统计页的 tab、筛选器或 dataZoom。
- 空数据、API 失败和 fallback 数据仍复用 `useProductionBigScreenData.js` 的稳定结构；布局高度不因错误提示、空状态或刷新按钮出现而变化。

**验收标准:**

- `trend`、`devices`、`stations`、`recent_tasks` 和质量/异常状态均能正常展示。
- 设备/工位状态区只展示设备和工位聚合状态，不出现具体设备名、工位名、机器人名、操作员或当前任务明细。
- 设备/工位状态动画在默认模式下克制可见，在 `prefers-reduced-motion` 下静态或近似静态。
- 图表 resize 正常，组件卸载时 dispose。
- 任务流和设备列表数量受限，不造成小屏溢出。
- API 失败时有可见降级提示。
- `3840x2160`、`2560x1440`、`1920x1080`、`1600x900`、`1366x768` 横屏视口下，document/body 不出现纵向滚动，dashboard 根容器高度不超过 viewport，核心区域不重叠。
- `1366x768`、`1280x720`、`1152x648` 下顶部完整日期时间和全屏按钮均可见，且不触发横向滚动。

**验证命令:**

```bash
npm run build
```

**风险:**

- ECharts 在容器尺寸变化时可能计算错误；需要在 mounted、nextTick、resize 后统一 resize。

### T7. 电视视口与性能验收

**仓库:** `synapse-worktree2`，必要时 `keystone-worktree2`

**目标:** 对大屏在目标视口和长时间运行场景下做收尾修正。

**依赖:** T4、T5、T6。

**建议修改范围:**

- 大屏相关 Vue/CSS
- `useProductionBigScreenData.js`
- 后端 overview limit 或字段，如果发现接口返回过大

**实现要点:**

- 检查目标视口：`3840x2160`、`2560x1440`、`1920x1080`、`1600x900`、`1366x768`。
- 额外检查页面缩放后的等效视口：`1280x720`、`1152x648`；完整日期时间和全屏按钮必须可见。
- 检查横向滚动、文字重叠、视频舞台被挤压、图表空白。
- 检查 timer、resize listener、fullscreen listener、chart instance、current/next video listeners、hidden next video、MCAP reader 和 object URL 的释放。
- 检查轮播资源上限：连续运行时只保留 current + next，hidden video / MCAP reader / object URL 数量不随轮播无限增长。
- 检查下一条未 ready 场景：不应黑屏离场，应冻结当前最后画面最多 10 秒；仍未 ready 时重播当前条。
- 检查 API 失败、空数据、真实视频失败、无视频 URL。
- 必要时减少小屏列表条数或降低动画强度。

**验收标准:**

- 所有目标视口无明显布局问题。
- `npm run build` 通过。
- 页面刷新和轮播不会造成明显闪烁。
- 长时间运行没有明显内存泄漏风险点。

**验证命令:**

```bash
npm run build
```

**风险:**

- 不同电视浏览器对 CSS filter、autoplay、fullscreen 支持不同；需要保留降级路径。

## 5. 跨任务依赖

```text
T1 -> T3 -> T4 -> T5 -> T7
T1 -> T2 -> T3
T4 -> T6 -> T7
```

说明：

- T1 是前端稳定接入的基础。
- T2 可以在 T3 后继续增强，只要 overview response 结构稳定。
- T4 可以先用 T3 fallback 数据推进，但合入前应对齐真实 overview。
- T5 和 T6 可以并行，但都依赖 T4 的基础布局区域。

## 6. 第一版 MVP 定义

第一版 MVP 至少完成：

- T1：后端 overview API 基础响应
- T3：前端 overview 数据层和 fallback
- T4：大屏响应式基础布局
- T5：Video Flight Stage MVP

MVP 可以暂缓：

- T2 中复杂 `quality.recent_failures` 查询
- T6 中更细的图表和信息密度打磨
- T7 中非基础视口的细节优化

MVP 不应暂缓：

- API 失败 fallback
- 无真实视频时的 MCAP 图像帧播放与 poster fallback，且不能伪装成 `<video>` 播放
- `1920x1080` 与 `1366x768` 的基本可读性
- timer/listener/chart/video cleanup

## 7. 后续开发提示词建议

后续让编码助手开发时，建议按任务编号发起，例如：

```text
请根据 docs/designs/synapse-production-big-screen.md 和
docs/designs/synapse-production-big-screen-tasks.md，先实现 T1。
只修改 keystone-worktree2，新增 /production/dashboard/overview 聚合 API。
不要实现前端，不要新增告警表，不要实现告警栏，不要做视频转码服务。
完成后运行 gofmt 和受影响 handler 测试。
```

每次只实现 1 到 2 个任务，避免一个 PR 过大。

## 8. 待确认后再开发的问题

这些问题不阻塞 T1/T3/T4，但会影响后续打磨：

- `video_url` 真实来源是否存在；若不存在，第一版必须使用 `preview_url` 的 MCAP 图像帧播放，不得伪造 `<video>` 播放。
- 如果后续要细分“部分联通”，需确认 recorder-only、transfer-only 是否单独展示；当前大屏只展示两个 hub 均在线的设备在线率。
- 大屏是否允许 data_collector 访问，还是只给 admin/只读展示账号。
- 生产现场默认刷新频率。
- 异常状态在顶部状态、设备状态和任务流中的展示阈值。
- 是否需要参观模式脱敏任务 ID、设备 ID 或客户信息。
