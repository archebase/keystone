# Archebase Data Gateway Swift SDK

本文档面向接入 Archebase Data Gateway Swift SDK 的 iOS App 开发人员。你不需要了解服务端内部实现，只需要根据本文完成 SDK 安装、设备初始化、上传、续传、取消、错误处理和上线前验证。

## 1. SDK 能做什么

`DataGatewayClient` 是面向 iOS 的高层文件上传 SDK。App 只需要提供本地文件 URL、上传标签和少量业务上下文，SDK 会完成上传所需的认证、分片上传、失败重试、本地状态持久化和断点恢复。

SDK 提供以下能力：

1. 初始化 iOS 设备并在 App 私有目录写入 `archebase-config.json`。
2. 从 `archebase-config.json` 创建上传客户端。
3. 上传本地文件并返回 `UploadResult`。
4. 订阅上传阶段和分片进度事件。
5. 列出 App 本地仍可恢复的上传任务。
6. 使用用户 access JWT 列出当前用户可见的远端 verified 逻辑对象。
7. 在 App 重启、网络恢复或上传中断后继续上传。
8. 取消远端上传并清理本地状态。
9. 仅删除本地快照，便于用户放弃恢复或处理异常状态。
10. 接入日志与指标回调，方便接入宿主 App 的可观测性系统。

## 2. 环境要求

| 项目 | 要求 |
|---|---|
| Swift | `>= 6.1` |
| iOS | `>= 18.0` |
| Xcode | `>= 16.2` |
| macOS 开发环境 | `>= 15` 推荐 |
| 并发模型 | Swift Concurrency，主要 API 使用 `async throws` 和 `AsyncThrowingStream` |

SDK 使用 App 私有容器中的 `archebase-endpoints.json` 定义 Archebase 公共服务端点。App 需要先通过可信渠道获取 endpoint JSON，并在设备初始化或创建上传客户端前调用 SDK 初始化方法写入本地文件。

## 3. 接入前需要准备

接入方需要从 Archebase 或本组织管理员处获取以下信息：

1. SDK 包地址或本地源码路径。
2. 首次初始化用的 `deviceID`。
3. 运行期 endpoint JSON，包含认证、上传网关和设备初始化端点。
4. 是否需要在 App 内提供重新初始化入口。

`deviceID` 不是用户在 App 中随意生成的 UUID。它应来自接入方的设备管理或交付流程，并由 operator 或管理员录入到 App。

## 4. SwiftPM 安装

### 4.1 Xcode 接入

在 Xcode 中打开宿主 App 工程，进入 `Package Dependencies`，添加组织提供的 SDK Git URL 或本地路径，然后在 App target 中选择产品：

```text
DataGatewayClient
DGWControlPlane
DGWStore
```

`DataGatewayClient` 是上传主入口。`DGWControlPlane` 提供 `DataGatewayClientError`，`DGWStore` 提供 `ArchebaseConfig`、`PendingUploadInfo` 和 `PersistedUploadPhase`。这些都是 SDK 当前公开 API 会直接暴露的类型，App 不需要理解它们背后的服务端实现。

### 4.2 Package.swift 接入

本 repo 根目录就是标准 SwiftPM package 根目录，`Package.swift` 位于 `data-sdk/Package.swift`。宿主 App 可以直接通过 Git URL 或本地 path 依赖本 repo。

远端包示例：

```swift
dependencies: [
    .package(url: "https://github.com/archebase/data-sdk.git", from: "0.1.4")
]
```

本地源码示例：

```swift
dependencies: [
    .package(path: "../data-sdk")
]
```

target 依赖示例。`package` 参数使用 SwiftPM package identity；Git URL 和上面的本地 path 示例通常解析为 `data-sdk`。

```swift
targets: [
    .target(
        name: "YourAppCore",
        dependencies: [
            .product(name: "DataGatewayClient", package: "data-sdk"),
            .product(name: "DGWControlPlane", package: "data-sdk"),
            .product(name: "DGWStore", package: "data-sdk")
        ]
    )
]
```

上传主入口产品名是 `DataGatewayClient`。

### 4.3 导入模块

```swift
import DataGatewayClient
import DGWControlPlane
import DGWStore
import Foundation
```

如果某个文件只发起上传且不显式引用错误、配置或待恢复任务类型，也可以只 `import DataGatewayClient`。本文示例为了覆盖完整 App 接入流程，统一展示三个 import。

## 5. 推荐接入流程

iOS App 推荐使用配置文件驱动方式接入。

1. App 计算 App 私有容器内的 `endpointsURL`、`configURL` 和 `persistRootURL`。
2. App 从可信渠道获取 endpoint JSON，调用 `DataGatewayClient.initialize(endpointsJSON:endpointsURL:)` 写入 `archebase-endpoints.json`。
3. App 首次启动或进入设备绑定页时，让 operator 输入平台提供的 `deviceID`。
4. 调用 `ArchebaseDeviceInitializer.initDevice(deviceID:)`。
5. SDK 将初始化结果写入 App 私有目录下的 `archebase-config.json`。
6. App 调用 `DataGatewayClient.fromArchebaseConfig(...)` 创建上传客户端。
7. 用户选择文件后，App 调用 `uploadEvents(_:)` 或 `upload(_:)` 上传。
8. App 每次启动后调用 `listPendingUploads()`，为用户展示可恢复任务。
9. 用户确认恢复时调用 `resumeUpload(logicalUploadID:)`。
10. 用户放弃上传时调用 `abortUpload(logicalUploadID:)`。

如果接入方已经通过其他安全渠道直接向 App 下发 `API Key`，也可以跳过设备初始化，直接使用 `DataGatewayClientConfig.recommended(...)` 创建客户端。生产 App 通常优先使用设备初始化方式。

### 5.1 穹彻专供接入

穹彻 App 优先使用 `QiongcheDataGatewaySDK`，不需要自行编排 endpoint 初始化、设备初始化和本地配置覆盖。穹彻 facade 只提供两个入口：

```swift
let qiongcheSDK = try QiongcheDataGatewaySDK()

try await qiongcheSDK.saveConfigAndInit(configString: configStringFromTrustedChannel)

let ready = await qiongcheSDK.isReadyToUpload()
```

