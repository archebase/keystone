<!--
SPDX-FileCopyrightText: 2026 ArcheBase

SPDX-License-Identifier: MulanPSL-2.0
-->

# Synapse 生产大屏设计与技术方案

**Scope:** Keystone production-dashboard 聚合 API 与 Synapse 生产大屏页面

## 1. 背景与目标

Synapse 生产大屏用于机器人数据生产现场的常驻展示，也用于客户参观和管理层查看生产状态。页面运行在电视或大屏显示器上，核心目标设备是 100 寸横屏电视，但不能只为单一尺寸写死布局；它还需要支持 55、65、75、85 寸电视以及常见桌面大屏。

这不是营销页、静态海报、普通后台 dashboard，也不是对现有后台页面做简单换色。它应该是一个真实运行的生产指挥中心：

- 一眼看到今日生产状态、任务进展、设备在线情况、质检结果和异常状态。
- 远距离可以读清核心 KPI、状态和异常。
- 有客户参观时的展示价值，但动效必须服务生产数据展示。
- 页面可以长期运行，定时刷新稳定，不因为数据刷新或视频轮播造成抖动。
- 前端优先消费后端聚合 API，避免分散调用多个 CRUD 接口后在页面里拼数据。

当前代码现状：

- Keystone 已有 `internal/api/handlers/production_dashboard.go`，注册在 `/api/v1/production/dashboard`，提供 `GET /snapshot` 和 `GET /batches/:id/task-summary`。
- Synapse 已有 `src/api/productionDashboard.js`，封装 `/production/dashboard/snapshot` 和批次任务摘要。
- Synapse 已有 `src/features/production/useDashboardData.js`，将 snapshot 映射为统计、趋势、质量、工位、活跃批次和活跃任务。
- Synapse 管理端 `/admin/dashboard` 当前使用 `src/views/FullscreenDashboard.vue`，普通生产页使用 `src/views/ProductionDashboard.vue`。
- 数据预览能力目前主要来自 `episodes` 相关 API 和 MCAP 预览页面，包括 `GET /api/v1/episodes/:id/presign`、`src/views/DataPreview.vue` 和 `src/features/inspector/useMcapReader.js`。当前没有面向大屏视频轮播的直接 `video_url` 契约。

### 1.1 第一版落地边界

第一版应先完成“可展示、可运行、可维护”的大屏，不追求一次性建设完整实时指挥系统。

必须做：

- 复用现有 dashboard、episode、task、station、sync 数据，提供一个面向大屏的聚合响应。
- 改造现有 `/admin/dashboard` 对应的大屏页面，而不是另起一套和当前系统割裂的展示应用。
- 实现响应式大屏布局，保证 `1920x1080` 是基础验收尺寸，并兼顾 4K 与较小横屏。
- 实现 Video Flight Stage 的状态机和 fallback；优先播放真实可播放 `video_url`，缺失时复用数据预览页能力从 `preview_url` 读取 MCAP 图像帧并自动轮播，仍不得把 MCAP URL 冒充成 `<video>`。
- 保留 API 失败、视频失败、空数据情况下的稳定展示。

第一版不做：

- 不新增告警表、告警确认流、复杂告警闭环或独立告警栏。
- 不建设 MCAP 转 MP4、服务端抽帧、缩略图生成等视频加工服务；只预留 `video_url` / `poster_url` / `preview_url` 契约，并在前端复用现有数据预览读取能力。
- 不使用 mock URL、MCAP URL、poster 或静态占位冒充 `video_url`；`video_url` 非空时必须是真实可播放的视频资源。
- 不引入 WebSocket 实时推送，先使用定时刷新聚合 API。
- 不引入 Three.js、复杂 3D 场景或重型动画库。
- 不重做全站设计系统，只在大屏范围内定义必要的大屏样式。
- 不把大屏做成可配置 BI 或报表搭建器。

## 2. 用户与观看场景

### 2.1 观看者

| 观看者 | 关注点 | 大屏表达重点 |
|---|---|---|
| 管理者 | 今日产能、完成率、失败率、设备利用情况 | KPI、趋势、异常设备 |
| 客户 | 生产过程是否专业可信，机器人数据生产是否可视化 | 中央视频舞台、生产流、整体视觉品质 |
| 现场操作人员 | 当前任务、工位状态、设备异常 | 任务流、设备状态 |
| 运维或生产负责人 | 系统是否稳定，失败是否需要处理 | 同步状态、失败任务、设备不在线、刷新时间 |

### 2.2 设备与观看距离

目标视口必须覆盖：

- `3840x2160`：4K 大屏，内容要自然扩展，不能显得太小或过空。
- `2560x1440`：中高分辨率电视或显示器，保持完整大屏体验。
- `1920x1080`：基础验收尺寸，页面必须完整、美观、无遮挡。
- `1600x900`：紧凑横屏，信息密度降低但核心内容保留。
- `1366x768`：小电视或窗口化调试场景，不重叠、不横向滚动、不丢核心 KPI 与视频舞台。

观看距离越远，越要优先显示高对比、强层级、少文字的核心信息。小尺寸电视可以压缩次要列表数量，但不能移除核心 KPI、视频舞台、设备健康和异常状态。

## 3. 页面信息架构

建议页面围绕“顶部状态栏 + KPI + 中央 Video Flight Stage + 两侧/底部运营面板”组织。

### 3.1 顶部状态栏

职责：建立页面身份、系统状态和数据新鲜度。

内容：

- `Synapse 生产指挥中心`
- 当前日期时间，秒级更新
- 系统状态：运行中、数据同步中、接口异常、降级展示
- 在线设备摘要：机器人在线数 / 总数，工位在线数 / 总数
- 今日生产摘要：今日任务、完成、失败
- 数据刷新时间：`generated_at` 或前端最后成功刷新时间

顶部状态栏在紧凑电视和浏览器页面缩放后的等效视口中仍必须保留完整日期时间与全屏按钮。不能为了压缩布局改成只显示 `HH:mm`、隐藏时间、隐藏全屏按钮，或缩小到远距离不可读；应通过 grid 右侧 `auto` 列、压缩中间摘要、减少次要文案来保证可见性。

### 3.2 核心 KPI

职责：远距离快速读数。

首屏必须展示：

- 今日任务数
- 已完成任务数
- 进行中任务数
- 待处理任务数
- 失败任务数
- 合格率
- 机器人在线率
- 活跃批次数或活跃工位数

KPI 文案应短，数字应使用 tabular numbers。数字刷新可以轻微计数，但刷新不得导致卡片尺寸变化。

### 3.3 中央 Video Flight Stage

职责：成为页面视觉中心，展示生产数据预览。

内容：

