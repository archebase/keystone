<!--
SPDX-FileCopyrightText: 2026 ArcheBase

SPDX-License-Identifier: MulanPSL-2.0
-->

# Keystone Callback Public Base URL 设计

**状态：** proposed

**范围：** Keystone 配置、设备注册响应、任务配置 callback URL、Synapse 下发任务逻辑。

## 1. 背景

Axon recorder 在机器人侧执行任务。任务开始和结束时，recorder 会通过 HTTP POST
回调 Keystone：

- `POST /api/v1/callbacks/start`
- `POST /api/v1/callbacks/finish`

当前 Synapse 在下发任务配置时，用浏览器当前 origin 拼 callback URL：

```js
new URL('/api/v1/callbacks/start', window.location.origin)
new URL('/api/v1/callbacks/finish', window.location.origin)
```

开发环境中，如果浏览器打开的是 `http://localhost:5174`，那么下发给 recorder 的
callback URL 就会变成：

```text
http://localhost:5174/api/v1/callbacks/start
http://localhost:5174/api/v1/callbacks/finish
```

但 Keystone 后端实际服务端口可能是 `9999`，`5174` 只是 Vite 前端开发服务器端口。
Vite 可以把浏览器请求代理到 Keystone，但机器人侧 recorder 不应该依赖前端开发服务器。

因此 Keystone 需要显式配置“机器人应该用哪个地址访问 Keystone callback API”。

## 2. 新增配置

新增环境变量：

```text
KEYSTONE_CALLBACK_PUBLIC_BASE_URL
```

含义：Keystone 对 Axon recorder 可访问的 callback 公共基地址。

示例：

```text
KEYSTONE_CALLBACK_PUBLIC_BASE_URL=http://192.168.1.20:9999
KEYSTONE_CALLBACK_PUBLIC_BASE_URL=https://keystone.factory.internal
```

该配置放在 Keystone 的 `ServerConfig` 中：

```go
type ServerConfig struct {
    Mode                  string
    BindAddr              string
    CallbackPublicBaseURL string
    ReadTimeout           int
    WriteTimeout          int
    ShutdownTimeout       int
}
```

## 3. 配置校验规则

`KEYSTONE_CALLBACK_PUBLIC_BASE_URL` 必须显式配置。未配置时，Keystone 启动失败。

校验规则：

| 规则 | 要求 |
| --- | --- |
| 非空 | 必须填写 |
| URL 类型 | 必须是绝对 URL |
| scheme | 只允许 `http` 或 `https` |
| host | 必须非空 |
| path | 必须为空或 `/` |
| query | 必须为空 |
| fragment | 必须为空 |

允许：

```text
http://192.168.1.20:9999
https://keystone.factory.internal
http://keystone.factory.internal/
```

不允许：

```text
192.168.1.20:9999
ftp://192.168.1.20:9999
http:///api
http://gateway.local/keystone
http://gateway.local?x=1
http://gateway.local#abc
```

校验通过后，Keystone 应规范化该值，去掉末尾 `/`：

```text
http://keystone.factory.internal/
-> http://keystone.factory.internal
```

## 4. 为什么不允许 base path

本轮设计不允许：

```text
http://gateway.local/keystone
```

原因是 Axon 的 callback allowlist 只返回固定路径前缀：

```text
/api/v1/callbacks/
```

如果 base URL 带 `/keystone`，recorder 实际访问路径会变成：

```text
/keystone/api/v1/callbacks/start
```

这会和 allowlist 的 `/api/v1/callbacks/` 对不上。

所以本轮约定：`KEYSTONE_CALLBACK_PUBLIC_BASE_URL` 只表达 scheme、host、port，不表达路径前缀。

## 5. Keystone 派生出来的值

Keystone 从 `KEYSTONE_CALLBACK_PUBLIC_BASE_URL` 派生任务 callback URL：

```text
start_callback_url  = KEYSTONE_CALLBACK_PUBLIC_BASE_URL + /api/v1/callbacks/start
finish_callback_url = KEYSTONE_CALLBACK_PUBLIC_BASE_URL + /api/v1/callbacks/finish
```

示例：

```text
KEYSTONE_CALLBACK_PUBLIC_BASE_URL=http://192.168.1.20:9999
```

派生结果：

```json
{
  "start_callback_url": "http://192.168.1.20:9999/api/v1/callbacks/start",
  "finish_callback_url": "http://192.168.1.20:9999/api/v1/callbacks/finish"
}
```

Keystone 从同一个配置派生注册响应中的 callback allowlist：

