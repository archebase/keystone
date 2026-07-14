import Crypto
import DGWAuth
import DGWControlPlane
import DGWOss
import DGWProto
import DGWStore
import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2

/// Stable module marker for the public client target.
public enum DataGatewayClientModule {
    public static let name = "DataGatewayClient"

    /// SDK version reported to the device initialization service.
    public static let version = "0.1.4"
}

/// Public configuration for device initialization and reinitialization.
public struct DeviceInitClientConfig: Sendable {
    public var configURL: URL
    public var endpointsURL: URL
    public var requestTimeout: Duration
    package var tls: TLSMode?

    /// Creates a device initialization configuration that loads the runtime public endpoint.
    public init(
        configURL: URL,
        endpointsURL: URL,
        requestTimeout: Duration = .seconds(10)
    ) {
        self.configURL = configURL
        self.endpointsURL = endpointsURL
        self.requestTimeout = requestTimeout
        self.tls = nil
    }

    package init(
        configURL: URL,
        requestTimeout: Duration = .seconds(10),
        tls: TLSMode
    ) {
        self.configURL = configURL
        self.endpointsURL = configURL
            .deletingLastPathComponent()
            .appendingPathComponent(ArchebasePublicEndpoints.endpointsFileName)
            .standardizedFileURL
        self.requestTimeout = requestTimeout
        self.tls = tls
    }
}

package enum TLSMode: Sendable, Equatable {
    case plaintext
    case tls
}

/// Standardized observability event emitted by the client.
public struct DataGatewayClientLogEvent: Sendable, Equatable {
    public var operation: String
    public var uploadID: String?
    public var logicalUploadID: String?
    public var phase: String?
    public var attempt: Int?
    public var statusCode: Int?
    public var detailCode: String?
    public var message: String

    public init(
        operation: String,
        uploadID: String? = nil,
        logicalUploadID: String? = nil,
        phase: String? = nil,
        attempt: Int? = nil,
        statusCode: Int? = nil,
        detailCode: String? = nil,
        message: String
    ) {
        self.operation = operation
        self.uploadID = uploadID
        self.logicalUploadID = logicalUploadID
        self.phase = phase
        self.attempt = attempt
        self.statusCode = statusCode
        self.detailCode = detailCode
        self.message = message
    }
}

/// Observability hook bundle for logs and metrics-style events.
public struct DataGatewayClientObservability: Sendable {
    public var onLog: (@Sendable (DataGatewayClientLogEvent) async -> Void)?
    public var onMetric: (@Sendable (_ name: String, _ dimensions: [String: String]) async -> Void)?

    public init(
        onLog: (@Sendable (DataGatewayClientLogEvent) async -> Void)? = nil,
        onMetric: (@Sendable (_ name: String, _ dimensions: [String: String]) async -> Void)? = nil
    ) {
        self.onLog = onLog
        self.onMetric = onMetric
    }

    /// A no-op observability configuration.
    public static let disabled = DataGatewayClientObservability()
}

/// Persistence knobs that affect local snapshot retention and direct-file upload behavior.
public struct LocalPersistencePolicy: Sendable, Equatable {
    public var keepTerminalSnapshot: Bool
    public var keepCompletedSnapshot: Bool
    public var completedSnapshotTTL: Duration
    public var terminalSnapshotTTL: Duration
    /// Compatibility flag retained for callers that already set it; new uploads always read the source file directly.
    public var copyExternalFileIntoManagedStaging: Bool

    public init(
        keepTerminalSnapshot: Bool,
        keepCompletedSnapshot: Bool,
        completedSnapshotTTL: Duration,
        terminalSnapshotTTL: Duration,
        copyExternalFileIntoManagedStaging: Bool
    ) {
        self.keepTerminalSnapshot = keepTerminalSnapshot
        self.keepCompletedSnapshot = keepCompletedSnapshot
        self.completedSnapshotTTL = completedSnapshotTTL
        self.terminalSnapshotTTL = terminalSnapshotTTL
        self.copyExternalFileIntoManagedStaging = copyExternalFileIntoManagedStaging
    }

    /// Recommended defaults for iOS resume and snapshot retention.
    public static let recommended = LocalPersistencePolicy(
        keepTerminalSnapshot: true,
        keepCompletedSnapshot: false,
        completedSnapshotTTL: .seconds(0),
        terminalSnapshotTTL: .seconds(3600),
        copyExternalFileIntoManagedStaging: false
    )
}

/// Execution policy for one upload orchestration.
public struct UploadExecutionPolicy: Sendable {
    public var maxRestartCount: Int
    public var autoResumeByFileURL: Bool
    public var reconcileRemotePartsOnResume: Bool
    public var cleanupOnTerminalFailure: Bool
    public var credentialRefreshSkew: Duration
    public var persistence: LocalPersistencePolicy

    public init(
        maxRestartCount: Int,
        autoResumeByFileURL: Bool,
        reconcileRemotePartsOnResume: Bool,
        cleanupOnTerminalFailure: Bool,
        credentialRefreshSkew: Duration,
        persistence: LocalPersistencePolicy
    ) {
        self.maxRestartCount = maxRestartCount
        self.autoResumeByFileURL = autoResumeByFileURL
        self.reconcileRemotePartsOnResume = reconcileRemotePartsOnResume
        self.cleanupOnTerminalFailure = cleanupOnTerminalFailure
        self.credentialRefreshSkew = credentialRefreshSkew
        self.persistence = persistence
    }

    /// Recommended execution defaults for restart, reconcile, and local cleanup.
    public static let recommended = UploadExecutionPolicy(
        maxRestartCount: 3,
        autoResumeByFileURL: true,
        reconcileRemotePartsOnResume: true,
        cleanupOnTerminalFailure: true,
        credentialRefreshSkew: .seconds(30),
        persistence: .recommended
    )
}

/// Public upload request envelope.
public struct UploadRequest: Sendable {
    public var fileURL: URL
    public var clientHints: [String: String]
    public var rawTags: [String: String]
    public var displayName: String?

    public init(
        fileURL: URL,
        clientHints: [String: String],
        rawTags: [String: String],
        displayName: String?
    ) {
        self.fileURL = fileURL
        self.clientHints = clientHints
        self.rawTags = rawTags
        self.displayName = displayName
    }
}

/// Public retry configuration for one transport tier.
public struct ClientRetryPolicy: Sendable, Equatable {
    public var maxAttempts: Int
    public var initialBackoff: Duration
    public var maxBackoff: Duration

    public init(maxAttempts: Int, initialBackoff: Duration, maxBackoff: Duration) {
        self.maxAttempts = maxAttempts
        self.initialBackoff = initialBackoff
        self.maxBackoff = maxBackoff
    }

    /// Recommended retry defaults for control-plane RPCs.
    public static let controlPlaneRecommended = ClientRetryPolicy(
        maxAttempts: 5,
        initialBackoff: Duration(secondsComponent: 0, attosecondsComponent: 500_000_000_000_000_000),
        maxBackoff: .seconds(8)
    )

    /// Recommended retry defaults for OSS data-plane operations.
    public static let dataPlaneRecommended = ClientRetryPolicy(
        maxAttempts: 8,
        initialBackoff: .seconds(1),
        maxBackoff: .seconds(30)
    )

    package var controlPlaneValue: DGWControlPlane.RetryPolicy {
        DGWControlPlane.RetryPolicy(
            maxAttempts: self.maxAttempts,
            initialBackoff: self.initialBackoff,
            maxBackoff: self.maxBackoff
        )
    }
}

/// Public retry policy bundle for control plane and OSS data plane.
public struct RetryPolicySet: Sendable, Equatable {
    public var controlPlane: ClientRetryPolicy
    public var dataPlane: ClientRetryPolicy

    public init(controlPlane: ClientRetryPolicy, dataPlane: ClientRetryPolicy) {
        self.controlPlane = controlPlane
        self.dataPlane = dataPlane
    }

    /// Recommended retry defaults aligned with the design document.
    public static let recommended = RetryPolicySet(
        controlPlane: .controlPlaneRecommended,
        dataPlane: .dataPlaneRecommended
    )
}

/// Top-level configuration used to construct the public client.
public struct DataGatewayClientConfig: Sendable {
    package var authEndpoint: URL
    package var gatewayEndpoint: URL
    public var credentialBase64: String
    public var authRefreshBefore: Duration
    public var requestTimeout: Duration
    public var persistRootURL: URL
    public var retryPolicy: RetryPolicySet
    public var execution: UploadExecutionPolicy
    package var authTLS: TLSMode
    package var gatewayTLS: TLSMode
    package var tls: TLSMode { self.authTLS }
    public var observability: DataGatewayClientObservability

    /// Creates a client configuration that uses the runtime public endpoints file.
    public init(
        credentialBase64: String,
        authRefreshBefore: Duration,
        requestTimeout: Duration,
        persistRootURL: URL,
        retryPolicy: RetryPolicySet,
        execution: UploadExecutionPolicy,
        endpointsURL: URL,
        observability: DataGatewayClientObservability = .disabled
    ) throws {
        let endpoints = try ArchebasePublicEndpoints.load(endpointsURL: endpointsURL)
        self.authEndpoint = endpoints.auth
        self.gatewayEndpoint = endpoints.gateway
        self.credentialBase64 = credentialBase64
        self.authRefreshBefore = authRefreshBefore
        self.requestTimeout = requestTimeout
        self.persistRootURL = persistRootURL
        self.retryPolicy = retryPolicy
        self.execution = execution
        self.authTLS = endpoints.authTLS
        self.gatewayTLS = endpoints.gatewayTLS
        self.observability = observability
    }

    package init(
        authEndpoint: URL,
        gatewayEndpoint: URL,
        credentialBase64: String,
        authRefreshBefore: Duration,
        requestTimeout: Duration,
        persistRootURL: URL,
        retryPolicy: RetryPolicySet,
        execution: UploadExecutionPolicy,
        tls: TLSMode,
        observability: DataGatewayClientObservability = .disabled
    ) {
        self.authEndpoint = authEndpoint
        self.gatewayEndpoint = gatewayEndpoint
        self.credentialBase64 = credentialBase64
        self.authRefreshBefore = authRefreshBefore
        self.requestTimeout = requestTimeout
        self.persistRootURL = persistRootURL
        self.retryPolicy = retryPolicy
        self.execution = execution
        self.authTLS = tls
        self.gatewayTLS = tls
        self.observability = observability
    }

    /// Recommended defaults for runtime public endpoints.
    public static func recommended(
        credentialBase64: String,
        persistRootURL: URL,
        endpointsURL: URL,
        observability: DataGatewayClientObservability = .disabled
    ) throws -> DataGatewayClientConfig {
        try DataGatewayClientConfig(
            credentialBase64: credentialBase64,
            authRefreshBefore: .seconds(60),
            requestTimeout: .seconds(10),
            persistRootURL: persistRootURL,
            retryPolicy: .recommended,
            execution: .recommended,
            endpointsURL: endpointsURL,
            observability: observability
        )
    }