- 视频/预览轮播舞台
- 当前预览的任务名称、机器人、工位、状态、时间
- episode/task 标识
- 加载、真实视频播放、MCAP 图像帧预览、错误、空数据 fallback

舞台不应只是普通视频播放器。它需要用克制的空间动效表达“生产数据流正在进入系统”。当没有真实 `video_url` 时，舞台优先读取 `preview_url` 对应的 MCAP 图像/压缩图像 topic，按帧自动播放；只有 MCAP 不可用时才退回 poster 或轻量空状态，避免在主舞台显示大段 episode/task 文本。

### 3.4 生产趋势

职责：展示数据生产产能变化。

大屏第一版趋势区域应对齐后台“数据生产统计”页面中的“趋势分析”视觉和口径：

- 固定展示近 7 天数据。
- 时间粒度固定为天。
- 默认展示数量趋势，使用 `trend` 每日数据生产记录合计作为“总数量”；口径与后台数据生产统计一致，基于 `episodes`，不是任务数。
- 每天的数据桶必须补齐；缺失日期显示 0，避免图表长度随 API 数据稀疏程度变化。
- 使用 ECharts 柱状趋势图，复用后台统计页的初始化、`setOption`、resize 和 dispose 模式。
- 大屏趋势区域空间有限，不显示趋势 tab、筛选器或 dataZoom；只保留清晰的日期横轴、数量纵轴、总数量柱状图和简短标题。

### 3.5 设备状态

职责：快速发现设备与工位整体是否健康、是否有可用产能、是否存在异常。

大屏上不展示具体设备、工位、机器人或操作员条目。电视观看距离下，逐条列表的信息价值低，且会挤占中央舞台和趋势区域。设备状态区应从“明细列表”改为“设备与工位健康态摘要”。

内容：

- 设备整体只表达在线状态：在线 / 不在线，以及设备在线率。
- 工位整体按后台工位管理状态表达：执行中 / 待命中 / 休息中 / 离线。
- 设备在线率、工位在线率、执行中工位数量、待命/休息/离线工位数量。
- 设备状态分布条只使用在线 / 不在线两段；工位状态分布条使用 active / inactive / break / offline 语义色。
- 抽象状态矩阵或状态灯阵：每个格子只表达一个设备或工位状态，不显示名称、ID、机器人名、操作员或当前任务。

状态颜色应明确但不刺眼。任务失败、质检失败等生产异常要醒目，健康状态要稳定。设备不在线与工位离线用灰色表达资源不可用，相关信息仍在设备/工位状态区内联表达，但不新增独立告警栏。

overview 不再返回设备或工位明细 `items`。前端只能使用 `devices.summary` 与 `stations.summary` 构建设备状态、工位状态摘要、分布条和抽象矩阵，避免大屏泄露或挤占空间展示具体设备、工位、机器人、操作员或当前任务明细。

### 3.6 最近任务流

职责：呈现生产现场“正在发生什么”。

内容：

- 最近开始、完成、失败的任务
- 当前批次任务
- 任务状态、机器人、工位、批次、耗时

任务流应避免普通后台表格感，建议使用紧凑事件流或生产流水线样式，但不要使用贯穿列表的装饰性竖线或时间轴线。大屏可展示条数很少，筛选和排序必须由后端在完整任务集合上完成，前端只按 API 返回顺序展示和裁剪。

候选集与裁剪策略：

- 后端按最近更新时间返回候选任务：`updated_at` 倒序，再按任务主键倒序；`updated_at` 可继续使用 `COALESCE(t.updated_at, t.completed_at, t.started_at, t.assigned_at, t.created_at)` 兜底，保证状态变化后的任务会进入最近任务流。
- 后端不再按状态优先级置顶 `failed` 或 `cancelled`；失败和取消只作为最近变化的一种状态展示，避免旧异常占满任务流。
- 前端不得把接口返回的候选任务全部强行塞进卡片，也不得给任务流开启内部滚动；应根据 `.task-feed` 实际高度、单条任务卡高度和间距计算可见条数。
- 可见条数建议在 3 到 10 条之间夹取；`1920x1080`、`1600x900`、`1366x768`、`1280x720`、`1152x648` 下都应按真实空间自动增减。
- 任务流区域变化时使用 `ResizeObserver` 或等价机制重新计算可见条数，避免 70 寸电视、浏览器缩放或系统缩放后出现空白过大、溢出或遮挡。
- 前端只做容量裁剪，不重新定义业务排序；如果空间只能显示 5 条，就展示 API 顺序中的前 5 条。

任务生命周期轨：

- 每张任务卡应使用四节点生命周期轨表达 `pending -> ready -> in_progress -> completed`，用于体现任务从待处理到完成的过程。
- `pending` 点亮第 1 节点，`ready` 点亮第 1、2 节点，`in_progress` 点亮第 1 到 3 节点且当前节点低频呼吸，`completed` 点亮全部 4 个节点。
- `failed` 和 `cancelled` 不进入置顶排序；卡片状态文案仍显示失败/已取消，生命周期轨使用低饱和异常或取消态，并只做一次短时反馈，不持续闪烁。
- 第一版不要求后端返回状态历史；前端基于当前 `status` 推断生命周期位置。若后续需要精确展示失败/取消发生在哪个阶段，再扩展 `previous_status` 或状态历史字段。

任务流动画应与数据刷新绑定，而不是持续装饰：

- 新进入列表的任务可以有一次短暂入列动画和横向扫描高光。
- 同一任务状态变化时，生命周期轨做一次短时扫光，当前节点做一次脉冲；如果只是 `updated_at` 变化但状态未变，只保留轻量卡片脉冲。
- `in_progress` 生命周期当前节点可以低频呼吸，表达正在执行。
- `completed` 状态变化时完成节点短暂亮起后保持静态；`failed`、`cancelled` 只做一次短脉冲，不持续闪烁。
- 列表重排使用 transform/opacity 平滑移动，不改变卡片高度，不造成页面滚动。
- 必须支持 `prefers-reduced-motion`，减少或关闭入列、脉冲和移动动画。

### 3.7 异常与降级状态

第一版不设置独立告警栏，也不新增告警表。异常状态应内联展示在已有区域中，避免额外占用大屏主布局：

- 顶部状态栏：展示 API 正常、降级展示、数据同步失败等全局状态。
- 设备/工位状态区：直接标记设备不在线、工位离线或其他资源不可用状态。
- 最近任务流：直接突出失败、取消、质检不通过等任务状态。
- 质量区域：展示合格率、待检和失败数量。

没有异常时不额外展示“无告警”面板；相关区域保持正常生产数据展示。

## 4. 视觉设计方向

### 4.1 整体气质

视觉方向：高级、克制、工业生产大屏。

应该体现：

- 机器人生产系统
- 数据可信、可监控
- 现场长期运行稳定
- 客户参观时有吸引力