`saveConfigAndInit(configString:)` 接收穹彻可信渠道下发的完整配置字符串，完成设备初始化并写入本地文件。设备尚未 initialized 时会调用远端 `InitDevice`；设备已 initialized 时会在收到 `DATA_GATEWAY_DEVICE_ALREADY_INITIALIZED` 后静默改调远端 `ReinitDevice`，拿到新凭证后再覆盖本地配置。`isReadyToUpload()` 用于上传前判断本地配置和服务连通性是否满足创建上传客户端的条件。

穹彻封装不提供 `reinit`、`reset` 或 `replaceConfig` 入口。需要更新配置时，继续调用 `saveConfigAndInit(configString:)`；该方法会在远端 init 或 reinit 成功后覆盖本地 endpoint、设备配置和穹彻 state。

完成 ready 检查后，上传仍使用通用上传客户端：

```swift
let supportRoot = FileManager.default.urls(
    for: .applicationSupportDirectory,
    in: .userDomainMask
)[0]
let archebaseRoot = supportRoot.appendingPathComponent("Archebase", isDirectory: true)

let client = try await DataGatewayClient.fromArchebaseConfig(
    configURL: archebaseRoot.appendingPathComponent("archebase-config.json"),
    persistRootURL: archebaseRoot.appendingPathComponent("Uploads", isDirectory: true),
    endpointsURL: archebaseRoot.appendingPathComponent("archebase-endpoints.json")
)
```

穹彻配置字符串是 UTF-8 JSON，顶层只接受 `auth`、`gateway`、`deviceInit` 和 `device_id`：

```json
{
  "device_id": "robot-001",
  "auth": { "scheme": "http", "host": "localhost", "port": 50051 },
  "gateway": { "scheme": "http", "host": "localhost", "port": 50053 },
  "deviceInit": { "scheme": "http", "host": "localhost", "port": 50057 }
}
```

字段约束：

| 字段 | 说明 |
|---|---|
| `device_id` | 必填，trim 后不能为空，不能包含控制字符；只写入穹彻 state，不写入 endpoint 文件。 |
| `auth` | 认证服务 endpoint，只接受 `scheme`、`host`、`port`。 |
| `gateway` | 上传网关 endpoint，只接受 `scheme`、`host`、`port`。 |
| `deviceInit` | 设备初始化 endpoint，只接受 `scheme`、`host`、`port`。 |

`saveConfigAndInit(configString:)` 的覆盖规则：

1. 先解析配置并使用本次 `deviceInit` endpoint 调远端 init；如果服务端提示已 initialized，则内部改调远端 reinit。
2. 远端 init 或 reinit 成功后，覆盖写入 `archebase-endpoints.json`、`archebase-config.json` 和 `qiongche-sdk-state.json`。
3. 本地已有 endpoint、设备配置或 state 时，不因为内容不同报错。
4. 远端 init 和必要的 reinit 失败时，不覆盖本地已有 endpoint、设备配置或 state。
5. endpoint 文件只保存 `auth`、`gateway`、`deviceInit`，不会保存 `device_id`。

`isReadyToUpload()` 的判断范围：

1. 本地 `archebase-config.json` 存在且可解析。
2. 本地 `archebase-endpoints.json` 存在且可解析。
3. `auth` 和 `gateway` endpoint 可发起轻量 gRPC 请求。

`isReadyToUpload()` 不创建上传任务，不申请对象存储临时凭证，不探测对象存储，也不探测 `deviceInit` endpoint。返回 `false` 的常见原因包括本地文件缺失、JSON 损坏、endpoint 不可达、TLS 配置不匹配、DNS/TCP 连接失败或请求超时。

穹彻接入时建议单独处理配置错误：

```swift
do {
    try await qiongcheSDK.saveConfigAndInit(configString: configStringFromTrustedChannel)
} catch let error as QiongcheSDKError {
    switch error {
    case .invalidConfigString(let message):
        print(message)
    }
} catch let error as DataGatewayClientError {
    switch error {
    case .persistenceFailed(let message):
        print(message)
    default:
        print(error)
    }
} catch {
    print(error.localizedDescription)
}
```

安全注意事项：

1. 不要把完整 `configString`、`api_key`、access token、STS secret 或带签名 query 的 URL 写入日志、埋点、剪贴板或可导出的诊断文件。
2. App UI 不应展示 `archebase-config.json` 中的 `api_key`。
3. `configString` 只能来自可信渠道；再次提交会切换本机 endpoint、设备配置和穹彻 state。
4. 排查问题时优先记录短错误摘要、文件是否存在、ready 结果和时间，不记录敏感字段值。

穹彻示例 App 位于：

```text
Examples/qiongche-sdk-demo/
```

运行方式：

```bash
open Examples/qiongche-sdk-demo/qiongche-sdk-demo.xcodeproj
```

示例 App 面向穹彻 SDK 接入者，首屏提供配置输入、保存并初始化、本地状态、ready 检查、生成样例文件和上传样例文件。示例 App 不提供 reinit 按钮；再次提交配置会继续调用 `saveConfigAndInit(configString:)`，由 SDK 内部按服务端状态执行 init 或 reinit 并覆盖本地 endpoint、设备配置和穹彻 state。

## 6. 文件目录建议

建议将 SDK 配置和上传持久化状态放在 `Application Support` 下，并保持在 App 私有容器内：

```swift
let supportRoot = FileManager.default.urls(
    for: .applicationSupportDirectory,
    in: .userDomainMask
)[0]

let archebaseRoot = supportRoot.appendingPathComponent("Archebase", isDirectory: true)

let endpointsURL = archebaseRoot.appendingPathComponent("archebase-endpoints.json")

let configURL = archebaseRoot.appendingPathComponent("archebase-config.json")

let persistRootURL = archebaseRoot.appendingPathComponent("Uploads", isDirectory: true)
```

`archebase-endpoints.json` 和 `archebase-config.json` 都应放在 App 私有容器内。`archebase-config.json` 包含上传凭证，两个文件都不要放入 App bundle、共享容器、剪贴板、日志、埋点或用户可导出的诊断文件中。SDK 在 iOS 上写入这些文件时会使用系统文件保护选项。

## 7. 设备初始化

### 7.1 首次初始化

```swift
let initializer = try ArchebaseDeviceInitializer(
    config: DeviceInitClientConfig(
        configURL: configURL,
        endpointsURL: endpointsURL
    )
)

let deviceConfig = try await initializer.initDevice(deviceID: "260427-000001")

print(deviceConfig.tags)
```