    package static func testRecommended(
        authEndpoint: URL,
        gatewayEndpoint: URL,
        credentialBase64: String,
        persistRootURL: URL,
        tls: TLSMode = .plaintext,
        observability: DataGatewayClientObservability = .disabled
    ) -> DataGatewayClientConfig {
        DataGatewayClientConfig(
            authEndpoint: authEndpoint,
            gatewayEndpoint: gatewayEndpoint,
            credentialBase64: credentialBase64,
            authRefreshBefore: .seconds(60),
            requestTimeout: .seconds(10),
            persistRootURL: persistRootURL,
            retryPolicy: .recommended,
            execution: .recommended,
            tls: tls,
            observability: observability
        )
    }

    /// Validates endpoint, TLS, and local persistence constraints before client construction.
    public func validate() throws {
        try Self.validate(endpoint: self.authEndpoint, tls: self.authTLS, fieldName: "authEndpoint")
        try Self.validate(endpoint: self.gatewayEndpoint, tls: self.gatewayTLS, fieldName: "gatewayEndpoint")

        if self.credentialBase64.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
            throw DataGatewayClientError.invalidConfiguration("credential_base64 must not be empty")
        }

        if self.execution.maxRestartCount < 0 {
            throw DataGatewayClientError.invalidConfiguration("execution.maxRestartCount must be >= 0")
        }
    }

    package static func validate(endpoint: URL, tls: TLSMode, fieldName: String) throws {
        guard let scheme = endpoint.scheme?.lowercased() else {
            throw DataGatewayClientError.invalidConfiguration("\(fieldName) must include a URL scheme")
        }

        switch (tls, scheme) {
        case (.plaintext, "http"), (.tls, "https"):
            break
        case (.plaintext, _):
            throw DataGatewayClientError.invalidConfiguration("\(fieldName) must use http when tls is plaintext")
        case (.tls, _):
            throw DataGatewayClientError.invalidConfiguration("\(fieldName) must use https when tls is tls")
        }
    }
}

package enum RawTagsMerger {
    package static let sourceFileNameRawTagKey = "a206e337ecdf70a93bb611cf6a30c346.raw_file"

    package static func merge(
        configTags: [String: String],
        uploadRawTags: [String: String],
        sourceFileURL: URL? = nil
    ) throws -> [String: String] {
        var merged = configTags
        if let sourceFileURL {
            for (key, value) in Self.sourceFileNameTags(sourceFileURL: sourceFileURL) {
                try Self.insert(key: key, value: value, into: &merged)
            }
        }
        for (key, value) in uploadRawTags {
            try Self.insert(key: key, value: value, into: &merged)
        }
        try ArchebaseConfig.validateTags(merged, fieldName: "raw_tags")
        return merged
    }

    private static func sourceFileNameTags(sourceFileURL: URL) -> [String: String] {
        let fileName = sourceFileURL.lastPathComponent
        guard !fileName.isEmpty else {
            return [:]
        }
        return [
            Self.sourceFileNameRawTagKey: fileName,
        ]
    }

    private static func insert(key: String, value: String, into tags: inout [String: String]) throws {
        if let existing = tags[key] {
            if existing != value {
                throw DataGatewayClientError.rawTagConflict(key: key)
            }
            return
        }
        tags[key] = value
    }
}

/// Public entry point for first-time device initialization and explicit reinitialization.
public actor ArchebaseDeviceInitializer {
    private let configStore: ArchebaseConfigStore
    private let initTransport: any DeviceInitTransport
    private let runtimeResources: DeviceInitRuntimeResources?
    private let sdkVersion: String
    private let platform: String

    /// Creates an initializer that targets the runtime public initialization endpoint.
    public init(config: DeviceInitClientConfig) throws {
        let endpoints = try ArchebasePublicEndpoints.load(endpointsURL: config.endpointsURL)
        var resolvedConfig = config
        resolvedConfig.tls = config.tls ?? endpoints.deviceInitTLS
        try self.init(
            config: resolvedConfig,
            initEndpoint: endpoints.deviceInit,
            sdkVersion: DataGatewayClientModule.version,
            platform: "ios"
        )
    }

    package init(
        config: DeviceInitClientConfig,
        initEndpoint: URL,
        sdkVersion: String = DataGatewayClientModule.version,
        platform: String = "ios"
    ) throws {
        let tls = config.tls ?? Self.tlsMode(for: initEndpoint)
        let security: ControlPlaneTransportSecurity = switch tls {
        case .plaintext: .plaintext
        case .tls: .tls
        }
        try DataGatewayClientConfig.validate(endpoint: initEndpoint, tls: tls, fieldName: "initEndpoint")

        let factory = ControlPlaneClientFactory(
            configuration: ControlPlaneTransportConfiguration(
                endpoint: initEndpoint,
                security: security,
                requestTimeout: config.requestTimeout
            )
        )
        let managedTransport = try factory.makeDeviceInitTransport()
        try self.init(
            configStore: ArchebaseConfigStore(configURL: config.configURL),
            initTransport: managedTransport.serviceClient,
            runtimeResources: DeviceInitRuntimeResources(initTransport: managedTransport),
            sdkVersion: sdkVersion,
            platform: platform
        )
    }

    private static func tlsMode(for endpoint: URL) -> TLSMode {
        endpoint.scheme?.lowercased() == "https" ? .tls : .plaintext
    }

    package init(
        configStore: ArchebaseConfigStore,
        initTransport: any DeviceInitTransport,
        runtimeResources: DeviceInitRuntimeResources? = nil,
        sdkVersion: String = DataGatewayClientModule.version,
        platform: String = "ios"
    ) throws {
        self.configStore = configStore
        self.initTransport = initTransport
        self.runtimeResources = runtimeResources
        self.sdkVersion = sdkVersion
        self.platform = platform
    }

    /// Initializes a device when no local configuration exists.
    public func initDevice(deviceID: String) async throws -> ArchebaseConfig {
        if await self.configStore.exists() {
            throw DataGatewayClientError.alreadyInitialized(configURL: await self.configStore.resolvedConfigURL())
        }
        let config = try await self.remoteConfig(deviceID: deviceID, mode: .initDevice)
        try await self.configStore.initialize(config)
        return config
    }

    /// Reinitializes a device by rotating the remote credential and writing the local configuration.
    ///
    /// This can recover a missing local config after a prior remote init succeeded but local persistence did not complete.
    public func reinitDevice(deviceID: String) async throws -> ArchebaseConfig {
        let config = try await self.remoteConfig(deviceID: deviceID, mode: .reinitDevice)
        try await self.configStore.replaceOrInitialize(config)
        return config
    }

    private func remoteConfig(deviceID: String, mode: DeviceInitRemoteMode) async throws -> ArchebaseConfig {
        try await DeviceInitConfigFetcher.fetch(
            mode: mode,
            deviceID: deviceID,
            transport: self.initTransport,
            sdkVersion: self.sdkVersion,
            platform: self.platform
        )
    }
}

package final class DeviceInitRuntimeResources: @unchecked Sendable {
    private let initTransport: ManagedControlPlaneServiceClient<any DeviceInitTransport>

    package init(initTransport: ManagedControlPlaneServiceClient<any DeviceInitTransport>) {
        self.initTransport = initTransport
    }
}

package protocol SecurityScopedFileAccessing: Sendable {
    func access<Result: Sendable>(_ fileURL: URL, operation: @Sendable () throws -> Result) throws -> Result
    func access<Result: Sendable>(_ fileURL: URL, operation: @Sendable () async throws -> Result) async throws -> Result
    func access<Result: Sendable>(
        _ fileURL: URL,
        bookmarkData: Data?,
        operation: @Sendable (_ accessibleURL: URL) throws -> Result
    ) throws -> Result
    func access<Result: Sendable>(
        _ fileURL: URL,
        bookmarkData: Data?,
        operation: @Sendable (_ accessibleURL: URL) async throws -> Result
    ) async throws -> Result
    func bookmarkData(for fileURL: URL) throws -> Data
}

package struct SecurityScopedFileAccessor: SecurityScopedFileAccessing {
    package init() {}

    package func access<Result: Sendable>(_ fileURL: URL, operation: @Sendable () throws -> Result) throws -> Result {
        try self.access(fileURL, bookmarkData: nil) { _ in
            try operation()
        }
    }

    package func access<Result: Sendable>(_ fileURL: URL, operation: @Sendable () async throws -> Result) async throws -> Result {
        try await self.access(fileURL, bookmarkData: nil) { _ in
            try await operation()
        }
    }

    package func access<Result: Sendable>(
        _ fileURL: URL,
        bookmarkData: Data?,
        operation: @Sendable (_ accessibleURL: URL) throws -> Result
    ) throws -> Result {
        let scopedURL = self.resolveSecurityScopedURL(fileURL: fileURL, bookmarkData: bookmarkData)
        let started = scopedURL.startAccessingSecurityScopedResource()
        defer {
            if started {
                scopedURL.stopAccessingSecurityScopedResource()
            }
        }
        return try operation(scopedURL)
    }

    package func access<Result: Sendable>(
        _ fileURL: URL,
        bookmarkData: Data?,
        operation: @Sendable (_ accessibleURL: URL) async throws -> Result
    ) async throws -> Result {
        let scopedURL = self.resolveSecurityScopedURL(fileURL: fileURL, bookmarkData: bookmarkData)
        let started = scopedURL.startAccessingSecurityScopedResource()
        defer {
            if started {
                scopedURL.stopAccessingSecurityScopedResource()
            }
        }
        return try await operation(scopedURL)
    }

    package func bookmarkData(for fileURL: URL) throws -> Data {
        try fileURL.bookmarkData(options: [.minimalBookmark], includingResourceValuesForKeys: nil, relativeTo: nil)
    }

    private func resolveSecurityScopedURL(fileURL: URL, bookmarkData: Data?) -> URL {
        guard let bookmarkData else {
            return fileURL
        }

        var bookmarkDataIsStale = false
        guard let resolvedURL = try? URL(
            resolvingBookmarkData: bookmarkData,
            options: [],
            relativeTo: nil,
            bookmarkDataIsStale: &bookmarkDataIsStale
        ) else {
            return fileURL
        }
        return resolvedURL.standardizedFileURL
    }
}

package protocol FileSystemProviding: Sendable {
    func fileExists(at url: URL) -> Bool
    func attributes(at url: URL) throws -> [FileAttributeKey: Any]
    func read(prefixFrom url: URL, maxLength: Int) throws -> Data
    func readRange(from url: URL, offset: UInt64, maxLength: Int) throws -> Data
    func inputStream(from url: URL, offset: UInt64, length: UInt64) throws -> InputStream
    func createDirectory(at url: URL) throws
    func copyItem(at sourceURL: URL, to destinationURL: URL) throws
}