避免：

- 廉价科幻风
- 过度霓虹
- 紫色模板
- 满屏发光卡片
- 动画抢走数据注意力
- 营销页式 hero 结构

### 4.2 背景与层次

建议背景为深色或沉稳冷灰基底，叠加非常轻的网格、扫描线或工业仪表纹理。背景不能影响文字可读性，不使用大面积发光团块。

层次建议：

- 背景层：低对比工业质感，保持暗部稳定。
- 主舞台层：中央视频区域，拥有最强视觉权重。
- 数据面板层：边栏和底部信息块，边界清晰但不厚重。
- 状态层：异常、刷新、系统状态，用小面积高对比色。

### 4.3 色彩与状态

建议沿用现有品牌蓝和中性色变量，但为大屏定义清晰的状态语义：

| 状态 | 建议语义 | 使用限制 |
|---|---|---|
| 成功 / 在线 / 已完成 | 低饱和绿色 | 只用于正向状态 |
| 进行中 / 主品牌 / 当前焦点 | 品牌蓝 | 用于主舞台边框、当前任务、重要数字 |
| 待处理 / 等待 / 警告 | 琥珀色 | 不要大面积铺满 |
| 失败 / 质量异常 | 红色 | 只用于任务失败、质检失败等生产异常 |
| 设备不在线 / 工位离线 | 灰色 | 表达资源不可用，不使用红色抢占失败告警语义 |
| 中性 / 无数据 | 灰蓝 | 用于背景和非重点信息 |

图表避免默认彩虹色，使用 3 到 5 个语义色即可。

### 4.4 字体与数字

- 标题：中等字重，清晰，不使用装饰字体。
- KPI 数字：大字号、tabular numbers、强对比。
- 辅助信息：短句，避免长段落。
- 任务 ID、设备 ID：等宽字体，支持中间截断或换行。
- 小屏下用 `clamp()` 限制字号范围，不用 `vw` 线性缩放所有文字。

### 4.5 非视频区域动效

除 Video Flight Stage 外，其他区域可以使用克制、低频、生产语义明确的动效。动效必须帮助观看者理解系统状态，不能成为装饰噪音。

设备与工位状态区允许：

- 忙碌 / 生产中状态灯使用低幅度呼吸动画，表达资源正在工作。
- 状态分布条在数据刷新时使用宽度过渡，表达状态占比变化。
- 状态矩阵上使用很淡的巡检扫描线，周期建议 6 到 8 秒，不持续高亮。
- overview 成功刷新时，在标题状态点或面板边缘做一次短促脉冲。
- 异常数量从 0 变为大于 0 时做一次轻微强调；异常状态不持续快速闪烁。

禁止：

- 大面积霓虹发光、持续闪烁、跑马灯、雷达式强动效。
- 每个状态格持续跳动或独立随机动画。
- 动画改变 grid track、面板高度、列表高度或导致页面滚动。
- 无数据或 API 失败时仍播放“生产中”动效。

所有非视频动效必须支持 `prefers-reduced-motion`，降级为静态状态或一次性淡入。

## 5. Video Flight Stage 动画方案

### 5.1 状态机

每个预览条目进入舞台时应经过明确生命周期：

```text
loading -> entering -> playing -> leaving -> loading(next)
                      -> error
```

状态定义：

| 状态 | 含义 | UI 表现 |
|---|---|---|
| `loading` | 当前预览正在准备 | 舞台显示骨架/海报，当前资源准备完成后进入 |
| `entering` | 预览卡片飞入主舞台 | 使用 transform + opacity + perspective |
| `playing` | 真实 `video_url` 视频或 MCAP 图像帧预览落位并播放 | 舞台稳定，元信息显示，周围面板不动；`video_url` 使用 `<video>`，`preview_url` 使用数据预览帧渲染 |
| `leaving` | 当前预览飞出 | 仅在下一条已有可展示内容时离场，下一条接替 |
| `error` | 视频加载或播放失败 | 显示 fallback 卡片、任务信息和错误状态 |

### 5.1.1 Current + Next 轮播模型

第一版不做复杂的多条前瞻预载，也不维护大型播放器池。轮播主路径只管理两个媒体槽位：当前正在展示的 `current slot` 和后台准备的 `next slot`。`preview item` 是业务数据，`media slot` 是真实持有 `<video>`、MCAP reader、object URL 和播放时间轴的资源；两者不能混为一谈。

- 当前条目是唯一允许播放的资源；下一条只能静默准备，不能调用 `play()`，不能产生声音。
- 当前条目进入 `playing` 后，立即开始准备队列中的下一条。
- 对 MCAP 预览，正常切换时旧 current 不应立刻销毁；如果它仍在轮播队列中，可以按稳定 `media_identity` 放入一个很小的 warm handoff cache，下一次成为 next 时直接复用已解析 metadata、reader 和已加载帧。该 cache 只服务当前/下一条交接，不等同于多条前瞻预载，必须有数量上限。
- next slot 生命周期是 `idle -> preparing -> armed -> activating -> current`。`activating` 只交换槽位所有权，不重新设置 `src`，不调用 `load()`，不把媒体时间轴 seek 到 0。
- 真实 `video_url` 的 next：使用一个隐藏 next video，设置 `muted`、`playsinline`、`preload="auto"`；`loadedmetadata` 只能算 metadata ready，`loadeddata` / `canplay` 只能算可开始播放；进入 `armed` 还需要至少具备可平滑起播的缓冲，或浏览器触发 `canplaythrough`。
- `preview_url` / MCAP 的 next：先解析 episode presign，加载 MCAP metadata，自动选择图像/压缩图像 topic，并准备一个有上限的起播帧窗口后进入 `armed`；禁止在 next 准备阶段读取整段 MCAP 或无上限缓存大量帧。
- poster fallback 的 next：图片 `load` 后进入 `armed`。
- next 准备失败只标记该条本轮不可切换，不影响 current 播放；current 重播或 overview 刷新后可以重新尝试。
- overview 刷新时，如果 current 的稳定 `media_identity` 不变，不打断 current；如果 next 的稳定 `media_identity` 变化，取消旧 next 并准备新 next。
- 离开队列、组件卸载或 URL 变化时，必须释放 next video、warm cache 中的 MCAP reader/消息状态、object URL 和 timer。

`media_identity` 必须是稳定媒体身份，不应直接使用可能随签名变化的完整访问 URL。推荐优先级：

1. 后端显式返回 `media_identity` 或对象存储 key。
2. `episode_id + task_id + media_kind + object_key`。
3. 当前端只能拿到 URL 时，使用 `origin + pathname + object/bucket/key` 等稳定参数，忽略签名、过期时间、token、随机数等访问参数。