`initDevice(deviceID:)` 的行为：

1. 从 `archebase-endpoints.json` 读取 `deviceInit` endpoint。
2. 本地没有 `archebase-config.json` 时，调用远端 `DeviceInitService.InitDevice` 请求设备配置并写入本地文件。
3. 本地已经存在配置文件时，抛出 `DataGatewayClientError.alreadyInitialized(configURL:)`。
4. 如果服务端返回 `DATA_GATEWAY_DEVICE_ALREADY_INITIALIZED`，错误会以 `DataGatewayClientError.gatewayFailed` 向上暴露，不会自动 fallback 到 reinit。
5. 写入成功后返回 `ArchebaseConfig`，其中包含 `API Key` 和设备 tags。

如果 `archebase-endpoints.json` 不存在，构造 `ArchebaseDeviceInitializer(config:)` 时会抛出 `DataGatewayClientError.endpointsNotInitialized(endpointsURL:)`。App 应先获取 endpoint JSON，调用 `DataGatewayClient.initialize(endpointsJSON:endpointsURL:)`，成功后重试设备初始化。

### 7.2 重新初始化

当设备换绑、凭证泄露、现场运维要求重置或管理员明确要求时，可以调用重新初始化：

```swift
let newDeviceConfig = try await initializer.reinitDevice(deviceID: "260427-000001")
```

`reinitDevice(deviceID:)` 的行为：

1. 本地没有配置文件时，抛出 `DataGatewayClientError.notInitialized(configURL:)`。
2. 本地已经存在配置文件时，调用远端 `DeviceInitService.ReinitDevice` 重新获取设备配置并原子替换本地文件。
3. 如果服务端返回 `DATA_GATEWAY_DEVICE_NOT_INITIALIZED`，错误会以 `DataGatewayClientError.gatewayFailed` 向上暴露，不会自动 fallback 到 init。
4. 重新初始化会轮换上传凭证，旧配置中的凭证会失效。

已经开始且仍在本地快照中的上传，会继续使用创建上传时保存的 tags 和恢复状态。重新初始化只影响后续新建的上传客户端和新上传。

建议将重新初始化放在设置页或运维入口中，不要在普通上传失败时自动调用。普通上传失败应优先根据错误类型提示用户重试、恢复上传或联系支持。

### 7.3 配置文件格式

SDK 写入的配置文件格式如下：

```json
{
  "api_key": "<API Key>",
  "tags": {
    "device": "robot-1"
  }
}
```

字段说明：

| 字段 | 说明 |
|---|---|
| `api_key` | 上传凭证。App 不应展示、记录或主动解析该值。 |
| `tags` | 与设备相关的标签。SDK 会自动合并到每次上传的 `rawTags` 中。 |

tags 约束：

1. 最多 `256` 个 key。
2. key 不能为空。
3. key 最长 `64` 个 UTF-8 bytes。
4. value 最长 `2048` 个 UTF-8 bytes。
5. key 和 value 不能包含 Unicode 控制字符。

## 8. 创建上传客户端

### 8.1 从 archebase-config.json 创建

这是 iOS App 的推荐方式：

```swift
let client = try await DataGatewayClient.fromArchebaseConfig(
    configURL: configURL,
    persistRootURL: persistRootURL,
    endpointsURL: endpointsURL,
    observability: .disabled
)
```

`fromArchebaseConfig(...)` 会读取并校验 `archebase-config.json`，使用其中的 `api_key` 创建客户端，并记录配置中的 tags。之后每次上传时，配置 tags 会自动与 `UploadRequest.rawTags` 合并。

如果配置文件不存在，方法会抛出 `DataGatewayClientError.notInitialized(configURL:)`。App 应引导用户先完成设备初始化。

如果 endpoint 文件不存在，方法会抛出 `DataGatewayClientError.endpointsNotInitialized(endpointsURL:)`。App 应先调用 `DataGatewayClient.initialize(endpointsJSON:endpointsURL:)`，成功后重试创建客户端。

### 8.2 使用显式 API Key 创建

仅当接入方已经通过其他安全渠道直接下发 `API Key` 时使用：

```swift
let config = try DataGatewayClientConfig.recommended(
    credentialBase64: "<credential_base64>",
    persistRootURL: persistRootURL,
    endpointsURL: endpointsURL
)

let client = try DataGatewayClient(config: config)
```

### 8.3 运行期公共服务端点

认证、上传网关和设备初始化端点来自运行期文件 `archebase-endpoints.json`。App 需要通过可信渠道获取 JSON 字符串，并在首次使用 SDK 端点前写入 App 私有容器：

```swift
try DataGatewayClient.initialize(
    endpointsJSON: endpointsJSONStringFromTrustedChannel,
    endpointsURL: endpointsURL
)
```

文件名固定为：

```text
archebase-endpoints.json
```

示例：

```json
{
  "auth": { "scheme": "http", "host": "nlb-example.cn-shanghai.nlb.aliyuncsslb.com", "port": 50051 },
  "gateway": { "scheme": "http", "host": "nlb-example.cn-shanghai.nlb.aliyuncsslb.com", "port": 50053 },
  "deviceInit": { "scheme": "http", "host": "nlb-example.cn-shanghai.nlb.aliyuncsslb.com", "port": 50057 }
}
```

字段说明：

| 字段 | 说明 |
|---|---|
| `scheme` | 只允许 `http` 或 `https` |
| `host` | DNS hostname、IPv4 或 IPv6，不包含 scheme 和 port |
| `port` | `1...65535` 的整数 |

顶层必须包含 `auth`、`gateway` 和 `deviceInit`。每组 endpoint 只接受 `scheme`、`host` 和 `port`；旧字段名 `schema` 不再接受。`http` 会使用 plaintext gRPC 连接，`https` 会使用 TLS gRPC 连接。认证、上传网关和设备初始化可以分别指定不同的 `scheme`、`host` 和 `port`。

`initialize` 会先完整解析和校验 JSON，成功后才创建目录并原子写入文件。重复传入等价 endpoint 内容会幂等成功；如果目标文件已经存在且内容不同，会抛出 `DataGatewayClientError.endpointsAlreadyInitialized(endpointsURL:)`，避免 App 无意中静默切换服务端。

SwiftPM 命令行构建示例：

```bash
cd data-sdk
swift build
swift test
```

App 只负责提供 `deviceID`、本地配置文件路径、上传持久化目录和凭证来源。

