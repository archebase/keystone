import Combine
import DataGatewayClient
import DGWControlPlane
import DGWStore
import Foundation

@MainActor
final class UploadDemoViewModel: ObservableObject {
    @Published var endpointsJSON: String = UploadDemoViewModel.sampleEndpointsJSON
    @Published var deviceID: String
    @Published var clientHintsJSON: String = """
    {
      "source": "dp-simulator"
    }
    """
    @Published var rawTagsJSON: String = """
    {
      "scene": "demo"
    }
    """

    @Published private(set) var paths: GatewayPaths = .appDefault
    @Published private(set) var endpointsAvailable = false
    @Published private(set) var configAvailable = false
    @Published private(set) var deviceReady = false
    @Published private(set) var pendingUploads: [PendingUploadInfo] = []
    @Published private(set) var selectedFileURL: URL?
    @Published private(set) var lastResult: UploadResult?
    @Published private(set) var deviceTags: [String: String] = [:]
    @Published private(set) var statusMessage = "正在检查本地配置..."
    @Published private(set) var uploadStatus = "空闲"
    @Published private(set) var uploadProgress: Double?
    @Published private(set) var isBusy = false
    @Published var errorMessage: String?

    private let service: GatewayUploadService
    private var didBootstrap = false
    #if DEBUG
    private var didRunLaunchSelfTest = false
    #endif
    private var uploadedBytesThisRun: UInt64 = 0

    init(service: GatewayUploadService = GatewayUploadService()) {
        self.service = service
        self.deviceID = Self.defaultDeviceID()
    }

    func bootstrap() async {
        guard !self.didBootstrap else {
            return
        }
        self.didBootstrap = true
        await self.refreshLocalState(showErrors: false)
    }

    func refreshLocalState(showErrors: Bool = true) async {
        let localState = await self.refreshStoredFileState()

        guard localState.endpointsExists else {
            self.deviceReady = false
            self.pendingUploads = []
            self.statusMessage = localState.configExists
                ? "检测到设备配置，但 endpoints 缺失。请先保存 endpoints JSON。"
                : "请先保存 endpoints JSON，再初始化设备。"
            return
        }

        guard localState.configExists else {
            self.deviceReady = false
            self.pendingUploads = []
            self.statusMessage = "已找到 endpoints，请输入 device ID 后初始化设备。"
            return
        }

        do {
            self.pendingUploads = try await self.service.listPendingUploads()
            self.deviceReady = true
            self.statusMessage = self.pendingUploads.isEmpty
                ? "已加载本地设备配置，可以直接上传。"
                : "已加载本地设备配置，发现 \(self.pendingUploads.count) 个待处理上传。"
        } catch {
            self.deviceReady = false
            self.pendingUploads = []
            self.statusMessage = "本地设备配置存在，但 SDK client 加载失败。"
            if showErrors {
                self.errorMessage = Self.describe(error)
            }
        }
    }

    func saveEndpoints() async {
        await self.withBusy {
            let json = self.endpointsJSON.trimmingCharacters(in: .whitespacesAndNewlines)
            guard !json.isEmpty else {
                throw DemoInputError("请输入 endpoints JSON。")
            }

            try await self.service.initializeEndpoints(json: json)
            let localState = await self.refreshStoredFileState()
            self.statusMessage = localState.configExists
                ? "Endpoints 已保存，已检测到本地设备配置。"
                : "Endpoints 已保存，可以初始化设备。"
        }
    }

    func fillSampleEndpoints() {
        self.endpointsJSON = Self.sampleEndpointsJSON
    }