package struct LocalFileSystem: FileSystemProviding {
    package init() {}

    package func fileExists(at url: URL) -> Bool {
        FileManager.default.fileExists(atPath: url.path)
    }

    package func attributes(at url: URL) throws -> [FileAttributeKey: Any] {
        try FileManager.default.attributesOfItem(atPath: url.path)
    }

    package func read(prefixFrom url: URL, maxLength: Int) throws -> Data {
        let handle = try FileHandle(forReadingFrom: url)
        defer { try? handle.close() }
        return try handle.read(upToCount: maxLength) ?? Data()
    }

    package func readRange(from url: URL, offset: UInt64, maxLength: Int) throws -> Data {
        let handle = try FileHandle(forReadingFrom: url)
        defer { try? handle.close() }
        try handle.seek(toOffset: offset)
        return try handle.read(upToCount: maxLength) ?? Data()
    }

    package func inputStream(from url: URL, offset: UInt64, length: UInt64) throws -> InputStream {
        FileRangeInputStream(fileURL: url, offset: offset, length: length)
    }

    package func createDirectory(at url: URL) throws {
        try FileManager.default.createDirectory(at: url, withIntermediateDirectories: true)
    }

    package func copyItem(at sourceURL: URL, to destinationURL: URL) throws {
        if FileManager.default.fileExists(atPath: destinationURL.path) {
            try FileManager.default.removeItem(at: destinationURL)
        }
        try FileManager.default.copyItem(at: sourceURL, to: destinationURL)
    }
}

private final class FileRangeInputStream: InputStream, @unchecked Sendable {
    private let fileURL: URL
    private let offset: UInt64
    private let length: UInt64
    private var handle: FileHandle?
    private var remaining: UInt64
    private var statusValue: Stream.Status = .notOpen
    private var errorValue: (any Error)?
    private var delegateValue: StreamDelegate?

    init(fileURL: URL, offset: UInt64, length: UInt64) {
        self.fileURL = fileURL
        self.offset = offset
        self.length = length
        self.remaining = length
        super.init(data: Data())
    }

    override var streamStatus: Stream.Status {
        self.statusValue
    }

    override var streamError: (any Error)? {
        self.errorValue
    }

    override var delegate: StreamDelegate? {
        get {
            self.delegateValue
        }
        set {
            self.delegateValue = newValue
        }
    }

    override var hasBytesAvailable: Bool {
        self.statusValue == .open && self.remaining > 0
    }

    override func open() {
        do {
            let handle = try FileHandle(forReadingFrom: self.fileURL)
            try handle.seek(toOffset: self.offset)
            self.handle = handle
            self.remaining = self.length
            self.errorValue = nil
            self.statusValue = self.remaining == 0 ? .atEnd : .open
        } catch {
            self.errorValue = error
            self.statusValue = .error
        }
    }

    override func close() {
        try? self.handle?.close()
        self.handle = nil
        if self.statusValue != .error {
            self.statusValue = .closed
        }
    }

    override func read(_ buffer: UnsafeMutablePointer<UInt8>, maxLength len: Int) -> Int {
        guard self.statusValue == .open else {
            return self.statusValue == .atEnd ? 0 : -1
        }
        guard self.remaining > 0 else {
            self.statusValue = .atEnd
            return 0
        }
        guard let handle else {
            self.errorValue = CocoaError(.fileReadUnknown)
            self.statusValue = .error
            return -1
        }

        do {
            let readLength = min(len, Int(min(self.remaining, UInt64(Int.max))))
            guard readLength > 0 else {
                self.statusValue = .atEnd
                return 0
            }
            let data = try handle.read(upToCount: readLength) ?? Data()
            guard !data.isEmpty else {
                self.statusValue = .atEnd
                return 0
            }
            data.copyBytes(to: buffer, count: data.count)
            self.remaining -= UInt64(data.count)
            if self.remaining == 0 {
                self.statusValue = .atEnd
            }
            return data.count
        } catch {
            self.errorValue = error
            self.statusValue = .error
            return -1
        }
    }

    override func schedule(in aRunLoop: RunLoop, forMode mode: RunLoop.Mode) {
        _ = (aRunLoop, mode)
    }

    override func remove(from aRunLoop: RunLoop, forMode mode: RunLoop.Mode) {
        _ = (aRunLoop, mode)
    }

    override func getBuffer(
        _ buffer: UnsafeMutablePointer<UnsafeMutablePointer<UInt8>?>,
        length len: UnsafeMutablePointer<Int>
    ) -> Bool {
        buffer.pointee = nil
        len.pointee = 0
        return false
    }
}

package struct PreparedLocalFile: Sendable, Equatable {
    package let sourceFileURL: URL
    package let managedFileURL: URL
    package let bookmarkData: Data?
    package let fileSize: UInt64
    package let fingerprint: LocalFileFingerprint

    package init(
        sourceFileURL: URL,
        managedFileURL: URL,
        bookmarkData: Data?,
        fileSize: UInt64,
        fingerprint: LocalFileFingerprint
    ) {
        self.sourceFileURL = sourceFileURL
        self.managedFileURL = managedFileURL
        self.bookmarkData = bookmarkData
        self.fileSize = fileSize
        self.fingerprint = fingerprint
    }
}

package struct LocalFileMD5Checksum: Sendable, Equatable {
    package let hex: String
    package let base64: String

    package init(hex: String, base64: String) {
        self.hex = hex
        self.base64 = base64
    }
}

package struct FileStagingCoordinator: Sendable {
    private static let modifiedAtComparisonTolerance: TimeInterval = 1
    private static let md5ChunkSize = 1024 * 1024

    private let stagingRoot: URL
    private let fileSystem: any FileSystemProviding
    private let securityScopedAccessor: any SecurityScopedFileAccessing

    package init(
        stagingRoot: URL,
        fileSystem: any FileSystemProviding = LocalFileSystem(),
        securityScopedAccessor: any SecurityScopedFileAccessing = SecurityScopedFileAccessor()
    ) {
        self.stagingRoot = stagingRoot
        self.fileSystem = fileSystem
        self.securityScopedAccessor = securityScopedAccessor
    }

    package func prepare(
        request: UploadRequest,
        persistence: LocalPersistencePolicy
    ) throws -> PreparedLocalFile {
        let sourceURL = request.fileURL.standardizedFileURL
        let bookmarkData = try? self.securityScopedAccessor.bookmarkData(for: sourceURL)

        return try self.securityScopedAccessor.access(sourceURL) {
            guard self.fileSystem.fileExists(at: sourceURL) else {
                throw DataGatewayClientError.invalidLocalFile("local file does not exist: \(sourceURL.path)")
            }

            let attributes = try self.fileSystem.attributes(at: sourceURL)
            let fileSize = try Self.resolveFileSize(from: attributes)
            if fileSize == 0 {
                throw DataGatewayClientError.zeroByteFile
            }

            let modifiedAt = attributes[.modificationDate] as? Date
            let firstChunk = try self.fileSystem.read(prefixFrom: sourceURL, maxLength: 1024 * 1024)
            let fingerprint = LocalFileFingerprint(
                size: fileSize,
                modifiedAt: modifiedAt,
                firstChunkMD5Hex: Self.md5Hex(firstChunk)
            )

            _ = persistence.copyExternalFileIntoManagedStaging
            let managedFileURL = sourceURL

            return PreparedLocalFile(
                sourceFileURL: sourceURL,
                managedFileURL: managedFileURL,
                bookmarkData: bookmarkData,
                fileSize: fileSize,
                fingerprint: fingerprint
            )
        }
    }

    package func validatePreparedFile(
        managedFileURL: URL,
        bookmarkData: Data? = nil,
        expectedFingerprint: LocalFileFingerprint
    ) throws {
        try self.securityScopedAccessor.access(managedFileURL, bookmarkData: bookmarkData) { accessibleURL in
            guard self.fileSystem.fileExists(at: accessibleURL) else {
                throw DataGatewayClientError.resumeNotPossible("source file missing: \(managedFileURL.path)")
            }

            let attributes = try self.fileSystem.attributes(at: accessibleURL)
            let actualFingerprint = LocalFileFingerprint(
                size: try Self.resolveFileSize(from: attributes),
                modifiedAt: attributes[.modificationDate] as? Date,
                firstChunkMD5Hex: Self.md5Hex(try self.fileSystem.read(prefixFrom: accessibleURL, maxLength: 1024 * 1024))
            )

            guard Self.fingerprintsMatch(actual: actualFingerprint, expected: expectedFingerprint) else {
                throw DataGatewayClientError.resumeNotPossible("local file fingerprint changed")
            }
        }
    }

    package func accessPreparedFile<Result: Sendable>(
        fileURL: URL,
        bookmarkData: Data?,
        operation: @Sendable (_ accessibleURL: URL) async throws -> Result
    ) async throws -> Result {
        try await self.securityScopedAccessor.access(fileURL, bookmarkData: bookmarkData, operation: operation)
    }

    package func inputStream(from fileURL: URL, offset: UInt64, length: UInt64) throws -> InputStream {
        try self.fileSystem.inputStream(from: fileURL, offset: offset, length: length)
    }

    package func md5Checksum(from fileURL: URL, offset: UInt64, length: UInt64) throws -> LocalFileMD5Checksum {
        var hasher = Insecure.MD5()
        var cursor = offset
        var remaining = length

        while remaining > 0 {
            let chunkLength = min(Self.md5ChunkSize, Int(min(remaining, UInt64(Self.md5ChunkSize))))
            let chunk = try self.fileSystem.readRange(from: fileURL, offset: cursor, maxLength: chunkLength)
            guard !chunk.isEmpty else {
                throw DataGatewayClientError.invalidLocalFile("unexpected EOF while reading local file range")
            }
            hasher.update(data: chunk)
            cursor += UInt64(chunk.count)
            remaining -= UInt64(chunk.count)
        }

        let digest = hasher.finalize()
        return LocalFileMD5Checksum(
            hex: Self.md5Hex(digest),
            base64: Data(digest).base64EncodedString()
        )
    }

    private static func resolveFileSize(from attributes: [FileAttributeKey: Any]) throws -> UInt64 {
        if let size = attributes[.size] as? NSNumber {
            return size.uint64Value
        }
        throw DataGatewayClientError.invalidLocalFile("local file size is unavailable")
    }

    private static func md5Hex(_ data: Data) -> String {
        let digest = Insecure.MD5.hash(data: data)
        return Self.md5Hex(digest)
    }

    private static func md5Hex(_ digest: Insecure.MD5.Digest) -> String {
        return digest.map { String(format: "%02X", $0) }.joined()
    }

    private static func fingerprintsMatch(actual: LocalFileFingerprint, expected: LocalFileFingerprint) -> Bool {
        guard actual.size == expected.size, actual.firstChunkMD5Hex == expected.firstChunkMD5Hex else {
            return false
        }

        switch (actual.modifiedAt, expected.modifiedAt) {
        case (.none, .none):
            return true
        case (.some(let actualModifiedAt), .some(let expectedModifiedAt)):
            return abs(actualModifiedAt.timeIntervalSince(expectedModifiedAt)) <= Self.modifiedAtComparisonTolerance
        default:
            return false
        }
    }
}