## 9. DataGatewayClientConfig

`DataGatewayClientConfig.recommended(...)` 会填入适合 iOS App 的默认值。多数 App 不需要手动创建完整配置。

| 字段 | 说明 | 推荐值 |
|---|---|---|
| `credentialBase64` | 上传凭证 | 来自配置文件或安全下发渠道 |
| `authRefreshBefore` | 认证缓存提前刷新时间 | `60s` |
| `requestTimeout` | 单次请求超时 | `10s` |
| `persistRootURL` | 上传快照根目录 | App 私有 `Application Support` 子目录 |
| `endpointsURL` | 运行期公共端点文件 | App 私有 `Application Support/Archebase/archebase-endpoints.json` |
| `retryPolicy` | 请求重试策略 | `.recommended` |
| `execution` | 上传执行策略 | `.recommended` |
| `observability` | 日志和指标回调 | `.disabled` 或宿主 App 自定义 |

默认重试策略：

| 层级 | 最大尝试次数 | 初始退避 | 最大退避 |
|---|---:|---:|---:|
| 认证和上传网关请求 | `5` | `0.5s` | `8s` |
| 文件上传请求 | `8` | `1s` | `30s` |

默认上传执行策略：

| 字段 | 默认值 | 说明 |
|---|---:|---|
| `maxRestartCount` | `3` | 恢复时最多自动重建上传会话的次数 |
| `autoResumeByFileURL` | `true` | 预留给按文件 URL 自动恢复的策略开关 |
| `reconcileRemotePartsOnResume` | `true` | 恢复时校验已上传分片状态 |
| `cleanupOnTerminalFailure` | `true` | 终态失败时允许 SDK 清理不可恢复状态 |
| `credentialRefreshSkew` | `30s` | 上传凭证过期前提前刷新 |
| `persistence` | `.recommended` | 本地快照和直读文件策略 |

默认本地持久化策略：

| 字段 | 默认值 | 说明 |
|---|---:|---|
| `keepTerminalSnapshot` | `true` | 失败终态快照短期保留，便于排查 |
| `keepCompletedSnapshot` | `false` | 完成后不保留快照 |
| `completedSnapshotTTL` | `0s` | 完成快照保留时间 |
| `terminalSnapshotTTL` | `3600s` | 失败终态快照保留时间 |
| `copyExternalFileIntoManagedStaging` | `false` | 兼容字段；当前版本不再生成 staging 数据副本，上传直接读取源文件 |

## 10. 发起上传

### 10.1 最小上传

```swift
let request = UploadRequest(
    fileURL: fileURL,
    clientHints: ["source": "ios-app"],
    rawTags: ["scene": "inspection"],
    displayName: fileURL.lastPathComponent
)

let result = try await client.upload(request)

print(result.logicalUploadID)
print(result.objectKey)
print(result.ossObjectETag)
```

`upload(_:)` 适用于不需要细粒度进度 UI 的场景。方法成功后返回最终 `UploadResult`，失败时抛出 `DataGatewayClientError` 或底层错误。

### 10.2 带上传事件的上传

```swift
let request = UploadRequest(
    fileURL: fileURL,
    clientHints: ["source": "ios-app"],
    rawTags: ["scene": "inspection"],
    displayName: fileURL.lastPathComponent
)

var newlySentBytes: UInt64 = 0

for try await event in await client.uploadEvents(request) {
    switch event {
    case .preparing:
        print("preparing")
    case .authenticating:
        print("authenticating")
    case .creatingLogicalUpload:
        print("creating upload")
    case .initiatingMultipart(let uploadID):
        print("upload session: \(uploadID)")
    case .uploadingPart(let partNumber, let sentBytes, let totalBytes):
        newlySentBytes += sentBytes
        let progress = Double(newlySentBytes) / Double(totalBytes)
        print("part \(partNumber), progress \(progress)")
    case .refreshingCredentials:
        print("refreshing credentials")
    case .reconcilingRemoteParts:
        print("checking resumable state")
    case .completingMultipart:
        print("finishing file upload")
    case .completingBusinessUpload:
        print("finalizing upload")
    case .completed(let result):
        print("completed: \(result.logicalUploadID)")
    case .resuming(let logicalUploadID):
        print("resuming: \(logicalUploadID)")
    }
}
```

`uploadEvents(_:)` 会启动一次新的上传，它不是对 `upload(_:)` 已启动任务的旁路监听。`uploadingPart.sentBytes` 表示当前分片大小，不是累计上传字节数。恢复上传时，该值只覆盖本次恢复后实际继续发送的分片。

### 10.3 UploadRequest 字段

| 字段 | 说明 |
|---|---|
| `fileURL` | 待上传的本地文件 URL。文件必须存在且非空。 |
| `clientHints` | 传给上传网关的轻量上下文。建议只放路由、来源、App 版本等非敏感信息。 |
| `rawTags` | 上传完成后随文件保存的业务标签。会与设备配置 tags 和 SDK 源文件名保留 tag 合并。 |
| `displayName` | 可选展示名。建议传入用户可识别的文件名，但不要依赖它决定远端存储路径。 |

### 10.4 rawTags 合并规则

SDK 会把源文件名写入保留 raw tag，value 是 `UploadRequest.fileURL.lastPathComponent`：

1. `a206e337ecdf70a93bb611cf6a30c346.raw_file`

如果客户端由 `fromArchebaseConfig(...)` 创建，SDK 还会把配置文件中的 tags 与单次上传的 `rawTags` 合并：

1. key 只存在于配置 tags 中，写入最终 tags。
2. key 只存在于 `UploadRequest.rawTags` 中，写入最终 tags。
3. key 同时存在且 value 相同，接受。
4. key 同时存在但 value 不同，抛出 `DataGatewayClientError.rawTagConflict(key:)`，不会创建上传任务。

不要手动覆盖上述源文件名保留 key；如果传入的 `rawTags` 或配置 tags 对这些 key 使用了不同 value，SDK 会按冲突处理。

不要在 `rawTags` 或 `clientHints` 中放入密码、token、个人隐私信息或其他不应进入业务元数据的内容。

## 11. 上传结果

`UploadResult` 字段如下：