`video_url`、`preview_url`、`poster_url` 是访问地址，不是媒体身份。签名 URL 刷新不能导致当前视频重建播放器或回到开头。

### 5.1.1.1 槽位激活与重播边界

- `prepare next` 可以设置 next slot 的 `src`、预载 metadata、等待缓冲和首帧。
- `activate next` 只能把已 armed 的 next slot 标记为 current，并开始播放这个已经准备好的媒体；不得重新设置 `src`、不得重新 `load()`、不得主动 `currentTime = 0`。
- `replay current` 是唯一允许 seek 到 0 的路径；它只发生在 current 已经结束且 next 等待 10 秒仍未 armed 时。
- current 正常切到 next 后，旧 current slot 才进入 idle；若它仍在轮播队列中且是 MCAP 资源，可先进入有上限的 warm handoff cache，否则释放资源。
- 数据刷新只更新队列和元信息，不控制 current 的媒体时间轴。
- MCAP 帧预览必须按有限帧序列播放；next armed 后可在后台继续补齐剩余有限帧。播放到当前已准备序列末尾但补齐仍在进行时停在最后一帧等待补齐，补齐失败或等待超时后再进入 next 检查；不允许用取模循环反复播放开头帧。
- MCAP 播放速度应优先使用图像 message 的录制时间戳驱动。前端以相邻帧 `logTime` / timestamp 差值调度下一帧，不使用 `duration_seconds / frame_count` 推导平均帧率作为主路径；时间戳缺失、重复、倒退或异常时才退回保守 fallback 间隔。

### 5.1.2 不间断轮播边界

“不间断”在浏览器和电视端应定义为网络正常、资源格式兼容时切换无明显黑屏和长时间等待，而不是承诺所有网络和浏览器策略下绝对无缝。第一版采用保守切换规则：

- current 播放结束后，先检查 next 是否 ready。
- 如果 next 已 `armed`，current 离场并激活 next slot。
- 如果 next 尚未 ready，current 停在最后一帧或当前最后一张 MCAP 帧，不清空舞台、不显示大段等待文字。
- 从 current 结束时开始等待 next，最多等待 10 秒。
- 10 秒内 next armed，则切换到 next。
- 10 秒后 next 仍未 ready，则从头重播 current，并在重播期间继续准备同一个 next。
- MCAP current 到达末尾后同样停在最后一帧；10 秒后 next 未 ready 时，从第 0 帧重播 current。
- 当队列很短且 MCAP 条目反复出现时，应优先复用上一轮已经 armed 的 MCAP warm cache，避免每轮重新 presign、读取 metadata 和首帧窗口导致 current 重播。
- 数据刷新不得让 current 最后一帧或 current 播放状态失效；只有 current 资源 key 变化、条目离队或当前资源错误时才重置 current。

### 5.2 动画叙事

建议动线：

1. 下一条预览从右侧远处或侧后方出现，初始为 `translate3d(24%, -4%, -120px) rotateY(-10deg) scale(0.86)`。
2. 进入时逐渐清晰，`opacity` 从 0 到 1，`filter: blur()` 归零。
3. 落位后保持稳定 16:9 舞台，不再移动；`video_url` 指向真实可播放视频时自动播放 `<video>`，否则用 `preview_url` 的 MCAP 图像帧自动播放。
4. 播放结束或达到最大展示时长后，向左上或左侧轨道飞出，透明度降低。
5. current 结束后只有 next 已 ready 才离场；next 未 ready 时停在 current 最后一帧，等待最多 10 秒，超时后重播 current。

动画应该表达“数据片段进入生产系统”，不要像游戏抽卡或宣传片。

### 5.3 技术建议

实现时应优先使用：

- `transform: translate3d(...) scale(...) rotateY(...)`
- `opacity`
- `filter` 的低强度 blur 或 brightness
- `perspective` 容器
- `overflow: hidden` 裁剪飞行动画
- CSS transition / keyframes

谨慎使用：

- `will-change`：只在动画元素上使用，不长期挂在大量节点。
- box-shadow：大面积阴影和发光会提高 GPU/合成成本。
- filter：避免高强度、长时间全屏 blur。

禁止：

- 动画过程中改变 grid track、width、height、margin 导致布局重排。
- 同时播放多个视频。
- 为了预备 next 而解码/渲染多个全尺寸视频或大量 MCAP 帧。
- next 未 ready 时强制清空当前舞台或离场。
- 每次刷新数据都重置舞台状态。
- 飞行动画造成横向滚动条。

### 5.4 视频与 fallback

当前系统的真实预览更接近 MCAP 数据预览，而不是现成 MP4 视频。第一版必须明确区分“真实视频播放”和“数据预览帧播放”：

- `video_url`：真实可直接播放的视频 URL，第一优先；非空时必须能被 `<video>` 播放，不能返回 MCAP presigned URL、poster URL、内部对象路径或 mock URL。
- `preview_url`：可预览的数据 URL，例如 episode presign API 或 MCAP storage proxy URL；不是 `<video>` 的 `src`，而是前端复用 `DataPreview.vue` / `useMcapReader.js` 读取图像或压缩图像 topic 后按帧渲染，并按 MCAP message 时间戳恢复录制节奏。
- `poster_url`：静态封面或后端生成缩略图。

MCAP 图像帧播放规则：

- 优先读取每条 image message 的 `logTime` 作为录制时间轴；如果后续确认传感器 `publishTime` 更能代表业务时间线，可以改为优先 `publishTime`、缺失时退回 `logTime`。
- 下一帧延迟等于 `next.timestamp - current.timestamp`，单位从纳秒转换为毫秒；timestamp 必须保持字符串或 `BigInt`，不能用 JavaScript `Number` 存储纳秒级大整数。
- `duration_seconds` 只作为缺失时间戳时的 fallback 信息，不应作为正常播放速度来源。
- 对异常 timestamp 做保护：缺失、重复、倒退时退回 fallback 帧间隔；超长 gap 可以设上限，避免大屏看起来卡死。
- 预载窗口仍有帧数/资源上限；播放到已加载尾部但后续帧仍在补齐时，停在最后一帧等待补齐，不取模回放开头。

fallback 策略：

| 场景 | 显示策略 |
|---|---|
| `video_url` 存在且可播放 | 渲染 `<video>`，静音自动播放，只播放当前条目 |
| `video_url` 不存在但有 `preview_url` | 解析 MCAP，自动选择图像/压缩图像 topic，按帧播放，不渲染 `<video>` |
| `preview_url` 不可用但有 `poster_url` | 显示 poster + 小型任务元信息，不显示大段占位文字 |
| `video_url` 加载失败 | 显示 poster 或工业占位画面 + 错误状态 |
| `preview_url` / `poster_url` 都不存在 | 显示轻量等待状态和小型任务元信息，不伪装成视频 |
| `previews` 为空 | 显示健康空状态：暂无可轮播预览，保留 KPI 和趋势 |
| 自动播放被浏览器限制 | 对真实视频静音重试；若仍失败，优先降级到 MCAP 图像帧，再退回 poster / 轻量等待状态 |