    #if DEBUG
    func runLaunchSelfTestIfRequested() async {
        guard !self.didRunLaunchSelfTest else {
            return
        }
        guard ProcessInfo.processInfo.arguments.contains("--dp-simulator-self-test-init-device") else {
            return
        }

        self.didRunLaunchSelfTest = true
        self.deviceID = Self.launchArgumentValue(named: "--dp-simulator-device-id") ?? "260508-000001"
        await self.writeLaunchSelfTestResult(stage: "started", passed: false, error: nil)

        await self.saveEndpoints()
        if let errorMessage {
            await self.writeLaunchSelfTestResult(stage: "saveEndpoints", passed: false, error: errorMessage)
            return
        }

        await self.initializeDevice()
        if let errorMessage {
            await self.writeLaunchSelfTestResult(stage: "initializeDevice", passed: false, error: errorMessage)
            return
        }

        await self.refreshLocalState(showErrors: false)
        await self.writeLaunchSelfTestResult(stage: "completed", passed: self.deviceReady, error: nil)
    }
    #endif

    func initializeDevice() async {
        await self.withBusy {
            try await self.ensureEndpointsReady()
            let trimmedDeviceID = try self.validDeviceID()
            let tags = try await self.service.initializeDevice(deviceID: trimmedDeviceID)
            UserDefaults.standard.set(trimmedDeviceID, forKey: Self.deviceIDDefaultsKey)
            self.deviceTags = tags
            await self.refreshLocalState(showErrors: false)
            self.statusMessage = "设备初始化完成，后续启动会直接使用本地配置。"
        }
    }

    func reinitializeDevice() async {
        await self.withBusy {
            try await self.ensureEndpointsReady()
            let trimmedDeviceID = try self.validDeviceID()
            let tags = try await self.service.reinitializeDevice(deviceID: trimmedDeviceID)
            UserDefaults.standard.set(trimmedDeviceID, forKey: Self.deviceIDDefaultsKey)
            self.deviceTags = tags
            await self.refreshLocalState(showErrors: false)
            self.statusMessage = "设备已重新初始化，新上传会使用更新后的配置。"
        }
    }

    func selectFile(_ fileURL: URL?) {
        guard let fileURL else {
            return
        }
        self.selectedFileURL = fileURL
        self.uploadStatus = "已选择 \(fileURL.lastPathComponent)"
    }

    func createSampleFile() async {
        await self.withBusy {
            let fileURL = try await self.service.makeSampleFile()
            self.selectedFileURL = fileURL
            self.uploadStatus = "已生成样例文件 \(fileURL.lastPathComponent)"
        }
    }

    func uploadSelectedFile() async {
        await self.withBusy {
            guard let selectedFileURL else {
                throw DemoInputError("请先选择或生成一个要上传的文件。")
            }
            guard self.deviceReady else {
                throw DemoInputError("请先完成设备初始化，或等待应用加载已有设备配置。")
            }

            let clientHints = try Self.parseStringDictionary(
                self.clientHintsJSON,
                fieldName: "clientHints"
            )
            let rawTags = try Self.parseStringDictionary(
                self.rawTagsJSON,
                fieldName: "rawTags"
            )

            self.uploadedBytesThisRun = 0
            self.uploadProgress = 0
            self.lastResult = nil
            let stream = try await self.service.uploadEventStream(
                fileURL: selectedFileURL,
                clientHints: clientHints,
                rawTags: rawTags
            )

            var completedResult: UploadResult?
            for try await event in stream {
                self.apply(event)
                if case .completed(let result) = event {
                    completedResult = result
                }
            }

            if let completedResult {
                self.lastResult = completedResult
            }
            await self.refreshPendingUploads()
            if let completedResult {
                self.statusMessage = "上传完成：\(completedResult.logicalUploadID)"
            }
        }
    }

    func refreshPendingUploads() async {
        do {
            let localState = await self.refreshStoredFileState()
            guard localState.endpointsExists else {
                self.pendingUploads = []
                self.deviceReady = false
                self.statusMessage = "请先保存 endpoints JSON。"
                return
            }
            guard localState.configExists else {
                self.pendingUploads = []
                self.deviceReady = false
                self.statusMessage = "请先初始化设备。"
                return
            }
            self.pendingUploads = try await self.service.listPendingUploads()
            self.deviceReady = true
            self.statusMessage = self.pendingUploads.isEmpty
                ? "没有待恢复上传。"
                : "已刷新待恢复上传：\(self.pendingUploads.count) 个。"
        } catch {
            self.errorMessage = Self.describe(error)
        }
    }

