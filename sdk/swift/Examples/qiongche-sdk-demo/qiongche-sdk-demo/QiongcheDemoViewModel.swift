import Combine
import DataGatewayClient
import DGWControlPlane
import Foundation

@MainActor
final class QiongcheDemoViewModel: ObservableObject {
    @Published var configString: String
    @Published private(set) var localState: QiongcheDemoLocalState
    @Published private(set) var ready: Bool?
    @Published private(set) var lastReadyCheck: Date?
    @Published private(set) var sampleFileURL: URL?
    @Published private(set) var uploadEvents: [String] = []
    @Published private(set) var resultSummary: String?
    @Published private(set) var statusMessage = "正在检查本地状态..."
    @Published private(set) var isBusy = false
    @Published var errorMessage: String?

    private let service: QiongcheDemoService
    private var didBootstrap = false

    init(service: QiongcheDemoService = QiongcheDemoService()) {
        self.service = service
        self.configString = QiongcheDemoViewModel.sampleConfigString
        self.localState = QiongcheDemoStateReader.read(paths: .appDefault)
    }

    func bootstrap() async {
        guard !self.didBootstrap else {
            return
        }
        self.didBootstrap = true
        await self.refreshLocalState()
    }

    func refreshLocalState() async {
        self.localState = await self.service.localState()
        self.statusMessage = self.stateSummary(self.localState)
    }

    func fillSampleConfig() {
        self.configString = Self.sampleConfigString
    }

    func saveConfigAndInit() async {
        await self.withBusy {
            let trimmed = self.configString.trimmingCharacters(in: .whitespacesAndNewlines)
            guard !trimmed.isEmpty else {
                throw QiongcheDemoInputError("请输入配置 JSON。")
            }
            try await self.service.saveConfigAndInit(trimmed)
            self.ready = nil
            await self.refreshLocalState()
            self.statusMessage = "配置已保存，设备初始化完成。"
        }
    }

    func checkReady() async {
        await self.withBusy {
            let value = await self.service.checkReady()
            self.ready = value
            self.lastReadyCheck = Date()
            self.statusMessage = value ? "Ready" : "Not Ready"
        }
    }

    func makeSampleFile() async {
        await self.withBusy {
            let fileURL = try await self.service.makeSampleFile()
            self.sampleFileURL = fileURL
            self.uploadEvents = ["已生成 \(fileURL.lastPathComponent)"]
            self.resultSummary = nil
        }
    }

    func uploadSampleFile() async {
        await self.withBusy {
            guard let sampleFileURL else {
                throw QiongcheDemoInputError("请先生成样例文件。")
            }
            self.uploadEvents = ["开始上传 \(sampleFileURL.lastPathComponent)"]
            self.resultSummary = nil
            let stream = try await self.service.uploadSampleFile(sampleFileURL)
            for try await event in stream {
                self.uploadEvents.append(Self.describe(event))
                if case .completed(let result) = event {
                    self.resultSummary = "logicalUploadID: \(result.logicalUploadID)"
                }
            }
            await self.refreshLocalState()
        }
    }

    private func withBusy(_ operation: () async throws -> Void) async {
        guard !self.isBusy else {
            return
        }
        self.isBusy = true
        defer {
            self.isBusy = false
        }

        do {
            try await operation()
        } catch {
            self.errorMessage = Self.describe(error)
        }
    }

    private func stateSummary(_ state: QiongcheDemoLocalState) -> String {
        if let stateReadError = state.stateReadError {
            return stateReadError
        }
        if state.endpointsExists, state.configExists {
            return "本地配置存在。"
        }
        if state.endpointsExists {
            return "endpoint 已存在，设备配置缺失。"
        }
        if state.configExists {
            return "设备配置存在，endpoint 缺失。"
        }
        return "尚未保存穹彻配置。"
    }

    static func describe(_ error: any Error) -> String {
        if let error = error as? QiongcheSDKError {
            switch error {
            case .invalidConfigString(let message):
                return message
            }
        }
        if let error = error as? DataGatewayClientError {
            switch error {
            case .persistenceFailed(let message), .invalidConfiguration(let message):
                return message
            case .authenticationFailed(_, let message), .gatewayFailed(_, _, let message):
                return message
            default:
                return String(describing: error)
            }
        }
        if let error = error as? QiongcheDemoInputError {
            return error.message
        }
        return error.localizedDescription
    }

    static func describe(_ event: UploadEvent) -> String {
        switch event {
        case .preparing:
            return "preparing"
        case .authenticating:
            return "authenticating"
        case .creatingLogicalUpload:
            return "creating logical upload"
        case .resuming(let logicalUploadID):
            return "resuming \(logicalUploadID)"
        case .initiatingMultipart(let uploadID):
            return "multipart \(uploadID)"
        case .uploadingPart(let partNumber, let sentBytes, let totalBytes):
            return "part \(partNumber): \(sentBytes)/\(totalBytes)"
        case .refreshingCredentials(let uploadID):
            return "refreshing \(uploadID)"
        case .reconcilingRemoteParts(let uploadID):
            return "reconciling \(uploadID)"
        case .completingMultipart(let uploadID):
            return "completing multipart \(uploadID)"
        case .completingBusinessUpload(let uploadID):
            return "completing upload \(uploadID)"
        case .completed:
            return "completed"
        }
    }

    static let sampleConfigString = """
    {
      "device_id": "robot-001",
      "auth": { "scheme": "http", "host": "localhost", "port": 50051 },
      "gateway": { "scheme": "http", "host": "localhost", "port": 50053 },
      "deviceInit": { "scheme": "http", "host": "localhost", "port": 50057 }
    }
    """
}

struct QiongcheDemoInputError: Error {
    let message: String

    init(_ message: String) {
        self.message = message
    }
}
