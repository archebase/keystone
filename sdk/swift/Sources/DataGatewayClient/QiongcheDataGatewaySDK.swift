import DGWControlPlane
import DGWStore
import Foundation

package protocol QiongcheSDKClock: Sendable {
    func now() async -> Date
}

package struct SystemQiongcheSDKClock: QiongcheSDKClock {
    package init() {}

    package func now() async -> Date {
        Date()
    }
}

package protocol QiongcheLocalPersisting: Sendable {
    func replaceEndpoints(endpointsJSON: String, endpointsURL: URL) async throws
    func replaceConfig(_ config: ArchebaseConfig, configURL: URL) async throws
    func replaceState(_ state: QiongcheSDKState, stateURL: URL) async throws
}

package struct DefaultQiongcheLocalPersister: QiongcheLocalPersisting {
    package init() {}

    package func replaceEndpoints(endpointsJSON: String, endpointsURL: URL) async throws {
        try ArchebasePublicEndpoints.replace(endpointsJSON: endpointsJSON, endpointsURL: endpointsURL)
    }

    package func replaceConfig(_ config: ArchebaseConfig, configURL: URL) async throws {
        try await ArchebaseConfigStore(configURL: configURL)
            .replaceOrInitialize(config)
    }

    package func replaceState(_ state: QiongcheSDKState, stateURL: URL) async throws {
        try QiongcheSDKStateStore(stateURL: stateURL).replace(state)
    }
}

public actor QiongcheDataGatewaySDK {
    private let paths: QiongcheSDKPaths
    private let stateStore: QiongcheSDKStateStore
    private let deviceProvisioner: any QiongcheDeviceProvisioning
    private let readinessProbe: (any QiongcheEndpointProbing)?
    private let localPersister: any QiongcheLocalPersisting
    private let clock: any QiongcheSDKClock
    private let deviceInitTimeout: Duration
    private let readinessTimeout: Duration

    public init(
        rootURL: URL? = nil,
        deviceInitTimeout: Duration = .seconds(10),
        readinessTimeout: Duration = .seconds(3)
    ) throws {
        try self.init(
            rootURL: rootURL,
            deviceInitTimeout: deviceInitTimeout,
            readinessTimeout: readinessTimeout,
            deviceProvisioner: DefaultQiongcheDeviceProvisioner(),
            readinessProbe: DefaultQiongcheEndpointProbe(),
            localPersister: DefaultQiongcheLocalPersister(),
            clock: SystemQiongcheSDKClock()
        )
    }

    package init(
        rootURL: URL? = nil,
        deviceInitTimeout: Duration = .seconds(10),
        readinessTimeout: Duration = .seconds(3),
        deviceProvisioner: any QiongcheDeviceProvisioning,
        readinessProbe: (any QiongcheEndpointProbing)? = nil,
        localPersister: any QiongcheLocalPersisting = DefaultQiongcheLocalPersister(),
        clock: any QiongcheSDKClock = SystemQiongcheSDKClock()
    ) throws {
        let paths = try QiongcheSDKPaths(rootURL: rootURL)
        self.paths = paths
        self.stateStore = QiongcheSDKStateStore(stateURL: paths.stateURL)
        self.deviceProvisioner = deviceProvisioner
        self.readinessProbe = readinessProbe
        self.localPersister = localPersister
        self.clock = clock
        self.deviceInitTimeout = deviceInitTimeout
        self.readinessTimeout = readinessTimeout
    }

    public func saveConfigAndInit(configString: String) async throws {
        let parsed = try QiongcheConfigParser.parse(configString)
        let remoteConfig = try await self.deviceProvisioner.initDevice(
            deviceID: parsed.deviceID,
            deviceInitEndpoint: parsed.resolvedEndpoints.deviceInit,
            tls: parsed.resolvedEndpoints.deviceInitTLS,
            timeout: self.deviceInitTimeout
        )

        // A successful remote init/reinit can invalidate the previous credential.
        // Keep readiness false until endpoints, config, and state all commit again.
        try self.stateStore.removeIfExists()

        try await self.localPersister.replaceEndpoints(
            endpointsJSON: parsed.normalizedEndpointsJSONString,
            endpointsURL: self.paths.endpointsURL
        )

        try await self.localPersister.replaceConfig(remoteConfig, configURL: self.paths.configURL)

        let now = await self.clock.now()
        let state = try QiongcheSDKState(
            deviceID: parsed.deviceID,
            endpointsSHA256: parsed.endpointsSHA256Hex,
            initializedAtUnix: Int64(now.timeIntervalSince1970)
        )
        try await self.localPersister.replaceState(state, stateURL: self.paths.stateURL)
    }

    public func isReadyToUpload() async -> Bool {
        guard let readinessProbe = self.readinessProbe else {
            return false
        }

        do {
            let state = try self.stateStore.load()
            let endpointsData = try Data(contentsOf: self.paths.endpointsURL)
            guard state.endpointsSHA256 == QiongcheConfigParser.sha256Hex(endpointsData) else {
                return false
            }
            _ = try await ArchebaseConfigStore(configURL: self.paths.configURL).load()
            let endpoints = try ArchebasePublicEndpoints.load(endpointsURL: self.paths.endpointsURL)

            async let authReachable = readinessProbe.authEndpointReachable(
                endpoint: endpoints.auth,
                tls: endpoints.authTLS,
                timeout: self.readinessTimeout
            )
            async let gatewayReachable = readinessProbe.gatewayEndpointReachable(
                endpoint: endpoints.gateway,
                tls: endpoints.gatewayTLS,
                timeout: self.readinessTimeout
            )

            let auth = await authReachable
            let gateway = await gatewayReachable
            return auth && gateway
        } catch {
            return false
        }
    }
}