| 字段 | 说明 |
|---|---|
| `logicalUploadID` | 稳定上传标识。App 应使用它做恢复、取消和问题排查。 |
| `uploadID` | 当前上传会话标识。恢复或重建后可能变化。 |
| `bucket` | 对象存储 bucket 名称。通常用于诊断或与后端支持协作。 |
| `objectKey` | 文件在对象存储中的 key。 |
| `fileSize` | 上传文件大小，单位 bytes。 |
| `ossObjectETag` | 上传后对象的 ETag。可用于诊断和完整性排查。 |

App 侧持久化上传记录时，优先保存 `logicalUploadID`、`objectKey`、`fileSize` 和业务自己的文件记录 ID。不要把 `api_key`、临时访问凭证或完整错误详情写入可导出的用户日志。

## 12. 列出远端对象

`listObjects(_:authorizationHeader:)` 使用用户 access JWT 调用 Data Gateway object service，返回当前用户可见的 verified 逻辑对象。它不使用上传 `api_key`。`authorizationHeader` 不能为空；临时网络错误会复用控制面重试策略，但 `unauthenticated` 会直接返回给 App，由 App 负责刷新用户会话后重试。

```swift
let page = try await client.listObjects(
    ListObjectsOptions(
        pageSize: 50,
        pageToken: nil,
        filter: "status:verified"
    ),
    authorizationHeader: "Bearer \(userAccessToken)"
)

for object in page.objects {
    print(object.objectID)
    print(object.fileID)
    print(object.status)
}
```

`page.nextPageToken` 非空时，可带同一 filter 继续请求下一页。

## 13. 恢复上传

SDK 会在 `persistRootURL` 下保存上传快照。App 重启、网络恢复、进程被系统终止后，可以使用这些快照恢复上传。

### 13.1 启动时列出待恢复任务

```swift
let pendingUploads = try await client.listPendingUploads()

for item in pendingUploads {
    print(item.logicalUploadID)
    print(item.fileURL)
    print(item.fileSize)
    print(item.phase)
}
```

`listPendingUploads()` 只返回仍在 active 状态的本地快照。使用推荐配置时，已完成上传不会出现在结果中。

### 13.2 恢复指定任务

```swift
let result = try await client.resumeUpload(logicalUploadID: pending.logicalUploadID)
print(result.objectKey)
```

恢复时 SDK 会校验原始文件是否仍然存在、大小是否一致、文件指纹是否匹配。如果文件丢失、移动或被修改，会抛出 `DataGatewayClientError.resumeNotPossible`。

恢复逻辑对 App 的承诺：

1. 已经成功上传并确认的分片不会被无意义重复上传。
2. 如果上传实际已经完成但 App 在最终确认前退出，SDK 会尝试补齐最终确认。
3. 如果远端状态无法继续安全恢复，SDK 会在 `maxRestartCount` 限制内自动重建上传会话。
4. 超过重建次数上限后，SDK 抛出 `DataGatewayClientError.uploadRestartExceeded`。

### 13.3 PendingUploadInfo 字段

| 字段 | 说明 |
|---|---|
| `logicalUploadID` | 恢复和取消使用的稳定上传标识。 |
| `uploadID` | 最近一次上传会话标识。 |
| `fileURL` | 本地源文件 URL。 |
| `fileSize` | 文件大小，单位 bytes。 |
| `phase` | 本地记录的上传阶段。 |
| `restartCount` | 已自动重建上传会话的次数。 |
| `updatedAt` | 本地快照最近更新时间。 |

`phase` 可能值：

| 值 | 说明 |
|---|---|
| `sessionCreated` | 已创建上传任务，尚未开始分片。 |
| `multipartInitiated` | 已准备分片上传。 |
| `uploading` | 已上传部分分片。 |
| `multipartCompleted` | 文件分片上传已完成，等待最终确认。 |
| `businessCompleting` | 正在完成最终确认。 |
| `terminalFailed` | 已进入失败终态。 |

## 14. 取消和本地清理

### 14.1 取消远端上传

```swift
try await client.abortUpload(logicalUploadID: logicalUploadID)
```

`abortUpload(logicalUploadID:)` 会请求远端取消该上传，并在取消成功或远端已经找不到该上传时删除本地快照。用户在 UI 中选择“取消上传”或“放弃任务”时，优先使用该方法。

### 14.2 仅删除本地快照

```swift
try await client.deleteLocalSnapshot(logicalUploadID: logicalUploadID)
```

`deleteLocalSnapshot(logicalUploadID:)` 只删除 App 本地快照，不会取消远端上传。仅在以下场景使用：

1. 用户明确选择“仅从本机移除记录”。
2. 支持人员要求清理本地损坏状态。
3. App 决定不再展示某个不可恢复任务，但不希望发送远端取消请求。

如果你的目标是取消上传并释放远端资源，请使用 `abortUpload(logicalUploadID:)`。

## 15. 错误处理

SDK 的公开错误模型是 `DataGatewayClientError`。

```swift
do {
    let result = try await client.upload(request)
    print(result.logicalUploadID)
} catch let error as DataGatewayClientError {
    switch error {
    case .notInitialized:
        // 引导用户完成设备初始化。
        break
    case .alreadyInitialized:
        // 本机已经初始化。可以直接创建上传 client。
        break
    case .endpointsNotInitialized:
        // 获取 endpoint JSON，调用 DataGatewayClient.initialize 后重试。
        break
    case .endpointsAlreadyInitialized:
        // 本机已有不同 endpoint 配置。不要静默覆盖，交给运维流程处理。
        break
    case .rawTagConflict(let key):
        // 调整单次上传 rawTags，避免覆盖设备配置 tags。
        print("tag conflict: \(key)")
    case .invalidLocalFile(let message):
        // 文件不存在、无法读取或属性异常。
        print(message)
    case .zeroByteFile:
        // 提示用户选择非空文件。
        break
    case .resumeNotPossible(let reason):
        // 提示用户重新上传，或提供取消/清理本地记录入口。
        print(reason)
    case .authenticationFailed(_, let message):
        // 凭证无效、过期且无法刷新，或账号权限问题。
        print(message)
    case .gatewayFailed(_, let detailCode, let message):
        // 上传网关返回失败。可以记录 detailCode 供排查。
        print(detailCode ?? "-", message)
    case .ossFailed(let httpStatus, let ossCode, let message):
        // 文件上传过程失败。可以提示用户检查网络后重试。
        print(httpStatus as Any, ossCode ?? "-", message)
    case .retryExhausted(let lastError):
        // 多次重试后仍失败。
        print(lastError)
    case .uploadRestartExceeded:
        // 自动重建上传会话次数达到上限。
        break
    case .integrityCheckFailed(let message):
        // 完整性校验失败。建议保留日志并联系支持。
        print(message)
    case .invalidConfiguration(let message):
        // credential、策略参数或本地配置不合法。
        print(message)
    case .persistenceFailed(let message):
        // 本地文件系统写入或校验失败。
        print(message)
    case .cancelled:
        // 当前请求被取消。
        break
    }
} catch {
    print(error.localizedDescription)
}
```