    func resume(_ upload: PendingUploadInfo) async {
        await self.withBusy {
            self.uploadStatus = "正在恢复 \(upload.logicalUploadID)"
            let result = try await self.service.resumeUpload(logicalUploadID: upload.logicalUploadID)
            self.lastResult = result
            self.uploadProgress = 1
            self.statusMessage = "恢复完成：\(result.logicalUploadID)"
            await self.refreshPendingUploads()
        }
    }

    func resumeAllPending() async {
        await self.withBusy {
            let uploads = self.pendingUploads
            guard !uploads.isEmpty else {
                self.statusMessage = "没有待恢复上传。"
                return
            }

            var successCount = 0
            var failures: [String] = []
            for upload in uploads {
                do {
                    self.uploadStatus = "正在恢复 \(upload.logicalUploadID)"
                    self.lastResult = try await self.service.resumeUpload(logicalUploadID: upload.logicalUploadID)
                    successCount += 1
                } catch {
                    failures.append("\(upload.logicalUploadID): \(Self.describe(error))")
                }
            }

            await self.refreshPendingUploads()
            self.statusMessage = "批量恢复完成：成功 \(successCount) 个，失败 \(failures.count) 个。"
            if !failures.isEmpty {
                self.errorMessage = failures.joined(separator: "\n\n")
            }
        }
    }

    func abort(_ upload: PendingUploadInfo) async {
        await self.withBusy {
            try await self.service.abortUpload(logicalUploadID: upload.logicalUploadID)
            await self.refreshPendingUploads()
            self.statusMessage = "已取消上传：\(upload.logicalUploadID)"
        }
    }

    func abortAllPending() async {
        await self.withBusy {
            let uploads = self.pendingUploads
            guard !uploads.isEmpty else {
                self.statusMessage = "没有待取消上传。"
                return
            }

            var successCount = 0
            var failures: [String] = []
            for upload in uploads {
                do {
                    try await self.service.abortUpload(logicalUploadID: upload.logicalUploadID)
                    successCount += 1
                } catch {
                    failures.append("\(upload.logicalUploadID): \(Self.describe(error))")
                }
            }

            await self.refreshPendingUploads()
            self.statusMessage = "批量取消完成：成功 \(successCount) 个，失败 \(failures.count) 个。"
            if !failures.isEmpty {
                self.errorMessage = failures.joined(separator: "\n\n")
            }
        }
    }

    static func formatBytes(_ bytes: UInt64) -> String {
        let formatter = ByteCountFormatter()
        formatter.allowedUnits = [.useBytes, .useKB, .useMB, .useGB]
        formatter.countStyle = .file
        return formatter.string(fromByteCount: Int64(bytes))
    }

    static func describe(_ error: Error) -> String {
        if let inputError = error as? DemoInputError {
            return inputError.message
        }

        guard let sdkError = error as? DataGatewayClientError else {
            return error.localizedDescription
        }

        switch sdkError {
        case .authenticationFailed(let code, let message):
            return "认证失败 \(code ?? "UNKNOWN")：\(message)"
        case .gatewayFailed(let statusCode, let detailCode, let message):
            return "Gateway 请求失败 \(statusCode) \(detailCode ?? "UNKNOWN")：\(message)"
        case .invalidConfiguration(let message):
            return "配置无效：\(message)"
        case .alreadyInitialized(let configURL):
            return "设备已经初始化：\(configURL.path)。如需换绑请使用 Reinit Device。"
        case .notInitialized(let configURL):
            return "设备尚未初始化：\(configURL.path)。"
        case .endpointsAlreadyInitialized(let endpointsURL):
            return "Endpoints 已存在且内容不同：\(endpointsURL.path)。当前 demo 不会静默切换服务端。"
        case .endpointsNotInitialized(let endpointsURL):
            return "Endpoints 尚未初始化：\(endpointsURL.path)。"
        case .invalidLocalFile(let message):
            return "本地文件不可用：\(message)"
        case .zeroByteFile:
            return "不能上传 0 字节文件。"
        case .ossFailed(let httpStatus, let ossCode, let message):
            return "OSS 上传失败 \(httpStatus.map(String.init) ?? "-") \(ossCode ?? "UNKNOWN")：\(message)"
        case .persistenceFailed(let message):
            return "本地持久化失败：\(message)"
        case .rawTagConflict(let key):
            return "rawTags 与设备 tags 冲突：\(key)"
        case .uploadRestartExceeded:
            return "恢复上传时超过自动重建次数。"
        case .resumeNotPossible(let reason):
            return "无法恢复上传：\(reason)"
        case .integrityCheckFailed(let message):
            return "完整性校验失败：\(message)"
        case .retryExhausted(let lastError):
            return "重试耗尽：\(lastError)"
        case .cancelled:
            return "操作已取消。"
        }
    }