package struct QiongcheSDKPaths: Sendable, Equatable {
    package let rootURL: URL
    package let endpointsURL: URL
    package let configURL: URL
    package let stateURL: URL
    package let persistRootURL: URL

    package init(rootURL: URL? = nil) throws {
        let root = try rootURL ?? Self.defaultRootURL()
        self.rootURL = root.standardizedFileURL
        self.endpointsURL = self.rootURL
            .appendingPathComponent(ArchebasePublicEndpoints.endpointsFileName, isDirectory: false)
            .standardizedFileURL
        self.configURL = self.rootURL
            .appendingPathComponent("archebase-config.json", isDirectory: false)
            .standardizedFileURL
        self.stateURL = self.rootURL
            .appendingPathComponent("qiongche-sdk-state.json", isDirectory: false)
            .standardizedFileURL
        self.persistRootURL = self.rootURL
            .appendingPathComponent("Uploads", isDirectory: true)
            .standardizedFileURL
    }

    private static func defaultRootURL() throws -> URL {
        guard let applicationSupport = FileManager.default.urls(
            for: .applicationSupportDirectory,
            in: .userDomainMask
        ).first else {
            throw DataGatewayClientError.invalidConfiguration("application support directory is unavailable")
        }

        return applicationSupport
            .appendingPathComponent("Archebase", isDirectory: true)
            .standardizedFileURL
    }
}

package struct QiongcheSDKState: Codable, Sendable, Equatable {
    package static let currentVersion = 1

    package var version: Int
    package var deviceID: String
    package var endpointsSHA256: String
    package var initializedAtUnix: Int64

    enum CodingKeys: String, CodingKey {
        case version
        case deviceID = "device_id"
        case endpointsSHA256 = "endpoints_sha256"
        case initializedAtUnix = "initialized_at_unix"
    }

    package init(
        version: Int = Self.currentVersion,
        deviceID: String,
        endpointsSHA256: String,
        initializedAtUnix: Int64
    ) throws {
        self.version = version
        self.deviceID = deviceID
        self.endpointsSHA256 = endpointsSHA256
        self.initializedAtUnix = initializedAtUnix
        try self.validate()
    }

    package func validate() throws {
        guard self.version == Self.currentVersion else {
            throw DataGatewayClientError.invalidConfiguration("qiongche sdk state version is unsupported")
        }
        guard !self.deviceID.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else {
            throw DataGatewayClientError.invalidConfiguration("qiongche sdk state device_id must not be empty")
        }
        guard !self.deviceID.unicodeScalars.contains(where: { $0.properties.generalCategory == .control }) else {
            throw DataGatewayClientError.invalidConfiguration("qiongche sdk state device_id contains unsupported control characters")
        }
        guard self.endpointsSHA256.count == 64,
              self.endpointsSHA256.allSatisfy({ $0.isHexDigit }) else {
            throw DataGatewayClientError.invalidConfiguration("qiongche sdk state endpoints_sha256 is invalid")
        }
        guard self.initializedAtUnix > 0 else {
            throw DataGatewayClientError.invalidConfiguration("qiongche sdk state initialized_at_unix is invalid")
        }
    }
}

package struct QiongcheSDKStateStore {
    private let stateURL: URL
    private let fileManager: FileManager

    package init(stateURL: URL, fileManager: FileManager = .default) {
        self.stateURL = stateURL.standardizedFileURL
        self.fileManager = fileManager
    }

    package func load() throws -> QiongcheSDKState {
        guard self.fileManager.fileExists(atPath: self.stateURL.path) else {
            throw DataGatewayClientError.notInitialized(configURL: self.stateURL)
        }

        do {
            let state = try Self.decoder.decode(QiongcheSDKState.self, from: Data(contentsOf: self.stateURL))
            try state.validate()
            return state
        } catch let error as DataGatewayClientError {
            throw error
        } catch {
            throw DataGatewayClientError.invalidConfiguration(
                "failed to load qiongche sdk state: \(error.localizedDescription)"
            )
        }
    }

    package func replace(_ state: QiongcheSDKState) throws {
        try state.validate()
        let data = try Self.encoder.encode(state)
        do {
            try AtomicFileWriter.write(data, to: self.stateURL, fileManager: self.fileManager) { temporaryURL, destination, fileManager in
                try AtomicFileWriter.replaceOrMoveTemporaryItem(temporaryURL, to: destination, fileManager: fileManager)
            }
            let loaded = try self.load()
            guard loaded == state else {
                throw DataGatewayClientError.persistenceFailed("qiongche sdk state verification failed after write")
            }
        } catch let error as DataGatewayClientError {
            throw error
        } catch {
            throw DataGatewayClientError.persistenceFailed("failed to write qiongche sdk state: \(error.localizedDescription)")
        }
    }

    package func removeIfExists() throws {
        guard self.fileManager.fileExists(atPath: self.stateURL.path) else {
            return
        }
        do {
            try self.fileManager.removeItem(at: self.stateURL)
        } catch {
            throw DataGatewayClientError.persistenceFailed(
                "failed to remove qiongche sdk state: \(error.localizedDescription)"
            )
        }
    }

    private static let encoder: JSONEncoder = {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        return encoder
    }()

    private static let decoder = JSONDecoder()
}