舞台播放卡片的元信息应以任务名、SOP 名、机器人、设备 ID 和必要的播放进度为主；不要在卡片上展示 episode ID、工位名、MCAP topic 名或“数据预览”字样。

## 6. 响应式布局方案

### 6.1 总原则

页面是电视响应式大屏，不是固定 100 寸画布。

必须使用：

- CSS Grid / Flexbox
- `clamp()`
- `minmax()`
- `aspect-ratio`
- 稳定的 grid 区域
- 固定格式组件的最小/最大尺寸约束

避免：

- 只适合 100 寸的固定 px 布局
- 用 `vw` 线性缩放所有字体
- 视频轮播改变舞台尺寸
- 小屏隐藏全部次要信息，只剩视频

优先级：

1. 顶部状态栏
2. 核心 KPI
3. 中央 Video Flight Stage
4. 设备健康 / 异常状态
5. 趋势与任务流
6. 细节列表

### 6.2 断点策略

#### >= 2400px：超宽/4K 大屏

布局：

- 顶部状态栏一行完整展示。
- KPI 采用 8 列或 4x2，数字更大。
- 中央视频舞台占中部 45% 到 55% 宽度。
- 左侧放 KPI 补充和设备状态，右侧放任务流与质量/同步状态。
- 底部放趋势图和状态分布。

要求：

- 内容不要太分散，主舞台仍然是视觉中心。
- 图表和列表显示更多条目，但保持行高稳定。

#### 1600px - 2399px：标准电视大屏

布局：

- 适合作为主验收布局。
- 顶部状态栏保留标题、时间、同步状态和设备摘要。
- KPI 4 列或 6 列自适应换行。
- 中央舞台在上半屏中间，侧栏为设备和任务流。
- 趋势和任务流在下半屏。

要求：

- `1920x1080` 下不需要滚动即可看到核心 KPI、视频、设备健康和异常状态。
- 列表可以减少到 5 到 8 条。

#### 1200px - 1599px：紧凑电视/桌面布局

布局：

- 顶部状态栏压缩：保留标题、时间、刷新状态，摘要合并为短标签。
- 当前日期时间保持完整 `YYYY/MM/DD HH:mm:ss`，全屏按钮保持基础触达尺寸，不在紧凑模式隐藏或降级为短时间。
- KPI 改为 4 列或 2 行。
- 视频舞台占据主要宽度，侧栏移动到视频下方或双列下方。
- 趋势图高度降低，任务流和设备列表减少条数。

要求：

- 不能横向滚动。
- 不能让侧栏挤压视频舞台到不可读。
- 浏览器页面缩放放大后的等效视口，例如 `1280x720`、`1152x648`，仍应显示完整日期时间和全屏按钮；优先压缩中间 telemetry、KPI 行高、列表条数和面板内边距。

#### < 1200px：降级布局

布局：

- 单列或两列紧凑布局。
- KPI 和视频舞台优先。
- 异常状态内联保留在设备和任务区域，视频下方不设置独立告警栏。
- 趋势、设备、任务流可以折叠为紧凑面板或减少展示条数。

要求：

- 不追求完整大屏氛围，但必须可读、可操作、无重叠。
- Video Flight Stage 降级为淡入淡出或轻量滑入。

### 6.3 视频舞台响应式

- 始终设置 `aspect-ratio: 16 / 9` 或接近 16:9。
- 舞台容器设置 `overflow: hidden`。
- 元信息 overlay 使用底部或侧边半透明带，不覆盖视频主体超过 25%。
- 飞入/飞出元素只在舞台容器内移动，不能影响页面滚动。
- 1366x768 下舞台仍应完整可见，不能被任务流或侧栏挤没。

## 7. 后端聚合 API 方案

### 7.1 路由建议

当前已有：

```text
GET /api/v1/production/dashboard/snapshot
GET /api/v1/production/dashboard/batches/:id/task-summary
```

建议新增或演进为：

```text
GET /api/v1/production/dashboard/overview
```

`snapshot` 可以继续服务现有页面，`overview` 面向新版大屏契约。若希望减少路由数量，也可以让 `snapshot` 追加字段，但文档建议使用 `overview`，原因是：

- 大屏需要 `previews`、更明确的 `summary`、设备聚合和最近任务流。
- 新契约可以避免破坏当前 `useDashboardData()` 对 `snapshot` 的映射。
- 后续可以为大屏单独设置 refresh、limit、preview 参数。

第一版实现时不要另起独立业务模块。优先在现有 `ProductionDashboardHandler` 中增加 `overview` handler，复用已有 scope 解析、只读事务、任务统计、趋势、质量和工位查询。只有当 handler 明显膨胀到难以维护时，再拆 service/helper。

### 7.2 查询参数

建议参数：

| 参数 | 默认 | 说明 |
|---|---:|---|
| `timezone_offset` | 当前前端时区 | 趋势聚合使用 |
| `trend_days` | 7 | 趋势窗口，最大 31 |
| `recent_limit` | 10 | 最近任务候选条数；大屏前端按可用高度裁剪 |
| `preview_limit` | 8 | 轮播预览条数 |
| `factory_id` | 空 | 管理端筛选 |
| `organization_id` | 空 | 管理端筛选 |
| `workstation_id` | 空 | 工位筛选；data_collector 角色自动限定 |

权限策略沿用现有 dashboard：`admin` 与 `data_collector` 可访问；`data_collector` 自动限定到绑定工位。

### 7.3 返回结构

```json
{
  "generated_at": "2026-05-17T10:00:00Z",
  "scope": {
    "role": "admin",
    "factory_id": "",
    "organization_id": "",
    "workstation_id": ""
  },
  "summary": {
    "today_task_count": 128,
    "completed_task_count": 92,
    "in_progress_task_count": 8,
    "pending_task_count": 24,
    "failed_task_count": 4,
    "pass_rate": 96.4,
    "robot_online_rate": 87.5,
    "active_batch_count": 6,
    "active_station_count": 14,
    "generated_at": "2026-05-17T10:00:00Z"
  },
  "trend": [
    {
      "date": "05-17",
      "total": 92
    }
  ],
  "task_status_distribution": {
    "completed": 92,
    "in_progress": 8,
    "pending": 18,
    "ready": 6,
    "failed": 4,
    "cancelled": 0
  },
  "quality": {
    "pass_count": 81,
    "fail_count": 3,
    "pass_rate": 96.4,
    "inspection_count": 84,
    "recent_failures": []
  },
  "devices": {
    "summary": {
      "total": 16,
      "online": 14,
      "offline": 2,
      "online_rate": 87.5
    }
  },
  "stations": {
    "summary": {
      "total": 16,
      "online": 14,
      "active": 8,
      "inactive": 4,
      "break": 2,
      "offline": 2,
      "online_rate": 87.5
    }
  },
  "recent_tasks": [],
  "previews": []
}
```