    private func ensureEndpointsReady() async throws {
        let localState = await self.refreshStoredFileState()
        if localState.endpointsExists {
            return
        }

        let json = self.endpointsJSON.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !json.isEmpty else {
            throw DemoInputError("请先输入 endpoints JSON。")
        }
        try await self.service.initializeEndpoints(json: json)
        _ = await self.refreshStoredFileState()
    }

    @discardableResult
    private func refreshStoredFileState() async -> GatewayLocalState {
        let localState = await self.service.localState()
        self.paths = localState.paths
        self.endpointsAvailable = localState.endpointsExists
        self.configAvailable = localState.configExists

        if let endpointsJSON = localState.endpointsJSON, !endpointsJSON.isEmpty {
            self.endpointsJSON = endpointsJSON
        }

        if !localState.endpointsExists || !localState.configExists {
            self.deviceReady = false
            self.pendingUploads = []
        } else {
            self.deviceReady = true
        }

        return localState
    }

    private func validDeviceID() throws -> String {
        let trimmed = self.deviceID.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else {
            throw DemoInputError("请输入 device ID。")
        }
        return trimmed
    }

    private func apply(_ event: UploadEvent) {
        switch event {
        case .preparing:
            self.uploadStatus = "准备本地文件"
            self.uploadProgress = 0
        case .authenticating:
            self.uploadStatus = "认证中"
        case .creatingLogicalUpload:
            self.uploadStatus = "创建逻辑上传"
        case .resuming(let logicalUploadID):
            self.uploadStatus = "恢复 \(logicalUploadID)"
        case .initiatingMultipart(let uploadID):
            self.uploadStatus = "初始化分片上传 \(uploadID)"
        case .uploadingPart(let partNumber, let sentBytes, let totalBytes):
            self.uploadedBytesThisRun += sentBytes
            if totalBytes > 0 {
                self.uploadProgress = min(Double(self.uploadedBytesThisRun) / Double(totalBytes), 0.99)
            }
            self.uploadStatus = "上传分片 \(partNumber)：\(Self.formatBytes(self.uploadedBytesThisRun)) / \(Self.formatBytes(totalBytes))"
        case .refreshingCredentials(let uploadID):
            self.uploadStatus = "刷新凭证 \(uploadID)"
        case .reconcilingRemoteParts(let uploadID):
            self.uploadStatus = "校验远端分片 \(uploadID)"
        case .completingMultipart(let uploadID):
            self.uploadStatus = "完成分片上传 \(uploadID)"
        case .completingBusinessUpload(let uploadID):
            self.uploadStatus = "完成业务上传 \(uploadID)"
        case .completed(let result):
            self.uploadProgress = 1
            self.lastResult = result
            self.uploadStatus = "上传完成 \(result.logicalUploadID)"
        }
    }