常见处理建议：

| 错误 | App 建议 |
|---|---|
| `notInitialized` | 展示设备初始化页面。 |
| `alreadyInitialized` | 不要重复初始化，直接进入上传流程。 |
| `endpointsNotInitialized` | 获取可信 endpoint JSON，调用 `DataGatewayClient.initialize(endpointsJSON:endpointsURL:)` 后重试。 |
| `endpointsAlreadyInitialized` | 本机已有不同 endpoint 文件，停止自动切换并提示运维处理。 |
| `invalidConfiguration` | 检查配置文件、credential 和策略参数是否正确。 |
| `rawTagConflict` | 修改单次上传 tags，避免与设备 tags 冲突。 |
| `invalidLocalFile` | 让用户重新选择文件。 |
| `zeroByteFile` | 提示不能上传空文件。 |
| `authenticationFailed` | 提示重新初始化或联系管理员。 |
| `gatewayFailed` | 记录 `detailCode` 并按接入方错误码策略提示用户。 |
| `ossFailed` | 通常与网络、对象存储可用性或临时凭证有关，可提示稍后重试。 |
| `retryExhausted` | 提示网络不稳定，允许用户稍后恢复。 |
| `resumeNotPossible` | 提供重新上传、取消上传或删除本地快照入口。 |
| `uploadRestartExceeded` | 提示重新上传，必要时保留日志联系支持。 |
| `persistenceFailed` | 检查磁盘空间、文件保护状态和 App 容器权限。 |

## 16. 可观测性

通过 `DataGatewayClientObservability` 将 SDK 日志和指标接入宿主 App：

```swift
let observability = DataGatewayClientObservability(
    onLog: { event in
        print("[DGW]", event.operation, event.phase ?? "-", event.message)
    },
    onMetric: { name, dimensions in
        print("[DGW metric]", name, dimensions)
    }
)

let client = try await DataGatewayClient.fromArchebaseConfig(
    configURL: configURL,
    persistRootURL: persistRootURL,
    endpointsURL: endpointsURL,
    observability: observability
)
```

日志事件字段：

| 字段 | 说明 |
|---|---|
| `operation` | 操作名称，例如 `upload`、`resume`、`refresh_credentials`。 |
| `uploadID` | 当前上传会话标识，可能为空。 |
| `logicalUploadID` | 稳定上传标识，可能为空。 |
| `phase` | SDK 当前阶段，可能为空。 |
| `attempt` | 重试次数，可能为空。 |
| `statusCode` | 错误状态码，可能为空。 |
| `detailCode` | 细分错误码，可能为空。 |
| `message` | 脱敏后的日志消息。 |

SDK 会对包含 `credential`、`token`、`accessKey`、`secret` 等关键词的日志消息进行脱敏。宿主 App 仍应避免在业务日志中输出 `api_key`、设备初始化输入、临时凭证或用户隐私信息。

当前标准指标：

| 指标名 | 维度 | 说明 |
|---|---|---|
| `upload_part` | `upload_id`、`part_number` | 分片上传事件。 |
| `credentials_refresh` | `upload_id` | 上传凭证刷新事件。 |

## 17. iOS App 生命周期建议

### 17.1 App 启动

App 启动后建议执行：

1. 计算 `endpointsURL`、`configURL` 和 `persistRootURL`。
2. 确保可信 endpoint JSON 已通过 `DataGatewayClient.initialize(endpointsJSON:endpointsURL:)` 写入。
3. 尝试调用 `DataGatewayClient.fromArchebaseConfig(...)`。
4. 如果抛出 `notInitialized`，进入设备初始化流程。
5. 如果抛出 `endpointsNotInitialized`，获取 endpoint JSON 并初始化后重试。
6. 如果创建成功，调用 `listPendingUploads()`。
7. 将待恢复任务展示给用户，或按产品策略自动恢复。

示例：

```swift
func makeClientOrRequireInitialization() async throws -> DataGatewayClient? {
    do {
        return try await DataGatewayClient.fromArchebaseConfig(
            configURL: configURL,
            persistRootURL: persistRootURL,
            endpointsURL: endpointsURL
        )
    } catch let error as DataGatewayClientError {
        if case .notInitialized = error {
            return nil
        }
        throw error
    }
}
```

### 17.2 前后台切换

当前 SDK 使用 Swift Concurrency 执行上传，不是 iOS `URLSession` background transfer。App 进入后台后，系统可能暂停或终止进程。

建议：

1. 对短上传，可以在进入后台时申请有限时长的 background task。
2. 对长上传，不要假设 App 被挂起后仍能持续上传。
3. App 回到前台或重启后，调用 `listPendingUploads()` 并恢复任务。
4. UI 上应允许用户重试、恢复、取消或重新选择文件。

### 17.3 文件选择与安全作用域

如果文件来自 `UIDocumentPickerViewController` 或其他安全作用域 URL，SDK 会尽量访问安全作用域资源并记录 bookmark，但不会把文件复制到 SDK staging 目录。上传和恢复都直接依赖原始文件 URL；因此 App 需要确保上传期间和恢复前源文件仍然存在且可访问，不要在任务完成前移动、删除或改写该文件。

### 17.4 文件大小与内存

当前版本直接读取源文件上传：单对象上传使用文件 body，multipart 上传按分片构造区间流，不再把完整文件 `readAll` 到内存。SDK 仍会读取首 1 MiB 计算恢复指纹，并按分片小块读取计算分片 MD5。对于非常大的文件、后台长传或高并发上传需求，建议在上线前与 Archebase 支持团队确认版本能力和压测结果。

## 18. 完整示例

下面示例展示一个 App 侧上传服务的典型封装方式：