### 7.4 字段来源建议

| 数据块 | 可能来源 | 当前状态 |
|---|---|---|
| `summary.today_task_count` | `tasks` 按今日时间窗口聚合 | 现有 `snapshot.tasks` 是全量 scope 计数，需要增加今日窗口 |
| `summary.completed/in_progress/pending/failed` | `tasks.status` | 现有已支持基础状态 |
| `summary.pass_rate` | `episodes.qa_status` 或现有 `dashboardQuality` | 现有 quality 可复用 |
| `summary.robot_online_rate` | `robots.device_id` 与 recorder/transfer hub 连接快照 | 设备在线率只表达联通状态：两个 hub 均在线才算在线 |
| `trend` | `episodes` 数据生产记录时间桶 | 与数据生产统计页数量趋势一致，按天返回每日总数量 |
| `task_status_distribution` | `tasks.status` | 现有 tasks counts 可复用并扩展 |
| `quality.recent_failures` | `episodes` + `tasks` + `inspectors` | 当前需新增查询 |
| `devices.summary` | `robots` + recorder/transfer hub 连接快照 | 不返回 `items`，仅返回 total / online / offline / online_rate |
| `stations.summary` | `workstations.status` | 使用后台工位管理状态 active / inactive / break / offline |
| `recent_tasks` | `tasks` + `batches` + `workstations` + `robots` | 现有 active_tasks 只覆盖进行中任务，需要最近完成/失败 |
| `previews` | `episodes` + task/workstation metadata + presign/poster | 当前 episode API 可 presign MCAP，但没有视频 URL |

第一版字段取舍：

- `summary`、`trend`、`task_status_distribution`、`devices`、`stations`、`recent_tasks` 是核心字段，应优先完成。
- 第一版不返回独立 `alerts` 字段；任务失败、设备不在线、工位离线、同步降级等异常内联体现在顶部状态、设备/工位状态和任务流中。
- `previews.video_url` 可以为空；但如果非空，必须是真实可播放视频 URL。前端不能把 `preview_url` 当作 `<video>` 源，但应把它作为 MCAP 数据预览源读取图像帧。
- `quality.recent_failures` 如果查询成本高，可以第一期返回空数组，但保留字段。

### 7.5 异常状态表达建议

第一期不需要建立新告警表，也不需要独立告警栏。异常应进入已有信息区域：

- 最近任务流：直接展示失败、取消、质检未通过等任务状态。
- 设备/工位状态区：直接展示设备不在线、工位离线和工位状态异常口径。
- 顶部状态栏：展示 overview 请求失败、保留旧数据、使用 fallback 等全局降级状态。
- 质量区域：通过失败数、待检数和合格率表达质量异常。

后续如需确认、闭环处理或审计记录，再设计独立 alerts 表和告警页面；它不属于生产大屏 MVP。

### 7.6 预览数据契约

第一期 `previews` 应允许没有真实视频，但必须遵守：`video_url` 为空表示没有可直接播放的视频；`video_url` 非空表示真实可播放视频。`preview_url` 表示数据预览源，前端可像数据预览页一样解析 MCAP 图像帧并在舞台播放。

```json
{
  "id": "episode:42",
  "title": "片段 EP-20260517-00042",
  "task_name": "抓取-放置标准动作采集",
  "sop_label": "pick-place@v3",
  "device_id": "AB-F0001-T0003-000001",
  "robot_name": "RB-07",
  "station_name": "A-03 工位",
  "status": "approved",
  "video_url": "",
  "preview_url": "/api/v1/episodes/42/presign?kind=mcap",
  "poster_url": "",
  "duration_seconds": 38,
  "created_at": "2026-05-17T09:52:10Z",
  "episode_id": 42,
  "task_id": 135
}
```

注意：`preview_url` 不应直接返回需要额外鉴权但前端无法读取的内部对象路径，也不能作为 `<video>` 的 `src`。若返回 episode presign API，前端需要先解析成 `/api/v1/storage/object?...` 后交给 MCAP reader。若视频需要从 MCAP 抽帧或转码，应在后续任务中定义生成服务；在真实视频生成前，大屏只做前端数据预览帧播放，不新增服务端视频加工能力。

### 7.7 空数据与错误

- 空 scope 返回 200，数组为空，summary 数值为 0，并携带 `scope.warning`。
- 查询失败返回 500，JSON 仍使用 `gin.H{"error": "failed to get production dashboard overview"}`。
- 参数错误返回 400。
- 未认证返回 401。
- 无权限返回 403，由现有 middleware 控制。

## 8. 前端数据流与组件拆分方案

### 8.1 建议页面入口

建议优先改造 `src/views/FullscreenDashboard.vue` 作为管理端大屏入口，因为当前 `/admin/dashboard` 已指向该页面，且它已经包含全屏按钮、ECharts 和大屏样式。

`src/views/ProductionDashboard.vue` 可以继续作为普通运营 dashboard，或在后续统一为共享组件。

### 8.2 建议组件

| 组件 | 职责 | 资源持有 |
|---|---|---|
| `ProductionBigScreen.vue` 或 `FullscreenDashboard.vue` | 页面总装、布局、刷新状态、路由入口 | 刷新 timer、全局 resize |
| `VideoFlightStage.vue` | 视频轮播状态机、current/next 媒体槽位、fallback | current/next video slot、current/next MCAP reader、stage timer、video event listeners |
| `BigScreenKpiStrip.vue` | KPI 展示、数字格式化、轻量刷新动效 | 可选数字动画 timer |
| `BigScreenStatusRail.vue` | 设备与工位状态摘要、分布条、抽象状态矩阵，不展示具体设备列表 | 可选刷新脉冲 / 巡检扫描 CSS 动画 |
| `BigScreenTaskFeed.vue` | 最近任务流、入列/更新/进行中动画 | 短时动画标记 timer |
| `BigScreenTrendPanel.vue` | ECharts 趋势图 | chart instance、ResizeObserver |
| `useProductionBigScreenData.js` | overview 请求、adapter、mock fallback、刷新调度 | polling timer |

### 8.3 数据流

```text
page mounted
  -> useProductionBigScreenData.init()
  -> GET /production/dashboard/overview
  -> adapter normalizes response
  -> render KPI / stage / trend / devices / tasks
  -> start polling every 15s or 30s
  -> refresh data without resetting VideoFlightStage current item
  -> VideoFlightStage prepares only the next media slot after current starts
  -> component unmounted clears timers, listeners, charts, current/next resources
```