    private func withBusy(_ operation: () async throws -> Void) async {
        guard !self.isBusy else {
            return
        }
        self.isBusy = true
        self.errorMessage = nil
        defer {
            self.isBusy = false
        }

        do {
            try await operation()
        } catch let sdkError as DataGatewayClientError {
            if case .alreadyInitialized = sdkError {
                await self.refreshLocalState(showErrors: false)
            }
            self.errorMessage = Self.describe(sdkError)
        } catch {
            self.errorMessage = Self.describe(error)
        }
    }

    private static func parseStringDictionary(_ source: String, fieldName: String) throws -> [String: String] {
        let trimmed = source.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else {
            return [:]
        }

        do {
            let data = Data(trimmed.utf8)
            let object = try JSONSerialization.jsonObject(with: data)
            guard let dictionary = object as? [String: Any] else {
                throw DemoInputError("\(fieldName) 必须是 JSON object。")
            }

            var result: [String: String] = [:]
            for (key, value) in dictionary {
                guard let stringValue = value as? String else {
                    throw DemoInputError("\(fieldName).\(key) 的值必须是字符串。")
                }
                result[key] = stringValue
            }
            return result
        } catch let inputError as DemoInputError {
            throw inputError
        } catch {
            throw DemoInputError("\(fieldName) 不是合法 JSON：\(error.localizedDescription)")
        }
    }

    private static func defaultDeviceID() -> String {
        if let storedValue = UserDefaults.standard.string(forKey: Self.deviceIDDefaultsKey),
           !storedValue.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
            return storedValue
        }

        return ""
    }

    #if DEBUG
    private func writeLaunchSelfTestResult(stage: String, passed: Bool, error: String?) async {
        let localState = await self.service.localState()
        let result = LaunchSelfTestResult(
            stage: stage,
            passed: passed,
            error: error,
            statusMessage: self.statusMessage,
            deviceID: self.deviceID,
            endpointsAvailable: localState.endpointsExists,
            configAvailable: localState.configExists,
            deviceReady: self.deviceReady,
            endpointsURL: localState.paths.endpointsURL.path,
            configURL: localState.paths.configURL.path,
            timestamp: ISO8601DateFormatter().string(from: Date())
        )

        do {
            try FileManager.default.createDirectory(
                at: localState.paths.archebaseRootURL,
                withIntermediateDirectories: true
            )
            let data = try JSONEncoder().encode(result)
            let resultURL = localState.paths.archebaseRootURL.appendingPathComponent("launch-self-test-result.json")
            try data.write(to: resultURL, options: [.atomic])
        } catch {
            self.errorMessage = Self.describe(error)
        }
    }

    private static func launchArgumentValue(named name: String) -> String? {
        let arguments = ProcessInfo.processInfo.arguments
        guard let index = arguments.firstIndex(of: name) else {
            return nil
        }
        let valueIndex = arguments.index(after: index)
        guard valueIndex < arguments.endIndex else {
            return nil
        }
        return arguments[valueIndex]
    }
    #endif

    private static let deviceIDDefaultsKey = "dp-simulator.last-device-id"

    private static let sampleEndpointsJSON = """
    {
      "auth": {
        "scheme": "http",
        "host": "nlb-bz7li0ks67z1i7ii00.cn-shanghai.nlb.aliyuncsslb.com",
        "port": 50051
      },
      "gateway": {
        "scheme": "http",
        "host": "nlb-bz7li0ks67z1i7ii00.cn-shanghai.nlb.aliyuncsslb.com",
        "port": 50053
      },
      "deviceInit": {
        "scheme": "http",
        "host": "nlb-bz7li0ks67z1i7ii00.cn-shanghai.nlb.aliyuncsslb.com",
        "port": 50057
      }
    }
    """
}

#if DEBUG
private struct LaunchSelfTestResult: Encodable {
    let stage: String
    let passed: Bool
    let error: String?
    let statusMessage: String
    let deviceID: String
    let endpointsAvailable: Bool
    let configAvailable: Bool
    let deviceReady: Bool
    let endpointsURL: String
    let configURL: String
    let timestamp: String
}
#endif

struct DemoInputError: Error {
    let message: String

    init(_ message: String) {
        self.message = message
    }
}
