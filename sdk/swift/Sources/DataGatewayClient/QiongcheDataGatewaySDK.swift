// SPDX-FileCopyrightText: 2026 ArcheBase
// SPDX-License-Identifier: MulanPSL-2.0

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

public struct QiongcheUploadReadinessReport: Sendable, Equatable {
    public let ready: Bool
    public let stateLoaded: Bool
    public let endpointsLoaded: Bool
    public let endpointsHashMatches: Bool?
    public let configLoaded: Bool
    public let authReachable: Bool?
    public let gatewayReachable: Bool?
    public let failure: String?

    public init(
        ready: Bool,
        stateLoaded: Bool,
        endpointsLoaded: Bool,
        endpointsHashMatches: Bool?,
        configLoaded: Bool,
        authReachable: Bool?,
        gatewayReachable: Bool?,
        failure: String?
    ) {
        self.ready = ready
        self.stateLoaded = stateLoaded
        self.endpointsLoaded = endpointsLoaded
        self.endpointsHashMatches = endpointsHashMatches
        self.configLoaded = configLoaded
        self.authReachable = authReachable
        self.gatewayReachable = gatewayReachable
        self.failure = failure
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
            deviceName: parsed.deviceName,
            deviceAuthToken: parsed.deviceAuthToken,
            deviceInitEndpoint: parsed.resolvedEndpoints.deviceInit,
            tls: parsed.resolvedEndpoints.deviceInitTLS,
            timeout: self.deviceInitTimeout
        )
        let resolvedDeviceID = try Self.resolvedDeviceID(from: remoteConfig)

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
            deviceID: resolvedDeviceID,
            endpointsSHA256: parsed.endpointsSHA256Hex,
            initializedAtUnix: Int64(now.timeIntervalSince1970)
        )
        try await self.localPersister.replaceState(state, stateURL: self.paths.stateURL)
    }

    private static func resolvedDeviceID(from config: ArchebaseConfig) throws -> String {
        guard let deviceID = config.tags["device_id"]?.trimmingCharacters(in: .whitespacesAndNewlines),
              !deviceID.isEmpty else {
            throw DataGatewayClientError.invalidConfiguration("device_id is missing from device initialization response")
        }
        return deviceID
    }

    public func isReadyToUpload() async -> Bool {
        await self.uploadReadinessReport().ready
    }

    public func uploadReadinessReport() async -> QiongcheUploadReadinessReport {
        guard let readinessProbe = self.readinessProbe else {
            return QiongcheUploadReadinessReport(
                ready: false,
                stateLoaded: false,
                endpointsLoaded: false,
                endpointsHashMatches: nil,
                configLoaded: false,
                authReachable: nil,
                gatewayReachable: nil,
                failure: "readiness_probe_unavailable"
            )
        }

        do {
            let state = try self.stateStore.load()
            let endpointsData = try Data(contentsOf: self.paths.endpointsURL)
            let endpointsHashMatches = state.endpointsSHA256 == QiongcheConfigParser.sha256Hex(endpointsData)
            guard endpointsHashMatches else {
                return QiongcheUploadReadinessReport(
                    ready: false,
                    stateLoaded: true,
                    endpointsLoaded: true,
                    endpointsHashMatches: false,
                    configLoaded: false,
                    authReachable: nil,
                    gatewayReachable: nil,
                    failure: "endpoints_hash_mismatch"
                )
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
            return QiongcheUploadReadinessReport(
                ready: auth && gateway,
                stateLoaded: true,
                endpointsLoaded: true,
                endpointsHashMatches: true,
                configLoaded: true,
                authReachable: auth,
                gatewayReachable: gateway,
                failure: auth && gateway ? nil : "endpoint_unreachable"
            )
        } catch {
            return QiongcheUploadReadinessReport(
                ready: false,
                stateLoaded: self.fileExists(self.paths.stateURL),
                endpointsLoaded: self.fileExists(self.paths.endpointsURL),
                endpointsHashMatches: nil,
                configLoaded: false,
                authReachable: nil,
                gatewayReachable: nil,
                failure: Self.readinessFailureDescription(error)
            )
        }
    }

    private func fileExists(_ url: URL) -> Bool {
        FileManager.default.fileExists(atPath: url.path)
    }

    private static func readinessFailureDescription(_ error: any Error) -> String {
        if let dataGatewayError = error as? DataGatewayClientError {
            return "data_gateway_error_\(Self.dataGatewayErrorCode(dataGatewayError))"
        }
        return "unexpected_error_\(String(describing: type(of: error)))"
    }

    private static func dataGatewayErrorCode(_ error: DataGatewayClientError) -> String {
        switch error {
        case .authenticationFailed(let code, _):
            return "authentication_failed_\(code ?? "unknown")"
        case .gatewayFailed(let statusCode, let detailCode, _):
            return "gateway_failed_\(statusCode)_\(detailCode ?? "unknown")"
        case .invalidConfiguration:
            return "invalid_configuration"
        case .alreadyInitialized:
            return "already_initialized"
        case .notInitialized:
            return "not_initialized"
        case .endpointsAlreadyInitialized:
            return "endpoints_already_initialized"
        case .endpointsNotInitialized:
            return "endpoints_not_initialized"
        case .invalidLocalFile:
            return "invalid_local_file"
        case .zeroByteFile:
            return "zero_byte_file"
        case .ossFailed(let httpStatus, let ossCode, _):
            return "oss_failed_\(httpStatus.map(String.init) ?? "unknown")_\(ossCode ?? "unknown")"
        case .persistenceFailed:
            return "persistence_failed"
        case .rawTagConflict(let key):
            return "raw_tag_conflict_\(key)"
        case .uploadRestartExceeded:
            return "upload_restart_exceeded"
        case .resumeNotPossible:
            return "resume_not_possible"
        case .integrityCheckFailed:
            return "integrity_check_failed"
        case .retryExhausted:
            return "retry_exhausted"
        case .cancelled:
            return "cancelled"
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