```swift
import DataGatewayClient
import DGWControlPlane
import DGWStore
import Foundation

actor GatewayUploadService {
    private let endpointsURL: URL
    private let configURL: URL
    private let persistRootURL: URL
    private var client: DataGatewayClient?

    init() {
        let supportRoot = FileManager.default.urls(
            for: .applicationSupportDirectory,
            in: .userDomainMask
        )[0]
        let archebaseRoot = supportRoot.appendingPathComponent("Archebase", isDirectory: true)

        self.endpointsURL = archebaseRoot.appendingPathComponent("archebase-endpoints.json")
        self.configURL = archebaseRoot.appendingPathComponent("archebase-config.json")
        self.persistRootURL = archebaseRoot.appendingPathComponent("Uploads", isDirectory: true)
    }

    func initializeEndpoints(endpointsJSON: String) throws {
        try DataGatewayClient.initialize(
            endpointsJSON: endpointsJSON,
            endpointsURL: self.endpointsURL
        )
    }

    func initializeDevice(deviceID: String) async throws {
        let initializer = try ArchebaseDeviceInitializer(
            config: DeviceInitClientConfig(
                configURL: self.configURL,
                endpointsURL: self.endpointsURL
            )
        )
        _ = try await initializer.initDevice(deviceID: deviceID)
        self.client = try await self.makeClient()
    }

    func loadClient() async throws -> DataGatewayClient {
        if let client = self.client {
            return client
        }
        let client = try await self.makeClient()
        self.client = client
        return client
    }

    func pendingUploads() async throws -> [PendingUploadInfo] {
        let client = try await self.loadClient()
        return try await client.listPendingUploads()
    }

    func upload(fileURL: URL) async throws -> UploadResult {
        let request = UploadRequest(
            fileURL: fileURL,
            clientHints: ["source": "ios-app"],
            rawTags: [:],
            displayName: fileURL.lastPathComponent
        )
        let client = try await self.loadClient()
        return try await client.upload(request)
    }

    func resume(logicalUploadID: String) async throws -> UploadResult {
        let client = try await self.loadClient()
        return try await client.resumeUpload(logicalUploadID: logicalUploadID)
    }

    func abort(logicalUploadID: String) async throws {
        let client = try await self.loadClient()
        try await client.abortUpload(logicalUploadID: logicalUploadID)
    }

    private func makeClient() async throws -> DataGatewayClient {
        try await DataGatewayClient.fromArchebaseConfig(
            configURL: self.configURL,
            persistRootURL: self.persistRootURL,
            endpointsURL: self.endpointsURL
        )
    }
}
```

在 SwiftUI 或 UIKit 中更新 UI 时，请在 `MainActor` 上处理上传状态。不要在 `onLog`、`onMetric` 或上传事件循环中执行耗时同步操作。

## 19. Public API 速查

以下类型分布在 `DataGatewayClient`、`DGWControlPlane` 和 `DGWStore` 模块中。App 按第 4 节同时添加这三个产品并导入对应模块后，可以直接使用这些公开类型。

### 19.1 DataGatewayClient

```swift
public actor DataGatewayClient {
    public static func initialize(endpointsJSON: String, endpointsURL: URL) throws

    public init(config: DataGatewayClientConfig) throws

    public static func fromArchebaseConfig(
        configURL: URL,
        persistRootURL: URL,
        endpointsURL: URL,
        observability: DataGatewayClientObservability = .disabled
    ) async throws -> DataGatewayClient

    public func upload(_ request: UploadRequest) async throws -> UploadResult

    public func uploadEvents(_ request: UploadRequest) -> AsyncThrowingStream<UploadEvent, Error>

    public func resumeUpload(logicalUploadID: String) async throws -> UploadResult

    public func listPendingUploads() async throws -> [PendingUploadInfo]

    public func listObjects(
        _ options: ListObjectsOptions = ListObjectsOptions(),
        authorizationHeader: String
    ) async throws -> ListObjectsPage

    public func abortUpload(logicalUploadID: String) async throws

    public func deleteLocalSnapshot(logicalUploadID: String) async throws
}
```

### 19.2 Device Initialization

```swift
public struct DeviceInitClientConfig: Sendable {
    public var configURL: URL
    public var endpointsURL: URL
    public var requestTimeout: Duration

    public init(
        configURL: URL,
        endpointsURL: URL,
        requestTimeout: Duration = .seconds(10)
    )
}

public actor ArchebaseDeviceInitializer {
    public init(config: DeviceInitClientConfig) throws
    public func initDevice(deviceID: String) async throws -> ArchebaseConfig
    public func reinitDevice(deviceID: String) async throws -> ArchebaseConfig
}

public struct ArchebaseConfig: Codable, Sendable, Equatable {
    public var apiKey: String
    public var tags: [String: String]
}
```

### 19.3 Object Types

```swift
public struct ListObjectsOptions: Sendable, Equatable {
    public var pageSize: Int32
    public var pageToken: String?
    public var filter: String?
}

public struct ListObjectsPage: Sendable, Equatable {
    public var objects: [DataObject]
    public var nextPageToken: String
}

public struct DataObject: Sendable, Equatable {
    public var objectID: String
    public var fileID: String
    public var status: DataObjectStatus
    public var sizeBytes: Int64
    public var createdAtUnix: Int64
    public var uploadedAtUnix: Int64
    public var verifiedAtUnix: Int64
    public var etag: String
}

public enum DataObjectStatus: Sendable, Equatable {
    case unspecified
    case created
    case uploaded
    case verified
    case bad
    case aborted
    case invalid
    case unrecognized(Int)
}
```

### 19.4 Upload Types

```swift
public struct UploadRequest: Sendable {
    public var fileURL: URL
    public var clientHints: [String: String]
    public var rawTags: [String: String]
    public var displayName: String?
}

public struct UploadResult: Sendable, Equatable {
    public var logicalUploadID: String
    public var uploadID: String
    public var bucket: String
    public var objectKey: String
    public var fileSize: UInt64
    public var ossObjectETag: String
}

public enum UploadEvent: Sendable, Equatable {
    case preparing
    case authenticating
    case creatingLogicalUpload
    case resuming(logicalUploadID: String)
    case initiatingMultipart(uploadID: String)
    case uploadingPart(partNumber: Int, sentBytes: UInt64, totalBytes: UInt64)
    case refreshingCredentials(uploadID: String)
    case reconcilingRemoteParts(uploadID: String)
    case completingMultipart(uploadID: String)
    case completingBusinessUpload(uploadID: String)
    case completed(UploadResult)
}
```