/// Final upload result returned to callers.
public struct UploadResult: Sendable, Equatable {
    public var logicalUploadID: String
    public var uploadID: String
    public var bucket: String
    public var objectKey: String
    public var fileSize: UInt64
    public var ossObjectETag: String

    public init(
        logicalUploadID: String,
        uploadID: String,
        bucket: String,
        objectKey: String,
        fileSize: UInt64,
        ossObjectETag: String
    ) {
        self.logicalUploadID = logicalUploadID
        self.uploadID = uploadID
        self.bucket = bucket
        self.objectKey = objectKey
        self.fileSize = fileSize
        self.ossObjectETag = ossObjectETag
    }
}

/// Options for listing verified logical objects visible to the user bearer token.
public struct ListObjectsOptions: Sendable, Equatable {
    public var pageSize: Int32
    public var pageToken: String?
    public var filter: String?

    public init(
        pageSize: Int32 = 0,
        pageToken: String? = nil,
        filter: String? = nil
    ) {
        self.pageSize = pageSize
        self.pageToken = pageToken
        self.filter = filter
    }
}

/// User-visible lifecycle state for one logical data object.
public enum DataObjectStatus: Sendable, Equatable {
    case unspecified
    case created
    case uploaded
    case verified
    case bad
    case aborted
    case invalid
    case unrecognized(Int)

    package init(proto: Archebase_DataGateway_V1_DataObjectStatus) {
        switch proto {
        case .unspecified:
            self = .unspecified
        case .created:
            self = .created
        case .uploaded:
            self = .uploaded
        case .verified:
            self = .verified
        case .bad:
            self = .bad
        case .aborted:
            self = .aborted
        case .invalid:
            self = .invalid
        case .UNRECOGNIZED(let value):
            self = .unrecognized(value)
        }
    }
}

/// One logical data object visible to the authenticated user.
public struct DataObject: Sendable, Equatable {
    public var objectID: String
    public var fileID: String
    public var status: DataObjectStatus
    public var sizeBytes: Int64
    public var createdAtUnix: Int64
    public var uploadedAtUnix: Int64
    public var verifiedAtUnix: Int64
    public var etag: String

    public init(
        objectID: String,
        fileID: String,
        status: DataObjectStatus,
        sizeBytes: Int64,
        createdAtUnix: Int64,
        uploadedAtUnix: Int64,
        verifiedAtUnix: Int64,
        etag: String
    ) {
        self.objectID = objectID
        self.fileID = fileID
        self.status = status
        self.sizeBytes = sizeBytes
        self.createdAtUnix = createdAtUnix
        self.uploadedAtUnix = uploadedAtUnix
        self.verifiedAtUnix = verifiedAtUnix
        self.etag = etag
    }

    package init(proto: Archebase_DataGateway_V1_DataObject) {
        self.init(
            objectID: proto.objectID,
            fileID: proto.fileID,
            status: DataObjectStatus(proto: proto.status),
            sizeBytes: proto.sizeBytes,
            createdAtUnix: proto.createdAtUnix,
            uploadedAtUnix: proto.uploadedAtUnix,
            verifiedAtUnix: proto.verifiedAtUnix,
            etag: proto.etag
        )
    }
}

/// One page of logical data objects and the opaque next-page token.
public struct ListObjectsPage: Sendable, Equatable {
    public var objects: [DataObject]
    public var nextPageToken: String

    public init(objects: [DataObject], nextPageToken: String) {
        self.objects = objects
        self.nextPageToken = nextPageToken
    }

    package init(proto: Archebase_DataGateway_V1_ListObjectsResponse) {
        self.init(
            objects: proto.objects.map(DataObject.init(proto:)),
            nextPageToken: proto.nextPageToken
        )
    }
}

/// Upload status events emitted by the coordinator or stream API.
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

package enum ResumeDecision: Sendable, Equatable {
    case continueExisting(uploadID: String)
    case completeOnly(uploadID: String, expectedObjectETag: String?)
    case restartUpload(previousUploadID: String)
    case permanentFailure(reason: String)
}

package enum ReconcileRemotePartsDecision: Sendable, Equatable {
    case continueUpload([PersistedUploadedPart])
    case restartUpload
}

private struct DataPlaneUploadCompletion: Sendable, Equatable {
    let completedPartCount: Int32
    let ossObjectETag: String
}

package protocol UploadCoordinatorClock: Sendable {
    func now() async -> Date
}

package struct SystemUploadCoordinatorClock: UploadCoordinatorClock {
    package init() {}

    package func now() async -> Date {
        Date()
    }
}

package protocol UploadCoordinatorGatewayClient: Sendable {
    func createLogicalUpload(
        clientHints: [String: String],
        restartFromUploadID: String?
    ) async throws -> Archebase_DataGateway_V1_CreateLogicalUploadResponse

    func getUploadRecovery(
        logicalUploadID: String
    ) async throws -> Archebase_DataGateway_V1_GetUploadRecoveryResponse

    func reissueUploadCredentials(
        uploadID: String
    ) async throws -> Archebase_DataGateway_V1_ReissueUploadCredentialsResponse

    func abortUpload(
        logicalUploadID: String,
        reason: String
    ) async throws -> Archebase_DataGateway_V1_AbortUploadResponse

    func completeUpload(
        uploadID: String,
        fileSize: Int64,
        rawTags: [String: String],
        completedPartCount: Int32,
        ossObjectEtag: String,
        partSizeBytes: Int64
    ) async throws -> Archebase_DataGateway_V1_CompleteUploadResponse
}

package protocol UploadCoordinatorMultipartSessionProtocol: Sendable {
    func ensureFreshCredentialsIfNeeded() async throws -> Bool
    func lastKnownCredentialExpiration() async -> Date?
    func initiateMultipartUpload() async throws -> String
    func uploadPart(
        multipartUploadID: String,
        partNumber: Int,
        body: OssUploadBody
    ) async throws -> UploadedPartDescriptor
    func putObject(body: OssUploadBody) async throws -> UploadedPartDescriptor
    func listParts(multipartUploadID: String) async throws -> [UploadedPartDescriptor]
    func headObjectETag() async throws -> String
    func completeMultipartUpload(
        multipartUploadID: String,
        parts: [UploadedPartDescriptor]
    ) async throws -> String
}

extension OssUploadSession: UploadCoordinatorMultipartSessionProtocol {}

/// Dependency bundle injected into `UploadCoordinator` for control plane, OSS, and persistence seams.
public struct UploadCoordinatorDependencies: Sendable {
    package let gatewayClient: any UploadCoordinatorGatewayClient
    package let stateStore: UploadStateStore
    package let fileCoordinator: FileStagingCoordinator
    package let ossClientFactory: @Sendable (OssUploadContext) throws -> any UploadCoordinatorMultipartSessionProtocol
    package let clock: any UploadCoordinatorClock
    package let observability: DataGatewayClientObservability

    package init(
        gatewayClient: any UploadCoordinatorGatewayClient,
        stateStore: UploadStateStore,
        fileCoordinator: FileStagingCoordinator,
        ossClientFactory: @escaping @Sendable (OssUploadContext) throws -> any UploadCoordinatorMultipartSessionProtocol,
        clock: any UploadCoordinatorClock = SystemUploadCoordinatorClock(),
        observability: DataGatewayClientObservability = .disabled
    ) {
        self.gatewayClient = gatewayClient
        self.stateStore = stateStore
        self.fileCoordinator = fileCoordinator
        self.ossClientFactory = ossClientFactory
        self.clock = clock
        self.observability = observability
    }
}