```text
allowed_host = URL(KEYSTONE_CALLBACK_PUBLIC_BASE_URL).Host
allowed_path_prefix = "/api/v1/callbacks/"
```

示例：

```json
{
  "callback_allowlist": {
    "allowed_host": "192.168.1.20:9999",
    "allowed_path_prefix": "/api/v1/callbacks/"
  }
}
```

如果 URL 使用默认端口，不额外补 `:80` 或 `:443`：

```text
https://keystone.factory.internal
-> allowed_host = keystone.factory.internal
```

## 6. 设备注册响应

`POST /api/v1/devices/register` 注册成功后，应返回 callback allowlist。注册响应也会返回
一次性明文 `ws_client_auth_token`，但该 token 的签发、存储和 WebSocket 校验规则由
`device-registration-api.md` 定义。

```json
{
  "device_id": "factory01-type02-0007",
  "factory": "上海一厂",
  "factory_id": "1",
  "robot_type": "搬运机器人",
  "robot_type_id": "2",
  "robot_id": "42",
  "ws_client_auth_token": "kws_v1_example",
  "callback_allowlist": {
    "allowed_host": "192.168.1.20:9999",
    "allowed_path_prefix": "/api/v1/callbacks/"
  }
}
```

注册响应只返回“允许访问范围”，不返回具体的 `start_callback_url` /
`finish_callback_url`。

## 7. 任务配置响应

`GET /api/v1/tasks/:id/config` 应返回 Keystone 服务端生成的 callback URL：

```json
{
  "task_id": "task_20260622_001",
  "device_id": "factory01-type02-0007",
  "start_callback_url": "http://192.168.1.20:9999/api/v1/callbacks/start",
  "finish_callback_url": "http://192.168.1.20:9999/api/v1/callbacks/finish"
}
```

任务配置只返回“本次任务具体回调地址”，不返回 `callback_allowlist`。

## 8. Keystone 下发安全边界

前端请求体里即使带了 callback URL，Keystone 也不应该信任它。

服务端在发送 recorder config RPC 前，应统一覆盖：

```text
start_callback_url  = 服务端生成值
finish_callback_url = 服务端生成值
```

这样可以避免 Synapse、脚本或其他调用方把 `localhost:5174`、错误 IP、或任意第三方地址塞进任务配置。

## 9. Synapse 行为

Synapse 不再使用 `window.location.origin` 拼 callback URL。

推荐行为：

- 打开任务配置时，从 Keystone 的 `GET /tasks/:id/config` 获取 `start_callback_url`
  和 `finish_callback_url`。
- 下发 recorder config 前，检查这两个字段是否非空。
- 如果缺失，阻止下发并提示 Keystone 配置错误。

错误提示建议：

```text
后端未返回 callback URL，请检查 Keystone 的 KEYSTONE_CALLBACK_PUBLIC_BASE_URL 配置
```

Synapse 不做 `5174` 兜底，也不从浏览器 origin 推导 callback 地址。

## 10. 开发环境

代码默认值仍然为空，强制显式配置。

Docker 开发环境可以预填：

```text
KEYSTONE_CALLBACK_PUBLIC_BASE_URL=http://localhost:8080
```

如果本机开发实际后端端口是 `9999`，应在本地 `.env` 或启动环境中配置：

```text
KEYSTONE_CALLBACK_PUBLIC_BASE_URL=http://localhost:9999
```

## 11. 本轮实现范围

本轮实现包含：

- Keystone 新增 `KEYSTONE_CALLBACK_PUBLIC_BASE_URL` 加载和强校验。
- Keystone 统一生成 callback URL。
- Keystone `devices/register` 返回 `callback_allowlist`。
- Keystone `tasks/:id/config` 返回服务端生成的 callback URL。
- Keystone recorder config 下发入口覆盖前端传入的 callback URL。
- Synapse 移除 `window.location.origin` 拼 callback URL 的逻辑。
- Synapse 下发前校验 callback URL 缺失并阻止。
- Docker dev compose 补默认开发值。
- 单元测试覆盖配置校验、callback URL 生成、注册响应字段。

本轮不包含：

- Axon transfer WebSocket token 鉴权。
- token 轮换 API。
- Axon token file 写入。
- HTML 动效或可视化文档。

## 12. 关键原则

- Keystone 不能可靠地自动知道机器人应该用哪个地址访问自己。
- callback 地址必须由 Keystone 显式配置生成，不能由 Synapse 浏览器 origin 推导。
- 注册响应给 Axon “允许访问哪里”。
- 任务配置给 recorder “这次具体回调哪里”。
- Keystone 是最终安全边界，不能信任前端传入的 callback URL。