关键原则：

- 数据刷新不得打断当前视频播放，也不得清空 `media_identity` 未变化的 current / next 槽位状态。
- `previews` 更新时，VideoFlightStage 只更新队列，当前 `playing` 条目除非已不存在或出错，否则继续播放。
- `previews` 更新时，如果 next 条目的稳定 `media_identity` 未变化，继续保留 next 的准备状态；如果媒体身份变化，取消旧 next 并准备新的 next。
- API 失败时保留上一份成功数据，并显示降级状态；没有上一份数据时使用集中 mock fallback。
- mock 数据集中放在 composable 或 dedicated mock 文件，不能散落在模板里。

### 8.4 Props 与状态建议

`VideoFlightStage.vue`：

- Props: `items`, `maxPlayMs`, `reducedMotion`
- Emits: `item-change`, `play-error`
- Internal state: `currentIndex`, `stageState`, `currentItem`, `nextItem`, `currentSlot`, `nextSlot`, `nextArmed`, `nextStatus`, `nextError`, `errorMessage`
- Cleanup: timers、current/next video `ended/error/canplay` listeners、单个 hidden next video、current/next MCAP reader、object URL

`BigScreenTrendPanel.vue`：

- Props: `trend`, `distribution`
- Internal state: chart instance
- Cleanup: `chart.dispose()`、ResizeObserver

`useProductionBigScreenData.js`：

- State: `loading`, `error`, `lastUpdatedAt`, `usingFallback`, `overview`
- Methods: `init`, `refresh`, `startPolling`, `stopPolling`
- Adapter: `normalizeOverviewResponse(raw)`

## 9. 数据结构草案

以下结构仅用于文档说明，不要求引入 TypeScript。

```js
/**
 * @typedef {Object} OverviewResponse
 * @property {string} generated_at
 * @property {Object} scope
 * @property {Summary} summary
 * @property {TrendPoint[]} trend
 * @property {Object<string, number>} task_status_distribution
 * @property {QualitySummary} quality
 * @property {DeviceSummary} devices
 * @property {StationSummary} stations
 * @property {RecentTask[]} recent_tasks
 * @property {PreviewItem[]} previews
 */

/**
 * @typedef {Object} Summary
 * @property {number} today_task_count
 * @property {number} completed_task_count
 * @property {number} in_progress_task_count
 * @property {number} pending_task_count
 * @property {number} failed_task_count
 * @property {number} pass_rate
 * @property {number} robot_online_rate
 * @property {number} active_batch_count
 * @property {number} active_station_count
 * @property {string} generated_at
 */

/**
 * @typedef {Object} TrendPoint
 * @property {string} date
 * @property {number} total
 */

/**
 * @typedef {Object} DeviceSummary
 * @property {Object} summary
 * @property {number} summary.total
 * @property {number} summary.online
 * @property {number} summary.offline
 * @property {number} summary.online_rate
 */

/**
 * @typedef {Object} StationSummary
 * @property {Object} summary
 * @property {number} summary.total
 * @property {number} summary.online
 * @property {number} summary.active
 * @property {number} summary.inactive
 * @property {number} summary.break
 * @property {number} summary.offline
 * @property {number} summary.online_rate
 */

/**
 * @typedef {Object} RecentTask
 * @property {string|number} id
 * @property {string} public_id
 * @property {string} name
 * @property {string} status
 * @property {string} robot_name
 * @property {string} station_name
 * @property {string} batch_name
 * @property {string} started_at
 * @property {string} finished_at
 * @property {number} duration_seconds
 */

/**
 * @typedef {Object} PreviewItem
 * @property {string} id
 * @property {string} title
 * @property {string} task_name
 * @property {string} sop_label
 * @property {string} device_id
 * @property {string} robot_name
 * @property {string} station_name
 * @property {string} status
 * @property {string} video_url
 * @property {string} preview_url
 * @property {string} poster_url
 * @property {number} duration_seconds
 * @property {string} created_at
 * @property {string|number} episode_id
 * @property {string|number} task_id
 */

/**
 * @typedef {'loading'|'entering'|'playing'|'leaving'|'error'} VideoStageState
 */
```

## 10. 性能与稳定性方案

页面会在电视浏览器长时间运行，优先稳定。

建议：

- overview API 刷新间隔默认 15s 或 30s；生产现场可配置。
- 如果本轮刷新失败，保留上一轮成功数据，不清空页面。
- 刷新时只更新数据，不重建整个组件树。
- ECharts 实例复用 `setOption`，窗口变化时 resize，卸载时 dispose。
- 舞台只播放当前条目；后台只准备队列中的下一条。带真实 `video_url` 时用单个 hidden next video 准备到 `loadeddata` / `canplay`，只有 `preview_url` 时准备 MCAP metadata 和首帧，不同时播放多个资源。
- `previews` 队列最多保留 8 到 12 条，避免大量视频资源占用。
- 不做 2 到 3 条前瞻预载；任意时刻只允许一个 current 播放资源和一个 next 准备资源，避免无上限创建 hidden video、MCAP reader 或 object URL。
- 下一条未 ready 时不得清空当前舞台；current 停在最后一帧并最多等待 10 秒，超时后从头重播 current，同时继续准备同一个 next。
- MCAP 播放 timer 应按录制 timestamp 计算下一帧延迟；如果 timestamp 不可信，fallback 到固定保守帧间隔，并对最小/最大延迟做 clamp。
- 动画元素数量固定，避免每次轮播创建大量 DOM。
- timers、ResizeObserver、fullscreen listener、current/next video listener、hidden next video、MCAP reader 和 object URL 必须在 unmount 清理。
- 长列表只展示有限条目，大屏不是审计表格。
- `will-change` 只用于正在进入或离开的舞台元素，离场后移除。

接口性能：

- 后端 overview 应在一个只读事务中完成关键查询，沿用当前 dashboard handler 的做法。
- 查询需要 limit，不能一次返回所有任务或设备。
- 对近期任务、episodes、sync_logs 的查询应使用时间或 limit 限制。
- 数据库字段不足时，第一期宁可返回空数组，也不要用昂贵临时查询阻塞大屏。

## 11. 可访问性与降级策略

### 11.1 减少动画

支持 `prefers-reduced-motion`：

- 关闭 3D 飞入/飞出。
- 使用淡入淡出或轻微 crossfade。
- KPI 计数动画改为直接更新。

### 11.2 API 不可用

优先级：

1. 保留上一份成功 overview。
2. 在顶部状态栏显示同步失败或降级状态。
3. 若无历史数据，使用集中 mock fallback。
4. 页面不得空白。

### 11.3 视频不可用

