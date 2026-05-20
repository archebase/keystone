<!--
SPDX-FileCopyrightText: 2026 ArcheBase

SPDX-License-Identifier: MulanPSL-2.0
-->

# Synapse 管理后台品牌色系调整

**Scope:** Synapse 非沉浸式管理后台页面，即 `/admin/*` 中除 `/admin/dashboard` 生产大屏外的普通管理页。

## 1. 背景

生产大屏已经采用“工业暗屏基底 + ArcheBase 品牌强调色”的视觉方向。普通管理后台也应与该方向保持品牌一致，但它的主要用途是高频操作、筛选、录入和表格扫描，因此不应直接套用生产大屏的暗色大屏布局。

本调整的目标是统一色系，不重做全站设计系统、不改变信息架构、不修改 API。

## 2. 设计方向

普通管理后台采用“深色品牌导航 + 浅色操作台”的结构：

- 左侧导航使用 ArcheBase 深蓝/青色暗屏基底，建立与生产大屏一致的品牌识别。
- 主内容区保持浅色、低噪声、适合长时间操作和表格阅读。
- 卡片、表格、筛选区使用冷灰蓝边界和轻量青色选中态。
- 主要按钮、当前导航、排序/链接/进度等焦点元素使用 ArcheBase 品牌蓝/青色。
- 状态色继续保持生产语义：成功/在线为绿色，进行中/当前焦点为品牌青，警告为琥珀色，失败为红色，离线/取消/不可用为灰色。

## 3. Token 约束

Synapse 已有品牌基础变量：

- `--brand-blue: #07338c`
- `--brand-cyan: #079dd9`
- `--brand-yellow: #f1d86b`

普通管理后台不应在各页面硬编码新色值，而应在 `AdminLayout.vue` 的非沉浸式 `.admin-layout` 范围内派生后台 token，并让子组件继承：

- `--page-bg`：冷灰蓝页面基底。
- `--surface-bg` / `--surface-subtle` / `--surface-muted` / `--surface-selected`：浅色操作台表面。
- `--border-color` / `--border-subtle` / `--border-strong`：冷灰蓝边界。
- `--admin-sidebar-bg` / `--admin-sidebar-text` / `--admin-sidebar-active-bg`：深色品牌导航。
- `--admin-accent-line` / `--admin-accent-soft`：表格 hover、筛选区、卡片边界和焦点弱背景。

生产大屏继续使用局部 `--screen-brand-*` token；普通后台不复用大屏的 `100dvh`、全屏 grid、视频舞台动效或大屏字号。

## 4. 实现范围

建议优先修改：

- `src/components/layout/AdminLayout.vue`
- `src/components/layout/AdminSidebar.vue`
- `src/styles/admin-crud.css`
- `src/components/crud/ListPageLayout.vue`
- `src/components/crud/CrudFormLayout.vue`
- `src/components/common/DataTable.vue`

避免逐个重写所有管理页面。已有页面内使用 `var(--primary-*)`、`var(--secondary-*)`、`var(--surface-*)`、`var(--border-*)` 的样式应自然继承新色系。

## 5. 默认入口

生产大屏是沉浸式展示入口，不是管理员日常操作首页。Admin 登录成功后应进入普通管理后台的工厂管理页面：

- admin 登录成功默认跳转 `/admin/factories`。
- 直接访问 `/admin` 默认重定向到 `/admin/factories`。
- `/admin/dashboard` 仍作为显式生产大屏入口保留，可以从侧边栏“生产大屏”进入。
- 旧 `/dashboard` legacy redirect 可以继续指向 `/admin/dashboard`，用于兼容已经配置好的电视或书签。

这样可以避免管理员每次登录后误入全屏展示页面，同时不破坏生产大屏的独立访问路径。

## 6. 验收标准

- `/admin/dashboard` 生产大屏视觉不回退，仍保持沉浸式一屏布局。
- `/admin/*` 普通管理页拥有一致的 ArcheBase 深蓝/青色品牌识别，但主内容区仍适合表格阅读和表单操作。
- 管理后台不出现紫色霓虹、满屏发光卡片、营销页 hero 或大屏化排版。
- 离线、取消、不可用仍为灰色；失败和错误仍为红色。
- 侧边栏、表格、筛选区、表单页和通用按钮均能看到统一色系。
- admin 登录成功和访问 `/admin` 都进入 `/admin/factories`，不会自动进入 `/admin/dashboard`。
- `/admin/dashboard` 仍可直接访问，且侧边栏保留生产大屏入口。
- 桌面与移动宽度下导航和内容不重叠、不出现由色系调整引入的横向溢出。

## 7. 验证

前端至少运行：

```bash
npm run build
```

建议浏览检查：

- `/admin/factories`
- `/admin/workstations`
- `/admin/tasks`
- `/admin/statistics/data-production`
- `/admin/dashboard`