### 19.5 Pending Upload Types

```swift
public struct PendingUploadInfo: Sendable, Equatable {
    public var logicalUploadID: String
    public var uploadID: String
    public var fileURL: URL
    public var fileSize: UInt64
    public var phase: PersistedUploadPhase
    public var restartCount: Int
    public var updatedAt: Date
}

public enum PersistedUploadPhase: String, Codable, Sendable, Equatable {
    case sessionCreated
    case multipartInitiated
    case uploading
    case multipartCompleted
    case businessCompleting
    case terminalFailed
}
```

### 19.6 Configuration Types

```swift
public struct DataGatewayClientConfig: Sendable {
    public static func recommended(
        credentialBase64: String,
        persistRootURL: URL,
        endpointsURL: URL,
        observability: DataGatewayClientObservability = .disabled
    ) throws -> DataGatewayClientConfig

    public func validate() throws
}

public struct ClientRetryPolicy: Sendable, Equatable {
    public var maxAttempts: Int
    public var initialBackoff: Duration
    public var maxBackoff: Duration
}

public struct RetryPolicySet: Sendable, Equatable {
    public var controlPlane: ClientRetryPolicy
    public var dataPlane: ClientRetryPolicy
}

public struct UploadExecutionPolicy: Sendable {
    public var maxRestartCount: Int
    public var autoResumeByFileURL: Bool
    public var reconcileRemotePartsOnResume: Bool
    public var cleanupOnTerminalFailure: Bool
    public var credentialRefreshSkew: Duration
    public var persistence: LocalPersistencePolicy
}

public struct LocalPersistencePolicy: Sendable, Equatable {
    public var keepTerminalSnapshot: Bool
    public var keepCompletedSnapshot: Bool
    public var completedSnapshotTTL: Duration
    public var terminalSnapshotTTL: Duration
    public var copyExternalFileIntoManagedStaging: Bool
}
```

### 19.7 Observability Types

```swift
public struct DataGatewayClientObservability: Sendable {
    public var onLog: (@Sendable (DataGatewayClientLogEvent) async -> Void)?
    public var onMetric: (@Sendable (_ name: String, _ dimensions: [String: String]) async -> Void)?

    public static let disabled: DataGatewayClientObservability
}

public struct DataGatewayClientLogEvent: Sendable, Equatable {
    public var operation: String
    public var uploadID: String?
    public var logicalUploadID: String?
    public var phase: String?
    public var attempt: Int?
    public var statusCode: Int?
    public var detailCode: String?
    public var message: String
}
```

### 19.8 Error Type

```swift
public enum DataGatewayClientError: Error, Sendable, Equatable {
    case authenticationFailed(code: String?, message: String)
    case gatewayFailed(statusCode: Int, detailCode: String?, message: String)
    case invalidConfiguration(String)
    case alreadyInitialized(configURL: URL)
    case notInitialized(configURL: URL)
    case endpointsAlreadyInitialized(endpointsURL: URL)
    case endpointsNotInitialized(endpointsURL: URL)
    case invalidLocalFile(String)
    case zeroByteFile
    case ossFailed(httpStatus: Int?, ossCode: String?, message: String)
    case persistenceFailed(String)
    case rawTagConflict(key: String)
    case uploadRestartExceeded
    case resumeNotPossible(String)
    case integrityCheckFailed(String)
    case retryExhausted(lastError: String)
    case cancelled
}
```

## 20. 上线前检查清单

1. 确认 `archebase-endpoints.json` 已初始化到 App 私有目录，包含 `auth`、`gateway` 和 `deviceInit` 三组 endpoint，App 网络环境可以访问这些端点。
2. `archebase-endpoints.json` 和 `archebase-config.json` 写入 App 私有目录，不进入日志、备份导出或共享容器。
3. App 支持首次初始化、已初始化跳过、重新初始化和初始化失败提示。
4. 上传 UI 支持进度、成功、失败、重试、恢复和取消。
5. App 启动后会调用 `listPendingUploads()` 并处理待恢复任务。
6. 用户放弃任务时调用 `abortUpload(logicalUploadID:)`，而不是只删除本地记录。
7. 对 `notInitialized`、`rawTagConflict`、`resumeNotPossible`、`authenticationFailed`、`gatewayFailed`、`ossFailed` 有明确 UI 或日志策略。
8. 已在 Wi-Fi、蜂窝网络、弱网、断网恢复、前后台切换、锁屏恢复、App 强杀重启场景完成验证。
9. `clientHints` 和 `rawTags` 不包含密码、token、个人隐私或其他敏感信息。
10. 已接入 `DataGatewayClientObservability` 或等价日志，且日志经过脱敏和采样策略控制。

## 21. 快速问题定位

| 现象 | 优先检查 |
|---|---|
| 创建 client 时报 `notInitialized` | 是否已成功调用 `initDevice`，`configURL` 是否一致。 |
| 创建 client 时报 `endpointsNotInitialized` | 是否已获取可信 endpoint JSON 并调用 `DataGatewayClient.initialize(endpointsJSON:endpointsURL:)`，`endpointsURL` 是否一致。 |
| 初始化 endpoint 时报 `endpointsAlreadyInitialized` | 本机是否已有不同 endpoint 文件；不要自动覆盖，按运维流程确认是否需要清理或迁移。 |
| 创建 client 时报 `invalidConfiguration` | 配置文件 JSON、endpoint JSON、credential 和本地持久化路径是否有效。 |
| 上传立即失败 `zeroByteFile` | 用户选择的文件是否为空。 |
| 上传立即失败 `invalidLocalFile` | 文件是否仍存在，App 是否有访问权限。 |
| 上传失败 `rawTagConflict` | `UploadRequest.rawTags` 是否覆盖了设备配置 tags 的同名 key。 |
| 恢复失败 `resumeNotPossible` | 原始文件是否被删除、移动或修改，用户是否撤销文件访问权限或清理过 App 数据。 |
| 频繁 `authenticationFailed` | 设备是否需要重新初始化，凭证是否已被管理员轮换或撤销。 |
| 频繁 `ossFailed` 或 `retryExhausted` | 网络质量、代理、防火墙、系统时间和对象存储可用性。 |
| `uploadRestartExceeded` | 让用户重新上传，并保留 `logicalUploadID` 与 SDK 日志联系支持。 |