/// Serial upload orchestrator that drives control plane, OSS, and persistence.
public actor UploadCoordinator {
    private let executionPolicy: UploadExecutionPolicy
    private let dependencies: UploadCoordinatorDependencies

    public init(
        executionPolicy: UploadExecutionPolicy,
        dependencies: UploadCoordinatorDependencies
    ) {
        self.executionPolicy = executionPolicy
        self.dependencies = dependencies
    }

    public func upload(
        _ request: UploadRequest,
        onEvent: (@Sendable (UploadEvent) async -> Void)? = nil
    ) async throws -> UploadResult {
        await self.emitLog(operation: "upload", phase: "preparing", message: "starting upload")
        await onEvent?(.preparing)
        let preparedFile = try self.dependencies.fileCoordinator.prepare(
            request: request,
            persistence: self.executionPolicy.persistence
        )

        await onEvent?(.authenticating)
        await onEvent?(.creatingLogicalUpload)
        let createResponse = try await self.dependencies.gatewayClient.createLogicalUpload(
            clientHints: request.clientHints,
            restartFromUploadID: nil
        )
        guard createResponse.hasCredentials else {
            throw DataGatewayClientError.gatewayFailed(
                statusCode: RPCError.Code.internalError.rawValue,
                detailCode: "DATA_GATEWAY_STS_UNAVAILABLE",
                message: "CreateLogicalUpload response missing credentials"
            )
        }

        let createdAt = await self.dependencies.clock.now()
        let persistedState = try self.makeInitialState(
            request: request,
            preparedFile: preparedFile,
            response: createResponse,
            createdAt: createdAt
        )
        try await self.dependencies.stateStore.saveActive(persistedState)

        let uploadContext = try OssUploadSession.makeUploadContext(
            uploadID: createResponse.uploadID,
            credentials: createResponse.credentials
        )
        let ossSession = try self.dependencies.ossClientFactory(uploadContext)
        await self.emitLog(operation: "upload", uploadID: createResponse.uploadID, logicalUploadID: createResponse.logicalUploadID, phase: "session_created", message: "created logical upload")

        var uploadState = persistedState
        let uploadCompletion: DataPlaneUploadCompletion
        if Self.shouldUseSingleObjectUpload(fileSize: uploadState.fileSize, partSizeBytes: uploadState.partSizeBytes) {
            uploadCompletion = try await self.uploadSingleObject(
                state: &uploadState,
                session: ossSession,
                onEvent: onEvent
            )
        } else {
            uploadCompletion = try await self.uploadMultipart(
                state: &uploadState,
                session: ossSession,
                existingMultipartUploadID: nil,
                onEvent: onEvent
            )
        }

        let result = try await self.completeBusinessUpload(
            state: &uploadState,
            uploadCompletion: uploadCompletion,
            onEvent: onEvent
        )
        await onEvent?(.completed(result))
        await self.emitLog(operation: "upload", uploadID: result.uploadID, logicalUploadID: result.logicalUploadID, phase: "completed", message: "upload completed")
        return result
    }

    package func resumeUpload(
        logicalUploadID: String,
        onEvent: (@Sendable (UploadEvent) async -> Void)? = nil
    ) async throws -> UploadResult {
        guard let state = try await self.dependencies.stateStore.loadSnapshot(logicalUploadID: logicalUploadID) else {
            throw DataGatewayClientError.resumeNotPossible("local snapshot not found: \(logicalUploadID)")
        }

        await onEvent?(.resuming(logicalUploadID: logicalUploadID))
        await self.emitLog(operation: "resume", logicalUploadID: logicalUploadID, phase: "resolving", message: "resuming persisted upload")
        try self.dependencies.fileCoordinator.validatePreparedFile(
            managedFileURL: state.managedFileURL,
            bookmarkData: state.fileURLBookmarkData,
            expectedFingerprint: state.fileFingerprint
        )

        let recovery = try await self.dependencies.gatewayClient.getUploadRecovery(logicalUploadID: logicalUploadID)
        switch Self.decideResumeAction(state: state, recovery: recovery) {
        case .continueExisting:
            return try await self.continueExistingUpload(state: state, recovery: recovery, onEvent: onEvent)
        case .completeOnly(_, let expectedObjectETag):
            return try await self.completeOnlyUpload(
                state: state,
                recovery: recovery,
                expectedObjectETag: expectedObjectETag,
                onEvent: onEvent
            )
        case .restartUpload:
            return try await self.restartUpload(state: state, onEvent: onEvent)
        case .permanentFailure(let reason):
            throw DataGatewayClientError.resumeNotPossible(reason)
        }
    }

    package func listPendingUploads() async throws -> [PendingUploadInfo] {
        try await self.dependencies.stateStore.listPendingUploads()
    }

    package func abortUpload(logicalUploadID: String) async throws {
        let state = try await self.dependencies.stateStore.loadSnapshot(logicalUploadID: logicalUploadID)

        do {
            _ = try await self.dependencies.gatewayClient.abortUpload(
                logicalUploadID: logicalUploadID,
                reason: "aborted by client"
            )
        } catch let error as DataGatewayClientError {
            guard case .gatewayFailed(_, let detailCode, _) = error, detailCode == "DATA_GATEWAY_UPLOAD_NOT_FOUND" else {
                throw error
            }
        } catch {
            throw error
        }

        try await self.dependencies.stateStore.deleteLocalSnapshot(logicalUploadID: logicalUploadID)
        if state != nil, self.executionPolicy.cleanupOnTerminalFailure {
            return
        }
    }

    package func deleteLocalSnapshot(logicalUploadID: String) async throws {
        try await self.dependencies.stateStore.deleteLocalSnapshot(logicalUploadID: logicalUploadID)
    }

    private func makeInitialState(
        request: UploadRequest,
        preparedFile: PreparedLocalFile,
        response: Archebase_DataGateway_V1_CreateLogicalUploadResponse,
        createdAt: Date
    ) throws -> PersistedUploadState {
        PersistedUploadState(
            version: 1,
            logicalUploadID: response.logicalUploadID,
            uploadID: response.uploadID,
            restartCount: 0,
            multipartUploadID: nil,
            bucket: response.credentials.bucket,
            endpoint: response.credentials.endpoint,
            objectKey: response.credentials.objectKey,
            fileURLBookmarkData: preparedFile.bookmarkData,
            managedFileURL: preparedFile.managedFileURL,
            fileSize: preparedFile.fileSize,
            fileFingerprint: preparedFile.fingerprint,
            partSizeBytes: UInt64(response.credentials.partSizeBytes),
            uploadedParts: [],
            clientHints: request.clientHints,
            rawTags: request.rawTags,
            phase: .sessionCreated,
            lastKnownSTSExpireAt: Date(timeIntervalSince1970: TimeInterval(response.credentials.stsExpireAtUnix)),
            createdAt: createdAt,
            updatedAt: createdAt
        )
    }

    private func continueExistingUpload(
        state: PersistedUploadState,
        recovery: Archebase_DataGateway_V1_GetUploadRecoveryResponse,
        onEvent: (@Sendable (UploadEvent) async -> Void)?
    ) async throws -> UploadResult {
        let uploadID = recovery.currentUploadID.nilIfBlank ?? state.uploadID
        await onEvent?(.refreshingCredentials(uploadID: uploadID))

        let refreshedCredentials = try await self.dependencies.gatewayClient.reissueUploadCredentials(uploadID: uploadID)
        guard refreshedCredentials.hasCredentials else {
            throw DataGatewayClientError.resumeNotPossible("reissue upload credentials response missing credentials")
        }
        let uploadContext = try OssUploadSession.makeUploadContext(
            uploadID: refreshedCredentials.uploadID,
            credentials: refreshedCredentials.credentials,
            sessionExpireAtUnix: recovery.sessionExpireAtUnix,
            credentialRefreshCount: recovery.credentialRefreshCount
        )
        let ossSession = try self.dependencies.ossClientFactory(uploadContext)

        var resumedState = state
        resumedState.uploadID = refreshedCredentials.uploadID.nilIfBlank ?? uploadID
        resumedState.bucket = refreshedCredentials.credentials.bucket
        resumedState.endpoint = refreshedCredentials.credentials.endpoint
        resumedState.objectKey = refreshedCredentials.credentials.objectKey
        resumedState.partSizeBytes = UInt64(refreshedCredentials.credentials.partSizeBytes)
        resumedState.lastKnownSTSExpireAt = Self.makeDate(fromUnix: refreshedCredentials.credentials.stsExpireAtUnix)
        resumedState.updatedAt = await self.dependencies.clock.now()
        try await self.dependencies.stateStore.saveActive(resumedState)

        if Self.isPersistedSingleObjectCompleted(resumedState) {
            return try await self.completePersistedSingleObjectOrRestart(
                state: resumedState,
                session: ossSession,
                onEvent: onEvent
            )
        }

        if Self.shouldUseSingleObjectUpload(fileSize: resumedState.fileSize, partSizeBytes: resumedState.partSizeBytes),
            resumedState.multipartUploadID?.nilIfBlank == nil,
            resumedState.uploadedParts.isEmpty {
            let uploadCompletion = try await self.uploadSingleObject(
                state: &resumedState,
                session: ossSession,
                onEvent: onEvent
            )
            let result = try await self.completeBusinessUpload(
                state: &resumedState,
                uploadCompletion: uploadCompletion,
                onEvent: onEvent
            )
            await onEvent?(.completed(result))
            return result
        }

        if self.executionPolicy.reconcileRemotePartsOnResume,
            let existingMultipartUploadID = resumedState.multipartUploadID?.nilIfBlank,
            resumedState.phase == .multipartInitiated || resumedState.phase == .uploading {
            await onEvent?(.reconcilingRemoteParts(uploadID: resumedState.uploadID))
            switch try await self.reconcileRemoteParts(
                state: resumedState,
                multipartUploadID: existingMultipartUploadID,
                session: ossSession
            ) {
            case .continueUpload(let reconciledParts):
                resumedState.uploadedParts = reconciledParts
                resumedState.updatedAt = await self.dependencies.clock.now()
                try await self.dependencies.stateStore.saveActive(resumedState)
            case .restartUpload:
                throw DataGatewayClientError.uploadRestartExceeded
            }
        }

        let uploadCompletion = try await self.uploadMultipart(
            state: &resumedState,
            session: ossSession,
            existingMultipartUploadID: resumedState.multipartUploadID?.nilIfBlank,
            onEvent: onEvent
        )
        let result = try await self.completeBusinessUpload(
            state: &resumedState,
            uploadCompletion: uploadCompletion,
            onEvent: onEvent
        )
        await onEvent?(.completed(result))
        return result
    }

    private func completeOnlyUpload(
        state: PersistedUploadState,
        recovery: Archebase_DataGateway_V1_GetUploadRecoveryResponse,
        expectedObjectETag: String?,
        onEvent: (@Sendable (UploadEvent) async -> Void)?
    ) async throws -> UploadResult {
        let uploadID = recovery.currentUploadID.nilIfBlank ?? state.uploadID
        await onEvent?(.refreshingCredentials(uploadID: uploadID))

        let refreshedCredentials = try await self.dependencies.gatewayClient.reissueUploadCredentials(uploadID: uploadID)
        guard refreshedCredentials.hasCredentials else {
            throw DataGatewayClientError.resumeNotPossible("reissue upload credentials response missing credentials")
        }
        let uploadContext = try OssUploadSession.makeUploadContext(
            uploadID: refreshedCredentials.uploadID,
            credentials: refreshedCredentials.credentials,
            sessionExpireAtUnix: recovery.sessionExpireAtUnix,
            credentialRefreshCount: recovery.credentialRefreshCount
        )
        let ossSession = try self.dependencies.ossClientFactory(uploadContext)

        var resumedState = state
        resumedState.uploadID = refreshedCredentials.uploadID.nilIfBlank ?? uploadID
        resumedState.bucket = refreshedCredentials.credentials.bucket
        resumedState.endpoint = refreshedCredentials.credentials.endpoint
        resumedState.objectKey = refreshedCredentials.credentials.objectKey
        resumedState.partSizeBytes = UInt64(refreshedCredentials.credentials.partSizeBytes)
        resumedState.lastKnownSTSExpireAt = Self.makeDate(fromUnix: refreshedCredentials.credentials.stsExpireAtUnix)
        resumedState.updatedAt = await self.dependencies.clock.now()
        try await self.dependencies.stateStore.saveActive(resumedState)

        if Self.isPersistedSingleObjectCompleted(resumedState) {
            return try await self.completePersistedSingleObjectOrRestart(
                state: resumedState,
                session: ossSession,
                onEvent: onEvent
            )
        }

        guard let expectedObjectETag = expectedObjectETag?.trimmingCharacters(in: .whitespacesAndNewlines), !expectedObjectETag.isEmpty else {
            throw DataGatewayClientError.uploadRestartExceeded
        }

        let remoteETag: String
        do {
            remoteETag = try await ossSession.headObjectETag()
        } catch let error as DataGatewayClientError {
            if case .ossFailed(let httpStatus, let ossCode, _) = error,
                httpStatus == 404 || ossCode == "NotFound" || ossCode == "NoSuchKey" {
                throw DataGatewayClientError.uploadRestartExceeded
            }
            throw error
        }

        if !Self.etagsMatch(remoteETag, expectedObjectETag) {
            throw DataGatewayClientError.uploadRestartExceeded
        }

        let result = try await self.completeBusinessUpload(
            state: &resumedState,
            uploadCompletion: DataPlaneUploadCompletion(
                completedPartCount: Int32(resumedState.uploadedParts.count),
                ossObjectETag: remoteETag
            ),
            onEvent: onEvent
        )
        await onEvent?(.completed(result))
        return result
    }

    private func restartUpload(
        state: PersistedUploadState,
        onEvent: (@Sendable (UploadEvent) async -> Void)?
    ) async throws -> UploadResult {
        if state.restartCount >= self.executionPolicy.maxRestartCount {
            try await self.dependencies.stateStore.deleteLocalSnapshot(logicalUploadID: state.logicalUploadID)
            throw DataGatewayClientError.uploadRestartExceeded
        }

        await onEvent?(.creatingLogicalUpload)
        let createResponse = try await self.dependencies.gatewayClient.createLogicalUpload(
            clientHints: state.clientHints,
            restartFromUploadID: state.uploadID
        )
        guard createResponse.hasCredentials else {
            throw DataGatewayClientError.gatewayFailed(
                statusCode: RPCError.Code.internalError.rawValue,
                detailCode: "DATA_GATEWAY_STS_UNAVAILABLE",
                message: "CreateLogicalUpload response missing credentials"
            )
        }

        let createdAt = await self.dependencies.clock.now()
        let preparedFile = PreparedLocalFile(
            sourceFileURL: state.managedFileURL,
            managedFileURL: state.managedFileURL,
            bookmarkData: state.fileURLBookmarkData,
            fileSize: state.fileSize,
            fingerprint: state.fileFingerprint
        )
        var restartedState = try self.makeInitialState(
            request: UploadRequest(
                fileURL: state.managedFileURL,
                clientHints: state.clientHints,
                rawTags: state.rawTags,
                displayName: nil
            ),
            preparedFile: preparedFile,
            response: createResponse,
            createdAt: createdAt
        )
        restartedState.restartCount = state.restartCount + 1
        try await self.dependencies.stateStore.saveActive(restartedState)

        let uploadContext = try OssUploadSession.makeUploadContext(
            uploadID: createResponse.uploadID,
            credentials: createResponse.credentials,
            credentialRefreshCount: Int32(restartedState.restartCount)
        )
        let ossSession = try self.dependencies.ossClientFactory(uploadContext)

        let uploadCompletion: DataPlaneUploadCompletion
        if Self.shouldUseSingleObjectUpload(fileSize: restartedState.fileSize, partSizeBytes: restartedState.partSizeBytes) {
            uploadCompletion = try await self.uploadSingleObject(
                state: &restartedState,
                session: ossSession,
                onEvent: onEvent
            )
        } else {
            uploadCompletion = try await self.uploadMultipart(
                state: &restartedState,
                session: ossSession,
                existingMultipartUploadID: nil,
                onEvent: onEvent
            )
        }

        let result = try await self.completeBusinessUpload(
            state: &restartedState,
            uploadCompletion: uploadCompletion,
            onEvent: onEvent
        )
        await onEvent?(.completed(result))
        return result
    }

    private func uploadSingleObject(
        state: inout PersistedUploadState,
        session: any UploadCoordinatorMultipartSessionProtocol,
        onEvent: (@Sendable (UploadEvent) async -> Void)?
    ) async throws -> DataPlaneUploadCompletion {
        try await self.refreshUploadSessionIfNeeded(session: session, state: &state, onEvent: onEvent)

        let bodySize = state.fileSize
        let uploadID = state.uploadID
        let totalBytes = state.fileSize
        let upload = try await self.dependencies.fileCoordinator.accessPreparedFile(
            fileURL: state.managedFileURL,
            bookmarkData: state.fileURLBookmarkData
        ) { accessibleFileURL in
            let checksum = try self.dependencies.fileCoordinator.md5Checksum(
                from: accessibleFileURL,
                offset: 0,
                length: bodySize
            )
            let body = OssUploadBody.file(
                accessibleFileURL,
                sizeBytes: try Self.int64Size(bodySize),
                contentMD5Base64: checksum.base64
            )
            await onEvent?(.uploadingPart(partNumber: 1, sentBytes: bodySize, totalBytes: totalBytes))
            await self.emitMetric("upload_part", dimensions: ["upload_id": uploadID, "part_number": "1"])

            let descriptor = try await session.putObject(body: body)
            return (descriptor, checksum.hex)
        }
        let descriptor = upload.0
        let md5Hex = upload.1
        state.multipartUploadID = nil
        state.uploadedParts = [
            PersistedUploadedPart(
                partNumber: 1,
                etag: descriptor.etag,
                offsetStart: 0,
                partSize: bodySize,
                md5Hex: md5Hex
            ),
        ]
        state.phase = .multipartCompleted
        state.updatedAt = await self.dependencies.clock.now()
        try await self.dependencies.stateStore.saveActive(state)

        return DataPlaneUploadCompletion(completedPartCount: 1, ossObjectETag: descriptor.etag)
    }

    private func uploadMultipart(
        state: inout PersistedUploadState,
        session: any UploadCoordinatorMultipartSessionProtocol,
        existingMultipartUploadID: String?,
        onEvent: (@Sendable (UploadEvent) async -> Void)?
    ) async throws -> DataPlaneUploadCompletion {
        let multipartUploadID: String
        if let existingMultipartUploadID {
            multipartUploadID = existingMultipartUploadID
        } else {
            await onEvent?(.initiatingMultipart(uploadID: state.uploadID))
            multipartUploadID = try await session.initiateMultipartUpload()
            state.multipartUploadID = multipartUploadID
            state.phase = .multipartInitiated
            state.updatedAt = await self.dependencies.clock.now()
            try await self.dependencies.stateStore.saveActive(state)
        }

        let partSize = state.partSizeBytes
        guard partSize > 0 else {
            throw DataGatewayClientError.invalidConfiguration("partSizeBytes must be greater than 0")
        }
        let partCount = Int((state.fileSize + partSize - 1) / partSize)
        var persistedPartsByNumber = Dictionary(uniqueKeysWithValues: state.uploadedParts.map { ($0.partNumber, $0) })
        var uploadedDescriptors = state.uploadedParts
            .sorted(by: { $0.partNumber < $1.partNumber })
            .map {
                UploadedPartDescriptor(
                    partNumber: $0.partNumber,
                    etag: $0.etag,
                    size: Int64($0.partSize),
                    lastModified: nil,
                    hashCRC64: nil
                )
            }

        for index in 0 ..< partCount {
            let partNumber = index + 1
            if persistedPartsByNumber[partNumber] != nil {
                continue
            }

            let offsetStart = UInt64(index) * partSize
            let currentPartSize = min(partSize, state.fileSize - offsetStart)
            try await self.refreshUploadSessionIfNeeded(session: session, state: &state, onEvent: onEvent)
            await onEvent?(.uploadingPart(partNumber: partNumber, sentBytes: currentPartSize, totalBytes: state.fileSize))
            await self.emitMetric("upload_part", dimensions: ["upload_id": state.uploadID, "part_number": String(partNumber)])

            let upload = try await self.dependencies.fileCoordinator.accessPreparedFile(
                fileURL: state.managedFileURL,
                bookmarkData: state.fileURLBookmarkData
            ) { accessibleFileURL in
                let checksum = try self.dependencies.fileCoordinator.md5Checksum(
                    from: accessibleFileURL,
                    offset: offsetStart,
                    length: currentPartSize
                )
                let fileCoordinator = self.dependencies.fileCoordinator
                let descriptor = try await session.uploadPart(
                    multipartUploadID: multipartUploadID,
                    partNumber: partNumber,
                    body: .stream(
                        sizeBytes: try Self.int64Size(currentPartSize),
                        contentMD5Base64: checksum.base64
                    ) {
                        try fileCoordinator.inputStream(
                            from: accessibleFileURL,
                            offset: offsetStart,
                            length: currentPartSize
                        )
                    }
                )
                return (descriptor, checksum.hex)
            }
            let descriptor = upload.0
            let md5Hex = upload.1
            uploadedDescriptors.append(descriptor)
            persistedPartsByNumber[partNumber] = PersistedUploadedPart(
                partNumber: partNumber,
                etag: descriptor.etag,
                offsetStart: offsetStart,
                partSize: currentPartSize,
                md5Hex: md5Hex
            )
            state.uploadedParts = persistedPartsByNumber.values.sorted(by: { $0.partNumber < $1.partNumber })
            state.phase = .uploading
            state.updatedAt = await self.dependencies.clock.now()
            try await self.dependencies.stateStore.saveActive(state)
        }

        uploadedDescriptors.sort(by: { $0.partNumber < $1.partNumber })
        try await self.refreshUploadSessionIfNeeded(session: session, state: &state, onEvent: onEvent)
        await onEvent?(.completingMultipart(uploadID: state.uploadID))
        let ossObjectETag = try await session.completeMultipartUpload(
            multipartUploadID: multipartUploadID,
            parts: uploadedDescriptors
        )

        state.phase = .multipartCompleted
        state.updatedAt = await self.dependencies.clock.now()
        try await self.dependencies.stateStore.saveActive(state)

        return DataPlaneUploadCompletion(
            completedPartCount: Int32(uploadedDescriptors.count),
            ossObjectETag: ossObjectETag
        )
    }

    private func completePersistedSingleObjectOrRestart(
        state: PersistedUploadState,
        session: any UploadCoordinatorMultipartSessionProtocol,
        onEvent: (@Sendable (UploadEvent) async -> Void)?
    ) async throws -> UploadResult {
        guard let persistedETag = state.uploadedParts.first?.etag.nilIfBlank else {
            return try await self.restartUpload(state: state, onEvent: onEvent)
        }

        let remoteETag: String
        do {
            remoteETag = try await session.headObjectETag()
        } catch let error as DataGatewayClientError {
            if Self.isObjectMissing(error) {
                return try await self.restartUpload(state: state, onEvent: onEvent)
            }
            throw error
        }

        guard Self.etagsMatch(remoteETag, persistedETag) else {
            return try await self.restartUpload(state: state, onEvent: onEvent)
        }

        var resumedState = state
        let result = try await self.completeBusinessUpload(
            state: &resumedState,
            uploadCompletion: DataPlaneUploadCompletion(completedPartCount: 1, ossObjectETag: remoteETag),
            onEvent: onEvent
        )
        await onEvent?(.completed(result))
        return result
    }

    private func completeBusinessUpload(
        state: inout PersistedUploadState,
        uploadCompletion: DataPlaneUploadCompletion,
        onEvent: (@Sendable (UploadEvent) async -> Void)?
    ) async throws -> UploadResult {
        try self.dependencies.fileCoordinator.validatePreparedFile(
            managedFileURL: state.managedFileURL,
            bookmarkData: state.fileURLBookmarkData,
            expectedFingerprint: state.fileFingerprint
        )
        await onEvent?(.completingBusinessUpload(uploadID: state.uploadID))
        _ = try await self.dependencies.gatewayClient.completeUpload(
            uploadID: state.uploadID,
            fileSize: Int64(state.fileSize),
            rawTags: state.rawTags,
            completedPartCount: uploadCompletion.completedPartCount,
            ossObjectEtag: uploadCompletion.ossObjectETag,
            partSizeBytes: Int64(state.partSizeBytes)
        )

        state.phase = .businessCompleting
        state.updatedAt = await self.dependencies.clock.now()
        try await self.dependencies.stateStore.moveToCompleted(state)

        return UploadResult(
            logicalUploadID: state.logicalUploadID,
            uploadID: state.uploadID,
            bucket: state.bucket,
            objectKey: state.objectKey,
            fileSize: state.fileSize,
            ossObjectETag: uploadCompletion.ossObjectETag
        )
    }

    private func refreshUploadSessionIfNeeded(
        session: any UploadCoordinatorMultipartSessionProtocol,
        state: inout PersistedUploadState,
        onEvent: (@Sendable (UploadEvent) async -> Void)?
    ) async throws {
        let refreshed = try await session.ensureFreshCredentialsIfNeeded()
        guard refreshed else {
            return
        }

        await onEvent?(.refreshingCredentials(uploadID: state.uploadID))
        await self.emitLog(operation: "refresh_credentials", uploadID: state.uploadID, logicalUploadID: state.logicalUploadID, phase: "refreshing", message: "refreshing upload credentials")
        await self.emitMetric("credentials_refresh", dimensions: ["upload_id": state.uploadID])
        state.lastKnownSTSExpireAt = await session.lastKnownCredentialExpiration()
        state.updatedAt = await self.dependencies.clock.now()
        try await self.dependencies.stateStore.saveActive(state)
    }

    private func emitLog(
        operation: String,
        uploadID: String? = nil,
        logicalUploadID: String? = nil,
        phase: String? = nil,
        attempt: Int? = nil,
        statusCode: Int? = nil,
        detailCode: String? = nil,
        message: String
    ) async {
        guard let onLog = self.dependencies.observability.onLog else {
            return
        }
        await onLog(
            DataGatewayClientLogEvent(
                operation: operation,
                uploadID: uploadID,
                logicalUploadID: logicalUploadID,
                phase: phase,
                attempt: attempt,
                statusCode: statusCode,
                detailCode: detailCode,
                message: Self.redactSensitiveContent(in: message)
            )
        )
    }

    private func emitMetric(_ name: String, dimensions: [String: String]) async {
        guard let onMetric = self.dependencies.observability.onMetric else {
            return
        }
        await onMetric(name, dimensions)
    }

    package static func redactSensitiveContent(in message: String) -> String {
        let lowered = message.lowercased()
        if lowered.contains("credential") || lowered.contains("token") || lowered.contains("accesskey") || lowered.contains("secret") {
            return "[REDACTED]"
        }
        return message
    }

    private static func makeDate(fromUnix unix: Int64) -> Date? {
        guard unix > 0 else {
            return nil
        }
        return Date(timeIntervalSince1970: TimeInterval(unix))
    }

    private static func shouldUseSingleObjectUpload(fileSize: UInt64, partSizeBytes: UInt64) -> Bool {
        fileSize <= partSizeBytes
    }

    private static func isPersistedSingleObjectCompleted(_ state: PersistedUploadState) -> Bool {
        state.phase == .multipartCompleted
            && state.multipartUploadID?.nilIfBlank == nil
            && state.uploadedParts.count == 1
    }

    private static func isObjectMissing(_ error: DataGatewayClientError) -> Bool {
        guard case .ossFailed(let httpStatus, let ossCode, _) = error else {
            return false
        }
        return httpStatus == 404 || ossCode == "NotFound" || ossCode == "NoSuchKey"
    }

    private static func etagsMatch(_ lhs: String, _ rhs: String) -> Bool {
        self.canonicalETag(lhs) == self.canonicalETag(rhs)
    }

    private static func canonicalETag(_ value: String) -> String {
        var trimmed = value.trimmingCharacters(in: .whitespacesAndNewlines)
        if trimmed.hasPrefix("\""), trimmed.hasSuffix("\""), trimmed.count >= 2 {
            trimmed.removeFirst()
            trimmed.removeLast()
        }
        return trimmed.lowercased()
    }

    package static func decideResumeAction(
        state: PersistedUploadState,
        recovery: Archebase_DataGateway_V1_GetUploadRecoveryResponse
    ) -> ResumeDecision {
        let uploadID = recovery.currentUploadID.nilIfBlank ?? state.uploadID

        switch recovery.nextAction {
        case .continue:
            return .continueExisting(uploadID: uploadID)
        case .completeOnly:
            return .completeOnly(
                uploadID: uploadID,
                expectedObjectETag: recovery.ossObjectEtag.nilIfBlank
            )
        case .restart:
            return .restartUpload(previousUploadID: uploadID)
        case .abort, .unspecified:
            return .permanentFailure(
                reason: recovery.terminalReason.nilIfBlank ?? "gateway requested abort"
            )
        case .UNRECOGNIZED(let rawValue):
            return .permanentFailure(
                reason: "gateway returned unrecognized recovery action: \(rawValue)"
            )
        }
    }

    private func reconcileRemoteParts(
        state: PersistedUploadState,
        multipartUploadID: String,
        session: any UploadCoordinatorMultipartSessionProtocol
    ) async throws -> ReconcileRemotePartsDecision {
        let remoteParts = try await session.listParts(multipartUploadID: multipartUploadID)
        let localPartsByNumber = Dictionary(uniqueKeysWithValues: state.uploadedParts.map { ($0.partNumber, $0) })
        let remotePartsByNumber = Dictionary(uniqueKeysWithValues: remoteParts.map {
            ($0.partNumber, PersistedUploadedPart(
                partNumber: $0.partNumber,
                etag: $0.etag,
                offsetStart: UInt64(max(0, ($0.partNumber - 1)) * Int(state.partSizeBytes)),
                partSize: UInt64($0.size ?? Int64(state.partSizeBytes)),
                md5Hex: localPartsByNumber[$0.partNumber]?.md5Hex ?? ""
            ))
        })

        if remoteParts.isEmpty {
            return .restartUpload
        }

        var merged: [Int: PersistedUploadedPart] = [:]
        for (partNumber, localPart) in localPartsByNumber {
            if let remotePart = remotePartsByNumber[partNumber] {
                if remotePart.etag != localPart.etag {
                    return .restartUpload
                }
                merged[partNumber] = localPart
            }
        }

        for (partNumber, remotePart) in remotePartsByNumber where merged[partNumber] == nil {
            merged[partNumber] = remotePart
        }

        return .continueUpload(merged.values.sorted(by: { $0.partNumber < $1.partNumber }))
    }

    private static func int64Size(_ size: UInt64) throws -> Int64 {
        guard size <= UInt64(Int64.max) else {
            throw DataGatewayClientError.invalidLocalFile("local file size exceeds supported OSS request length")
        }
        return Int64(size)
    }
}