- 无 `video_url` 但有 `preview_url`：解析 MCAP 图像帧并自动播放，失败后再显示 poster 或轻量等待状态。
- 视频加载错误：进入 `error` 状态，优先显示 poster 或 MCAP fallback；如果 next 已 ready 则切换 next，否则按 10 秒等待/重播 current 的规则继续。
- 自动播放失败：对真实视频静音重试；仍失败则优先使用 MCAP 图像帧，再退回 poster / 轻量等待状态。

### 11.4 小屏降级

- 1200px 以下禁用复杂 3D 动效。
- 任务流和设备列表减少条数。
- 趋势图高度降低，但保留 summary。
- 不允许横向滚动和文字重叠。

## 12. 开发阶段规划

### 阶段 1：文档和接口契约确认

目标：确认本方案、字段命名、异常表达方式、真实视频来源。

产出：

- 本设计文档
- overview API 契约确认
- 待确认问题关闭或建任务

验收：

- 产品、前端、后端理解同一套页面结构和数据契约。

风险：

- 真实视频来源不明确时，Video Flight Stage 第一版只能展示元信息 / poster fallback，不能播放假视频。

### 阶段 2：后端聚合 API

目标：新增 `/production/dashboard/overview`。

产出：

- Handler response structs
- summary/trend/devices/recent_tasks/previews 查询
- 单元测试
- Swagger 更新
- 不新增表，不新增告警闭环，不新增视频加工服务

验收：

- `go test ./internal/api/handlers/...` 通过。
- 空数据和 data_collector scope 正常。
- `previews.video_url` 为空时仍返回可展示的 episode/task 元信息；`video_url` 非空时必须是真实可播放视频。

风险：

- 设备在线判定和异常展示规则需要统一。

### 阶段 3：前端数据 adapter 和 mock fallback

目标：建立大屏数据入口。

产出：

- `useProductionBigScreenData.js`
- overview API client
- 集中 mock fallback
- 字段 normalize 和格式化

验收：

- API 成功、失败、空数据都能渲染稳定页面。

风险：

- fallback 与真实 API 字段不一致会带来后续返工。

### 阶段 4：响应式大屏布局

目标：建立多尺寸电视布局。

产出：

- 大屏页面结构
- KPI、主舞台、侧栏、底部区域响应式 CSS
- 关键断点适配

验收：

- 3840x2160、2560x1440、1920x1080、1600x900、1366x768 无重叠、无横向滚动。

风险：

- 信息过多导致 1366x768 下拥挤，需要减少列表条数。

### 阶段 5：Video Flight Stage 动画

目标：实现可运行的预览轮播舞台。

产出：

- `VideoFlightStage.vue`
- 状态机和 fallback
- reduced-motion 支持

验收：

- 视频/海报可进入、播放/展示、离场、下一条接替。
- 数据刷新不打断当前播放。

风险：

- 浏览器自动播放限制、视频格式兼容性、电视性能差异。

第一版到阶段 5 即可形成 MVP。阶段 6 和阶段 7 是增强与验收加固，不应阻塞第一版 PR 拆分。

### 阶段 6：图表和实时刷新

目标：完善趋势、设备、任务流和质量/异常状态。

产出：

- 趋势图组件
- 设备状态组件
- 任务流组件
- 质量/异常状态组件或内联展示
- 定时刷新策略

验收：

- 长时间运行稳定，刷新状态明确。

风险：

- 频繁 chart resize 或 setOption 造成性能波动。

### 阶段 7：性能、电视视口验证和修正

目标：验证真实展示效果。

产出：

- 视口截图或人工检查记录
- 性能问题修正
- 视频失败和 API 失败演练

验收：

- `npm run build` 通过。
- 目标视口下无明显问题。
- 页面长时间运行无明显内存增长。

风险：

- 电视浏览器版本较旧，需要 CSS/视频能力降级。

## 13. 验收标准

### 13.1 功能

- 聚合 API 可返回生产大屏所需数据。
- 前端优先使用聚合 API。
- 视频舞台可自动轮播。
- 当前条进入播放后，后台开始准备下一条；网络正常时，从第二条开始切换不应出现明显黑屏、空白或长时间加载。
- 下一条未 ready 时，舞台保留当前最后画面最多 10 秒；仍未 ready 时从头重播当前条，不做空白离场。
- 视频失败有 fallback。
- 任务流、设备状态、趋势图、质量/异常状态可展示。
- 数据刷新不打断当前视频播放。

### 13.2 视觉

- 第一眼像生产指挥中心。
- 中央视频舞台是视觉中心。
- KPI 远距离可读。
- 动画高级但克制。
- 不像营销页、静态海报或普通后台换皮。
- 异常状态醒目但不污染整个画面。

### 13.3 响应式

- `3840x2160` 正常，内容不显小。
- `2560x1440` 正常，布局自然。
- `1920x1080` 正常，作为基础验收尺寸。
- `1600x900` 正常，信息适度压缩。
- `1366x768` 不重叠、不横向滚动、不丢核心内容。

### 13.4 性能

- 长时间运行稳定。
- 定时刷新不造成闪烁或重排。
- 动画不卡顿。
- 无明显内存泄漏。
- 不同时播放多个视频。
- 轮播资源数量固定为 current + next，hidden video、MCAP reader 和 object URL 不随轮播无限增长。
- 图表实例和事件监听正确释放。

## 14. 待确认问题

后续开发前需要确认：

- 真实 `video_url` 从哪里来：直接上传视频、MCAP 转码，还是前端实时渲染后产出可播放视频流。未确认前不得伪造视频 URL。
- `preview_url` 应来自 episode、task 还是 MinIO 对象。
- 视频/预览 URL 是否需要鉴权，presigned URL 有效期多久。
- 大屏是否必须登录，是否需要专用只读展示 token。
- 大屏刷新频率是 15s、30s 还是可配置。
- 异常展示规则：哪些任务/同步/设备状态需要在顶部状态、设备状态或任务流中突出。
- 后续如需展示部分联通，应定义 recorder-only、transfer-only 与完全不在线的展示规则；当前设备在线只按 recorder/transfer hub 均联通计算。
- 质检通过率来源：`episodes.qa_status` 的哪些值算通过、失败、处理中。
- 是否需要多组织、多工厂筛选；如果大屏部署在单工厂电视，是否应固定 scope。
- 是否需要自动进入浏览器全屏，或者只保留手动全屏按钮。
- 是否需要展示敏感任务 ID 或客户名称，参观模式是否要脱敏。

## 15. 文档位置说明

本文档放在 `keystone-worktree2/docs/designs/`，因为当前仓库已有跨前后端设计文档目录，并且本方案的核心前提是后端聚合 API 契约。虽然页面实现主要发生在 `synapse-worktree2`，但后续开发应同时以本文档指导 Keystone API 与 Synapse 大屏实现。
