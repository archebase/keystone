import DataGatewayClient
import DGWControlPlane
import DGWStore
import Foundation

struct GatewayPaths: Sendable {
    let archebaseRootURL: URL
    let endpointsURL: URL
    let configURL: URL
    let persistRootURL: URL
    let demoFilesURL: URL

    nonisolated static var appDefault: GatewayPaths {
        let supportRoot = FileManager.default.urls(
            for: .applicationSupportDirectory,
            in: .userDomainMask
        )[0]
        let archebaseRoot = supportRoot
            .appendingPathComponent("Archebase", isDirectory: true)
            .standardizedFileURL

        return GatewayPaths(
            archebaseRootURL: archebaseRoot,
            endpointsURL: archebaseRoot.appendingPathComponent(ArchebasePublicEndpoints.endpointsFileName).standardizedFileURL,
            configURL: archebaseRoot.appendingPathComponent("archebase-config.json").standardizedFileURL,
            persistRootURL: archebaseRoot.appendingPathComponent("Uploads", isDirectory: true).standardizedFileURL,
            demoFilesURL: archebaseRoot.appendingPathComponent("Demo Files", isDirectory: true).standardizedFileURL
        )
    }
}

struct GatewayLocalState: Sendable {
    let paths: GatewayPaths
    let endpointsExists: Bool
    let configExists: Bool
    let endpointsJSON: String?
}

actor GatewayUploadService {
    private let paths: GatewayPaths
    private var client: DataGatewayClient?

    init(paths: GatewayPaths = .appDefault) {
        self.paths = paths
    }

    func localState() -> GatewayLocalState {
        let fileManager = FileManager.default
        let endpointsURL = self.paths.endpointsURL.standardizedFileURL
        let configURL = self.paths.configURL.standardizedFileURL
        let endpointsExists = fileManager.fileExists(atPath: endpointsURL.path)
        let configExists = fileManager.fileExists(atPath: configURL.path)
        let endpointsJSON = endpointsExists
            ? try? String(contentsOf: endpointsURL, encoding: .utf8)
            : nil

        return GatewayLocalState(
            paths: self.paths,
            endpointsExists: endpointsExists,
            configExists: configExists,
            endpointsJSON: endpointsJSON
        )
    }

    func initializeEndpoints(json: String) throws {
        do {
            try DataGatewayClient.initialize(
                endpointsJSON: json,
                endpointsURL: self.paths.endpointsURL
            )
        } catch let error as DataGatewayClientError {
            guard self.shouldFallbackPersistEndpoints(after: error) else {
                throw error
            }
            try self.writeEndpointsJSONFallback(json)
            try DataGatewayClient.initialize(
                endpointsJSON: json,
                endpointsURL: self.paths.endpointsURL
            )
        }
        self.client = nil
    }

    func initializeDevice(deviceID: String) async throws -> [String: String] {
        let initializer = try self.makeDeviceInitializer()
        let config: ArchebaseConfig
        do {
            config = try await initializer.initDevice(deviceID: deviceID)
        } catch let error as DataGatewayClientError {
            switch error {
            case .alreadyInitialized:
                config = try await self.loadExistingDeviceConfig()
            case .persistenceFailed where self.localState().configExists:
                config = try await self.loadExistingDeviceConfig()
            default:
                throw error
            }
        }
        self.client = try await self.makeClient()
        return config.tags
    }

    func reinitializeDevice(deviceID: String) async throws -> [String: String] {
        let initializer = try self.makeDeviceInitializer()
        let config = try await initializer.reinitDevice(deviceID: deviceID)
        self.client = try await self.makeClient()
        return config.tags
    }

    func listPendingUploads() async throws -> [PendingUploadInfo] {
        let client = try await self.loadClient()
        return try await client.listPendingUploads()
    }

    func uploadEventStream(
        fileURL: URL,
        clientHints: [String: String],
        rawTags: [String: String]
    ) async throws -> AsyncThrowingStream<UploadEvent, Error> {
        let client = try await self.loadClient()
        let request = UploadRequest(
            fileURL: fileURL,
            clientHints: clientHints,
            rawTags: rawTags,
            displayName: fileURL.lastPathComponent
        )
        return await client.uploadEvents(request)
    }

    func resumeUpload(logicalUploadID: String) async throws -> UploadResult {
        let client = try await self.loadClient()
        return try await client.resumeUpload(logicalUploadID: logicalUploadID)
    }

    func abortUpload(logicalUploadID: String) async throws {
        let client = try await self.loadClient()
        try await client.abortUpload(logicalUploadID: logicalUploadID)
    }

    func makeSampleFile() throws -> URL {
        try FileManager.default.createDirectory(
            at: self.paths.demoFilesURL,
            withIntermediateDirectories: true
        )

        let timestamp = ISO8601DateFormatter().string(from: Date())
        let payload: [String: Any] = [
            "source": "dp-simulator",
            "created_at": timestamp,
            "sequence": Int(Date().timeIntervalSince1970),
            "values": [
                "temperature": 21.6,
                "pressure": 101.3,
                "status": "demo",
            ],
        ]
        let data = try JSONSerialization.data(
            withJSONObject: payload,
            options: [.prettyPrinted, .sortedKeys]
        )
        let fileName = "dp-simulator-\(Int(Date().timeIntervalSince1970)).json"
        let fileURL = self.paths.demoFilesURL.appendingPathComponent(fileName)
        try data.write(to: fileURL, options: [.atomic])
        return fileURL
    }

    private func loadClient() async throws -> DataGatewayClient {
        if let client {
            return client
        }
        let client = try await self.makeClient()
        self.client = client
        return client
    }

    private func makeClient() async throws -> DataGatewayClient {
        try await DataGatewayClient.fromArchebaseConfig(
            configURL: self.paths.configURL,
            persistRootURL: self.paths.persistRootURL,
            endpointsURL: self.paths.endpointsURL,
            observability: .disabled
        )
    }

    private func makeDeviceInitializer() throws -> ArchebaseDeviceInitializer {
        try ArchebaseDeviceInitializer(
            config: DeviceInitClientConfig(
                configURL: self.paths.configURL,
                endpointsURL: self.paths.endpointsURL
            )
        )
    }

    private func loadExistingDeviceConfig() async throws -> ArchebaseConfig {
        let store = ArchebaseConfigStore(configURL: self.paths.configURL)
        return try await store.load()
    }

    private func shouldFallbackPersistEndpoints(after error: DataGatewayClientError) -> Bool {
        switch error {
        case .endpointsNotInitialized:
            return true
        case .persistenceFailed:
            return true
        default:
            return false
        }
    }

    private func writeEndpointsJSONFallback(_ json: String) throws {
        let endpointsURL = self.paths.endpointsURL.standardizedFileURL
        let parent = endpointsURL.deletingLastPathComponent()
        try FileManager.default.createDirectory(at: parent, withIntermediateDirectories: true)
        #if os(iOS)
        let options: Data.WritingOptions = [.atomic, .completeFileProtectionUnlessOpen]
        #else
        let options: Data.WritingOptions = [.atomic]
        #endif
        try Data(json.utf8).write(to: endpointsURL, options: options)
    }
}