/// High-level client entry point for starting uploads.
public actor DataGatewayClient {
    private let uploadCoordinator: UploadCoordinator
    private let objectClient: (any ObjectControlPlaneClientProtocol)?
    private let runtimeResources: DataGatewayClientRuntimeResources?
    private let configTags: [String: String]

    public static func initialize(endpointsJSON: String, endpointsURL: URL) throws {
        try ArchebasePublicEndpoints.initialize(endpointsJSON: endpointsJSON, endpointsURL: endpointsURL)
    }

    /// Creates a fully wired client from the public configuration.
    public init(config: DataGatewayClientConfig) throws {
        try self.init(config: config, configTags: [:])
    }

    package init(config: DataGatewayClientConfig, configTags: [String: String]) throws {
        try config.validate()
        try ArchebaseConfig.validateTags(configTags)

        let authSecurity: ControlPlaneTransportSecurity = switch config.authTLS {
        case .plaintext: .plaintext
        case .tls: .tls
        }
        let gatewaySecurity: ControlPlaneTransportSecurity = switch config.gatewayTLS {
        case .plaintext: .plaintext
        case .tls: .tls
        }

        let authFactory = ControlPlaneClientFactory(
            configuration: ControlPlaneTransportConfiguration(
                endpoint: config.authEndpoint,
                security: authSecurity,
                requestTimeout: config.requestTimeout
            )
        )
        let authTransport = try authFactory.makeAuthTransport()
        let authProvider = CredentialAuthProvider(
            credentialBase64: config.credentialBase64,
            refreshBefore: config.authRefreshBefore,
            requestTimeout: config.requestTimeout,
            transport: authTransport.serviceClient
        )

        let gatewayFactory = ControlPlaneClientFactory(
            configuration: ControlPlaneTransportConfiguration(
                endpoint: config.gatewayEndpoint,
                security: gatewaySecurity,
                requestTimeout: config.requestTimeout
            )
        )
        let gatewayTransport = try gatewayFactory.makeGatewayClient()
        let objectTransport = try gatewayFactory.makeObjectClient()
        let retryingGateway = AnyUploadCoordinatorGatewayClient(
            authProvider: authProvider,
            gatewayServiceClient: gatewayTransport.serviceClient,
            requestTimeout: config.requestTimeout,
            retryPolicy: config.retryPolicy.controlPlane.controlPlaneValue
        )
        let objectClient = RetryingObjectControlPlaneClient(
            objectClient: ObjectControlPlaneClient(
                client: objectTransport.serviceClient,
                requestTimeout: config.requestTimeout
            ),
            retryPolicy: config.retryPolicy.controlPlane.controlPlaneValue
        )

        let stateStore = UploadStateStore(persistRoot: config.persistRootURL)
        let fileCoordinator = FileStagingCoordinator(
            stagingRoot: config.persistRootURL
                .appendingPathComponent("data-gateway-client", isDirectory: true)
                .appendingPathComponent("staging", isDirectory: true)
        )

        let dependencies = UploadCoordinatorDependencies(
            gatewayClient: retryingGateway,
            stateStore: stateStore,
            fileCoordinator: fileCoordinator,
            ossClientFactory: { uploadContext in
                if Self.useMockOssFromEnvironment() {
                    return LocalStackMockMultipartSession(uploadID: uploadContext.uploadID)
                }
                return try OssUploadSession(
                    context: uploadContext,
                    refreshPolicy: STSRefreshPolicy(
                        refreshSkew: config.execution.credentialRefreshSkew,
                        requestTimeout: config.requestTimeout
                    ),
                    dataPlaneRetryPolicy: config.retryPolicy.dataPlane.controlPlaneValue,
                    requestTimeout: config.requestTimeout,
                    clientFactory: AlibabaOSSSDKClientFactory(),
                    credentialsProvider: retryingGateway
                )
            },
            observability: config.observability
        )

        self.uploadCoordinator = UploadCoordinator(
            executionPolicy: config.execution,
            dependencies: dependencies
        )
        self.objectClient = objectClient
        self.runtimeResources = DataGatewayClientRuntimeResources(
            authTransport: authTransport,
            gatewayTransport: gatewayTransport,
            objectTransport: objectTransport
        )
        self.configTags = configTags
    }

    private static func useMockOssFromEnvironment() -> Bool {
        guard let rawValue = ProcessInfo.processInfo.environment["DATA_GATEWAY_CLIENT_USE_MOCK_OSS"] else {
            return false
        }
        switch rawValue.trimmingCharacters(in: .whitespacesAndNewlines).lowercased() {
        case "1", "true", "yes", "on": return true
        default: return false
        }
    }

    /// Creates a fully wired client from `archebase-config.json`.
    public static func fromArchebaseConfig(
        configURL: URL,
        persistRootURL: URL,
        endpointsURL: URL,
        observability: DataGatewayClientObservability = .disabled
    ) async throws -> DataGatewayClient {
        let store = ArchebaseConfigStore(configURL: configURL)
        let archebaseConfig = try await store.load()
        let config = try DataGatewayClientConfig.recommended(
            credentialBase64: archebaseConfig.apiKey,
            persistRootURL: persistRootURL,
            endpointsURL: endpointsURL,
            observability: observability
        )
        return try DataGatewayClient(config: config, configTags: archebaseConfig.tags)
    }

    package static func testFromArchebaseConfig(
        authEndpoint: URL,
        gatewayEndpoint: URL,
        configURL: URL,
        persistRootURL: URL,
        tls: TLSMode = .plaintext,
        observability: DataGatewayClientObservability = .disabled
    ) async throws -> DataGatewayClient {
        let store = ArchebaseConfigStore(configURL: configURL)
        let archebaseConfig = try await store.load()
        let config = DataGatewayClientConfig.testRecommended(
            authEndpoint: authEndpoint,
            gatewayEndpoint: gatewayEndpoint,
            credentialBase64: archebaseConfig.apiKey,
            persistRootURL: persistRootURL,
            tls: tls,
            observability: observability
        )
        return try DataGatewayClient(config: config, configTags: archebaseConfig.tags)
    }

    package init(
        uploadCoordinator: UploadCoordinator,
        objectClient: (any ObjectControlPlaneClientProtocol)? = nil,
        runtimeResources: DataGatewayClientRuntimeResources? = nil,
        configTags: [String: String] = [:]
    ) {
        self.uploadCoordinator = uploadCoordinator
        self.objectClient = objectClient
        self.runtimeResources = runtimeResources
        self.configTags = configTags
    }

    /// Starts one new upload using the configured upload coordinator.
    public func upload(_ request: UploadRequest) async throws -> UploadResult {
        try await self.uploadCoordinator.upload(self.requestMergingConfigTags(request))
    }

    /// Starts one new upload and exposes phase/progress events for the same underlying state machine.
    public func uploadEvents(_ request: UploadRequest) -> AsyncThrowingStream<UploadEvent, Error> {
        AsyncThrowingStream { continuation in
            let task = Task {
                do {
                    let mergedRequest = try self.requestMergingConfigTags(request)
                    _ = try await self.uploadCoordinator.upload(mergedRequest) { event in
                        continuation.yield(event)
                    }
                    continuation.finish()
                } catch {
                    continuation.finish(throwing: error)
                }
            }

            continuation.onTermination = { _ in
                task.cancel()
            }
        }
    }

    /// Resumes one persisted upload task by its logical upload identifier.
    public func resumeUpload(logicalUploadID: String) async throws -> UploadResult {
        try await self.uploadCoordinator.resumeUpload(logicalUploadID: logicalUploadID)
    }

    /// Lists locally persisted uploads that are still pending in the `active/` snapshot namespace.
    public func listPendingUploads() async throws -> [PendingUploadInfo] {
        try await self.uploadCoordinator.listPendingUploads()
    }

    /// Lists verified logical objects visible to the supplied user bearer token.
    public func listObjects(
        _ options: ListObjectsOptions = ListObjectsOptions(),
        authorizationHeader: String
    ) async throws -> ListObjectsPage {
        guard let objectClient else {
            throw DataGatewayClientError.invalidConfiguration("data gateway object client is unavailable")
        }
        let trimmedAuthorizationHeader = authorizationHeader.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmedAuthorizationHeader.isEmpty else {
            throw DataGatewayClientError.invalidConfiguration("authorization header is required")
        }

        do {
            let response = try await objectClient.listObjects(
                pageSize: options.pageSize,
                pageToken: options.pageToken ?? "",
                filter: options.filter ?? "",
                authorizationHeader: trimmedAuthorizationHeader
            )
            return ListObjectsPage(proto: response)
        } catch let error as DataGatewayClientError {
            throw error
        } catch {
            throw ControlPlaneErrorMapper.map(error)
        }
    }

    /// Aborts one logical upload remotely and always removes its local snapshot on success or not-found.
    public func abortUpload(logicalUploadID: String) async throws {
        try await self.uploadCoordinator.abortUpload(logicalUploadID: logicalUploadID)
    }

    /// Deletes one local snapshot without making any remote call.
    public func deleteLocalSnapshot(logicalUploadID: String) async throws {
        try await self.uploadCoordinator.deleteLocalSnapshot(logicalUploadID: logicalUploadID)
    }

    private func requestMergingConfigTags(_ request: UploadRequest) throws -> UploadRequest {
        let mergedRawTags = try RawTagsMerger.merge(
            configTags: self.configTags,
            uploadRawTags: request.rawTags,
            sourceFileURL: request.fileURL
        )
        return UploadRequest(
            fileURL: request.fileURL,
            clientHints: request.clientHints,
            rawTags: mergedRawTags,
            displayName: request.displayName
        )
    }
}

package final class DataGatewayClientRuntimeResources: @unchecked Sendable {
    private let authTransport: ManagedControlPlaneServiceClient<any CredentialExchangeTransport>
    private let gatewayTransport: ManagedControlPlaneServiceClient<Archebase_DataGateway_V1_DataGatewayService.Client<HTTP2ClientTransport.TransportServices>>
    private let objectTransport: ManagedControlPlaneServiceClient<Archebase_DataGateway_V1_DataGatewayObjectService.Client<HTTP2ClientTransport.TransportServices>>

    package init(
        authTransport: ManagedControlPlaneServiceClient<any CredentialExchangeTransport>,
        gatewayTransport: ManagedControlPlaneServiceClient<Archebase_DataGateway_V1_DataGatewayService.Client<HTTP2ClientTransport.TransportServices>>,
        objectTransport: ManagedControlPlaneServiceClient<Archebase_DataGateway_V1_DataGatewayObjectService.Client<HTTP2ClientTransport.TransportServices>>
    ) {
        self.authTransport = authTransport
        self.gatewayTransport = gatewayTransport
        self.objectTransport = objectTransport
    }
}

package struct AnyUploadCoordinatorGatewayClient: UploadCoordinatorGatewayClient, GatewayUploadCredentialsProvider {
    private let createLogicalUploadHandler: @Sendable ([String: String], String?) async throws -> Archebase_DataGateway_V1_CreateLogicalUploadResponse
    private let getUploadRecoveryHandler: @Sendable (String) async throws -> Archebase_DataGateway_V1_GetUploadRecoveryResponse
    private let reissueUploadCredentialsHandler: @Sendable (String) async throws -> Archebase_DataGateway_V1_ReissueUploadCredentialsResponse
    private let abortUploadHandler: @Sendable (String, String) async throws -> Archebase_DataGateway_V1_AbortUploadResponse
    private let completeUploadHandler: @Sendable (String, Int64, [String: String], Int32, String, Int64) async throws -> Archebase_DataGateway_V1_CompleteUploadResponse

    package init(
        authProvider: CredentialAuthProvider,
        gatewayServiceClient: Archebase_DataGateway_V1_DataGatewayService.Client<HTTP2ClientTransport.TransportServices>,
        requestTimeout: Duration,
        retryPolicy: DGWControlPlane.RetryPolicy
        ) {
        let retryingClient = AuthenticatedGatewayControlPlaneClient(
            authProvider: authProvider,
            gatewayClient: GatewayControlPlaneClient(
                client: gatewayServiceClient,
                requestTimeout: requestTimeout
            ),
            retryPolicy: retryPolicy
        )
        self.createLogicalUploadHandler = { clientHints, restartFromUploadID in
            do {
                return try await retryingClient.createLogicalUpload(clientHints: clientHints, restartFromUploadID: restartFromUploadID)
            } catch {
                throw ControlPlaneErrorMapper.map(error)
            }
        }
        self.getUploadRecoveryHandler = { logicalUploadID in
            do {
                return try await retryingClient.getUploadRecovery(logicalUploadID: logicalUploadID)
            } catch {
                throw ControlPlaneErrorMapper.map(error)
            }
        }
        self.reissueUploadCredentialsHandler = { uploadID in
            do {
                return try await retryingClient.reissueUploadCredentials(uploadID: uploadID)
            } catch {
                throw ControlPlaneErrorMapper.map(error)
            }
        }
        self.abortUploadHandler = { logicalUploadID, reason in
            do {
                return try await retryingClient.abortUpload(logicalUploadID: logicalUploadID, reason: reason)
            } catch {
                throw ControlPlaneErrorMapper.map(error)
            }
        }
        self.completeUploadHandler = { uploadID, fileSize, rawTags, completedPartCount, ossObjectEtag, partSizeBytes in
            do {
                return try await retryingClient.completeUpload(
                    uploadID: uploadID,
                    fileSize: fileSize,
                    rawTags: rawTags,
                    completedPartCount: completedPartCount,
                    ossObjectEtag: ossObjectEtag,
                    partSizeBytes: partSizeBytes
                )
            } catch {
                throw ControlPlaneErrorMapper.map(error)
            }
        }
    }

    package func createLogicalUpload(
        clientHints: [String : String],
        restartFromUploadID: String?
    ) async throws -> Archebase_DataGateway_V1_CreateLogicalUploadResponse {
        try await self.createLogicalUploadHandler(clientHints, restartFromUploadID)
    }

    package func getUploadRecovery(
        logicalUploadID: String
    ) async throws -> Archebase_DataGateway_V1_GetUploadRecoveryResponse {
        try await self.getUploadRecoveryHandler(logicalUploadID)
    }

    package func reissueUploadCredentials(
        uploadID: String
    ) async throws -> Archebase_DataGateway_V1_ReissueUploadCredentialsResponse {
        try await self.reissueUploadCredentialsHandler(uploadID)
    }

    package func abortUpload(
        logicalUploadID: String,
        reason: String
    ) async throws -> Archebase_DataGateway_V1_AbortUploadResponse {
        try await self.abortUploadHandler(logicalUploadID, reason)
    }

    package func completeUpload(
        uploadID: String,
        fileSize: Int64,
        rawTags: [String : String],
        completedPartCount: Int32,
        ossObjectEtag: String,
        partSizeBytes: Int64
    ) async throws -> Archebase_DataGateway_V1_CompleteUploadResponse {
        try await self.completeUploadHandler(uploadID, fileSize, rawTags, completedPartCount, ossObjectEtag, partSizeBytes)
    }
}

private extension String {
    var nilIfBlank: String? {
        let trimmed = self.trimmingCharacters(in: .whitespacesAndNewlines)
        return trimmed.isEmpty ? nil : trimmed
    }
}
