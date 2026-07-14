import DGWControlPlane
import DGWStore
import Foundation
import Testing

@testable import DataGatewayClient

final class PassthroughSecurityScopedAccessor: SecurityScopedFileAccessing, @unchecked Sendable {
    func access<Result>(_ fileURL: URL, operation: @Sendable () throws -> Result) throws -> Result where Result: Sendable {
        _ = fileURL
        return try operation()
    }

    func access<Result>(_ fileURL: URL, operation: @Sendable () async throws -> Result) async throws -> Result where Result: Sendable {
        _ = fileURL
        return try await operation()
    }

    func access<Result>(
        _ fileURL: URL,
        bookmarkData: Data?,
        operation: @Sendable (_ accessibleURL: URL) throws -> Result
    ) throws -> Result where Result: Sendable {
        _ = bookmarkData
        return try operation(fileURL)
    }

    func access<Result>(
        _ fileURL: URL,
        bookmarkData: Data?,
        operation: @Sendable (_ accessibleURL: URL) async throws -> Result
    ) async throws -> Result where Result: Sendable {
        _ = bookmarkData
        return try await operation(fileURL)
    }

    func bookmarkData(for fileURL: URL) throws -> Data {
        Data("bookmark:\(fileURL.path)".utf8)
    }
}

final class RecordingSecurityScopedAccessor: SecurityScopedFileAccessing, @unchecked Sendable {
    struct AccessRecord: Equatable {
        let fileURL: URL
        let bookmarkData: Data?
    }

    private let lock = NSLock()
    private var records: [AccessRecord] = []

    func access<Result>(_ fileURL: URL, operation: @Sendable () throws -> Result) throws -> Result where Result: Sendable {
        self.record(fileURL: fileURL, bookmarkData: nil)
        return try operation()
    }

    func access<Result>(_ fileURL: URL, operation: @Sendable () async throws -> Result) async throws -> Result where Result: Sendable {
        self.record(fileURL: fileURL, bookmarkData: nil)
        return try await operation()
    }

    func access<Result>(
        _ fileURL: URL,
        bookmarkData: Data?,
        operation: @Sendable (_ accessibleURL: URL) throws -> Result
    ) throws -> Result where Result: Sendable {
        self.record(fileURL: fileURL, bookmarkData: bookmarkData)
        return try operation(fileURL)
    }

    func access<Result>(
        _ fileURL: URL,
        bookmarkData: Data?,
        operation: @Sendable (_ accessibleURL: URL) async throws -> Result
    ) async throws -> Result where Result: Sendable {
        self.record(fileURL: fileURL, bookmarkData: bookmarkData)
        return try await operation(fileURL)
    }

    func bookmarkData(for fileURL: URL) throws -> Data {
        Data("bookmark:\(fileURL.path)".utf8)
    }

    func accessRecords() -> [AccessRecord] {
        self.lock.lock()
        defer { self.lock.unlock() }
        return self.records
    }

    private func record(fileURL: URL, bookmarkData: Data?) {
        self.lock.lock()
        defer { self.lock.unlock() }
        self.records.append(AccessRecord(fileURL: fileURL, bookmarkData: bookmarkData))
    }
}

final class RecordingStreamDelegate: NSObject, StreamDelegate {
    private(set) var events: [Stream.Event] = []

    func stream(_ aStream: Stream, handle eventCode: Stream.Event) {
        _ = aStream
        self.events.append(eventCode)
    }
}

final class MemoryFileSystem: FileSystemProviding, @unchecked Sendable {
    struct Entry {
        let size: UInt64
        let modifiedAt: Date?
        let data: Data

        static func file(size: UInt64, modifiedAt: Date?, data: Data) -> Entry {
            Entry(size: size, modifiedAt: modifiedAt, data: data)
        }
    }

    private var storage: [URL: Entry]
    private var copyRecords: [(URL, URL)] = []

    init(files: [URL: Entry]) {
        self.storage = files.mapKeys { $0.standardizedFileURL }
    }

    func fileExists(at url: URL) -> Bool {
        self.storage[url.standardizedFileURL] != nil
    }

    func attributes(at url: URL) throws -> [FileAttributeKey : Any] {
        guard let entry = self.storage[url.standardizedFileURL] else {
            throw CocoaError(.fileNoSuchFile)
        }
        var attributes: [FileAttributeKey: Any] = [.size: NSNumber(value: entry.size)]
        if let modifiedAt = entry.modifiedAt {
            attributes[.modificationDate] = modifiedAt
        }
        return attributes
    }

    func read(prefixFrom url: URL, maxLength: Int) throws -> Data {
        guard let entry = self.storage[url.standardizedFileURL] else {
            throw CocoaError(.fileNoSuchFile)
        }
        return Data(entry.data.prefix(maxLength))
    }

    func readAll(from url: URL) throws -> Data {
        guard let entry = self.storage[url.standardizedFileURL] else {
            throw CocoaError(.fileNoSuchFile)
        }
        return entry.data
    }

    func readRange(from url: URL, offset: UInt64, maxLength: Int) throws -> Data {
        guard let entry = self.storage[url.standardizedFileURL] else {
            throw CocoaError(.fileNoSuchFile)
        }
        let start = min(Int(offset), entry.data.count)
        let end = min(start + maxLength, entry.data.count)
        return entry.data.subdata(in: start ..< end)
    }

    func inputStream(from url: URL, offset: UInt64, length: UInt64) throws -> InputStream {
        let data = try self.readRange(from: url, offset: offset, maxLength: Int(length))
        return InputStream(data: data)
    }

    func createDirectory(at url: URL) throws {
        _ = url
    }

    func copyItem(at sourceURL: URL, to destinationURL: URL) throws {
        let source = sourceURL.standardizedFileURL
        let destination = destinationURL.standardizedFileURL
        guard let entry = self.storage[source] else {
            throw CocoaError(.fileNoSuchFile)
        }
        self.storage[destination] = entry
        self.copyRecords.append((source, destination))
    }

    func copiedItems() -> [(URL, URL)] {
        self.copyRecords
    }
}

func makePersistencePolicy(copyExternalFileIntoManagedStaging: Bool) -> LocalPersistencePolicy {
    LocalPersistencePolicy(
        keepTerminalSnapshot: true,
        keepCompletedSnapshot: false,
        completedSnapshotTTL: .seconds(0),
        terminalSnapshotTTL: .seconds(3600),
        copyExternalFileIntoManagedStaging: copyExternalFileIntoManagedStaging
    )
}

@Test func dataGatewayClientModuleNameIsStable() {
    #expect(DataGatewayClientModule.name == "DataGatewayClient")
}

@Test func endpointDecodeAcceptsCurrentContract() throws {
    let endpoints = try ArchebasePublicEndpoints.decodeEndpoints(validEndpointsJSON().data(using: .utf8)!)

    #expect(endpoints.auth == URL(string: "http://auth.example.com:50051")!)
    #expect(endpoints.gateway == URL(string: "http://gateway.example.com:50053")!)
    #expect(endpoints.deviceInit == URL(string: "https://init.example.com:443")!)
    #expect(endpoints.authTLS == .plaintext)
    #expect(endpoints.gatewayTLS == .plaintext)
    #expect(endpoints.deviceInitTLS == .tls)
}

@Test func endpointNormalizationProducesStableJSONForEquivalentInput() throws {
    let reorderedEndpointsJSON = """
    {
      "deviceInit": { "port": 443, "host": "init.example.com", "scheme": "https" },
      "gateway": { "host": "gateway.example.com", "port": 50053, "scheme": "http" },
      "auth": { "scheme": "http", "port": 50051, "host": "auth.example.com" }
    }
    """

    let normalized = try ArchebasePublicEndpoints.normalizedJSONString(endpointsJSON: validEndpointsJSON())
    let reorderedNormalized = try ArchebasePublicEndpoints.normalizedJSONString(endpointsJSON: reorderedEndpointsJSON)

    #expect(normalized == reorderedNormalized)
    #expect(!normalized.contains("device_id"))

    let decoded = try ArchebasePublicEndpoints.decodeEndpoints(Data(normalized.utf8))
    #expect(decoded.auth == URL(string: "http://auth.example.com:50051")!)
    #expect(decoded.gateway == URL(string: "http://gateway.example.com:50053")!)
    #expect(decoded.deviceInit == URL(string: "https://init.example.com:443")!)
}

@Test func endpointDecodeRejectsLegacySchemaField() {
    let legacySchema = Data("""
    {
      "auth": { "schema": "http", "host": "auth.example.com", "port": 50051 },
      "gateway": { "scheme": "http", "host": "gateway.example.com", "port": 50053 },
      "deviceInit": { "scheme": "https", "host": "init.example.com", "port": 443 }
    }
    """.utf8)

    #expect(throws: DataGatewayClientError.self) {
        try ArchebasePublicEndpoints.decodeEndpoints(legacySchema)
    }
}

@Test func endpointDecodeRejectsInvalidScheme() {
    let invalidScheme = Data("""
    {
      "auth": { "scheme": "grpc", "host": "auth.example.com", "port": 50051 },
      "gateway": { "scheme": "http", "host": "gateway.example.com", "port": 50053 },
      "deviceInit": { "scheme": "https", "host": "init.example.com", "port": 443 }
    }
    """.utf8)

    #expect(throws: DataGatewayClientError.self) {
        try ArchebasePublicEndpoints.decodeEndpoints(invalidScheme)
    }
}

@Test func endpointDecodeRejectsEmptyHost() {
    let emptyHost = Data("""
    {
      "auth": { "scheme": "http", "host": "   ", "port": 50051 },
      "gateway": { "scheme": "http", "host": "gateway.example.com", "port": 50053 },
      "deviceInit": { "scheme": "https", "host": "init.example.com", "port": 443 }
    }
    """.utf8)

    #expect(throws: DataGatewayClientError.self) {
        try ArchebasePublicEndpoints.decodeEndpoints(emptyHost)
    }
}

@Test func endpointDecodeRejectsPortBelowRange() {
    let invalidPort = Data("""
    {
      "auth": { "scheme": "http", "host": "auth.example.com", "port": 0 },
      "gateway": { "scheme": "http", "host": "gateway.example.com", "port": 50053 },
      "deviceInit": { "scheme": "https", "host": "init.example.com", "port": 443 }
    }
    """.utf8)

    #expect(throws: DataGatewayClientError.self) {
        try ArchebasePublicEndpoints.decodeEndpoints(invalidPort)
    }
}

@Test func endpointDecodeRejectsPortAboveRange() {
    let invalidPort = Data("""
    {
      "auth": { "scheme": "http", "host": "auth.example.com", "port": 65536 },
      "gateway": { "scheme": "http", "host": "gateway.example.com", "port": 50053 },
      "deviceInit": { "scheme": "https", "host": "init.example.com", "port": 443 }
    }
    """.utf8)

    #expect(throws: DataGatewayClientError.self) {
        try ArchebasePublicEndpoints.decodeEndpoints(invalidPort)
    }
}

@Test func endpointLoadRejectsMissingFile() throws {
    let root = try filePreparationTemporaryRoot()
    let endpointsURL = root.appendingPathComponent(ArchebasePublicEndpoints.endpointsFileName)

    let error = #expect(throws: DataGatewayClientError.self) {
        try ArchebasePublicEndpoints.load(endpointsURL: endpointsURL)
    }

    #expect(error == .endpointsNotInitialized(endpointsURL: endpointsURL.standardizedFileURL))
}

@Test func endpointInitializeWritesValidatedJSON() throws {
    let root = try filePreparationTemporaryRoot()
    let endpointsURL = root.appendingPathComponent(ArchebasePublicEndpoints.endpointsFileName)

    try DataGatewayClient.initialize(endpointsJSON: validEndpointsJSON(), endpointsURL: endpointsURL)

    #expect(FileManager.default.fileExists(atPath: endpointsURL.path))
    let endpoints = try ArchebasePublicEndpoints.load(endpointsURL: endpointsURL)
    #expect(endpoints.gateway == URL(string: "http://gateway.example.com:50053")!)
}

@Test func endpointInitializeLoadsFromDirectoryWithSpaces() throws {
    let root = try filePreparationTemporaryRoot()
        .appendingPathComponent("Application Support", isDirectory: true)
        .appendingPathComponent("Archebase", isDirectory: true)
    let endpointsURL = root.appendingPathComponent(ArchebasePublicEndpoints.endpointsFileName)

    try DataGatewayClient.initialize(endpointsJSON: validEndpointsJSON(), endpointsURL: endpointsURL)

    #expect(FileManager.default.fileExists(atPath: endpointsURL.path))
    let endpoints = try ArchebasePublicEndpoints.load(endpointsURL: endpointsURL)
    #expect(endpoints.deviceInit == URL(string: "https://init.example.com:443")!)
}

@Test func endpointInitializeRejectsInvalidJSONWithoutCreatingFile() throws {
    let root = try filePreparationTemporaryRoot()
    let endpointsURL = root.appendingPathComponent(ArchebasePublicEndpoints.endpointsFileName)

    #expect(throws: DataGatewayClientError.self) {
        try DataGatewayClient.initialize(endpointsJSON: "{", endpointsURL: endpointsURL)
    }

    #expect(!FileManager.default.fileExists(atPath: endpointsURL.path))
}

@Test func endpointInitializeIsIdempotentForEquivalentEndpoints() throws {
    let root = try filePreparationTemporaryRoot()
    let endpointsURL = root.appendingPathComponent(ArchebasePublicEndpoints.endpointsFileName)

    try DataGatewayClient.initialize(endpointsJSON: validEndpointsJSON(), endpointsURL: endpointsURL)
    try DataGatewayClient.initialize(endpointsJSON: validEndpointsJSON(), endpointsURL: endpointsURL)
}

@Test func endpointInitializeRejectsDifferentExistingEndpoints() throws {
    let root = try filePreparationTemporaryRoot()
    let endpointsURL = root.appendingPathComponent(ArchebasePublicEndpoints.endpointsFileName)

    try DataGatewayClient.initialize(endpointsJSON: validEndpointsJSON(), endpointsURL: endpointsURL)
    let error = #expect(throws: DataGatewayClientError.self) {
        try DataGatewayClient.initialize(
            endpointsJSON: validEndpointsJSON(authHost: "other-auth.example.com"),
            endpointsURL: endpointsURL
        )
    }

    #expect(error == .endpointsAlreadyInitialized(endpointsURL: endpointsURL.standardizedFileURL))
}

@Test func endpointReplaceOverwritesDifferentExistingEndpoints() throws {
    let root = try filePreparationTemporaryRoot()
    let endpointsURL = root.appendingPathComponent(ArchebasePublicEndpoints.endpointsFileName)

    try DataGatewayClient.initialize(endpointsJSON: validEndpointsJSON(), endpointsURL: endpointsURL)
    try ArchebasePublicEndpoints.replace(
        endpointsJSON: validEndpointsJSON(authHost: "replacement-auth.example.com"),
        endpointsURL: endpointsURL
    )

    let endpoints = try ArchebasePublicEndpoints.load(endpointsURL: endpointsURL)
    #expect(endpoints.auth == URL(string: "http://replacement-auth.example.com:50051")!)
}

@Test func endpointReplaceRejectsInvalidJSONWithoutChangingExistingFile() throws {
    let root = try filePreparationTemporaryRoot()
    let endpointsURL = root.appendingPathComponent(ArchebasePublicEndpoints.endpointsFileName)

    try DataGatewayClient.initialize(endpointsJSON: validEndpointsJSON(), endpointsURL: endpointsURL)
    let originalData = try Data(contentsOf: endpointsURL)

    #expect(throws: DataGatewayClientError.self) {
        try ArchebasePublicEndpoints.replace(endpointsJSON: "{", endpointsURL: endpointsURL)
    }

    #expect(try Data(contentsOf: endpointsURL) == originalData)
    let endpoints = try ArchebasePublicEndpoints.load(endpointsURL: endpointsURL)
    #expect(endpoints.auth == URL(string: "http://auth.example.com:50051")!)
}

@Test func endpointInitializeRejectsCorruptExistingFile() throws {
    let root = try filePreparationTemporaryRoot()
    let endpointsURL = root.appendingPathComponent(ArchebasePublicEndpoints.endpointsFileName)
    try Data("not-json".utf8).write(to: endpointsURL)

    #expect(throws: DataGatewayClientError.self) {
        try DataGatewayClient.initialize(endpointsJSON: validEndpointsJSON(), endpointsURL: endpointsURL)
    }

    #expect(String(data: try Data(contentsOf: endpointsURL), encoding: .utf8) == "not-json")
}

@Test func publicClientConfigLoadsPersistedEndpoints() throws {
    let root = try filePreparationTemporaryRoot()
    let endpointsURL = root.appendingPathComponent(ArchebasePublicEndpoints.endpointsFileName)
    try DataGatewayClient.initialize(endpointsJSON: validEndpointsJSON(), endpointsURL: endpointsURL)

    let config = try DataGatewayClientConfig.recommended(
        credentialBase64: "credential-base64",
        persistRootURL: root,
        endpointsURL: endpointsURL
    )

    #expect(config.authEndpoint == URL(string: "http://auth.example.com:50051")!)
    #expect(config.gatewayEndpoint == URL(string: "http://gateway.example.com:50053")!)
    #expect(config.authTLS == .plaintext)
    #expect(config.gatewayTLS == .plaintext)
    #expect(config.credentialBase64 == "credential-base64")
    #expect(config.persistRootURL == root)
    #expect(throws: Never.self) { try config.validate() }
}

@Test func publicClientConfigThrowsWhenEndpointsMissing() throws {
    let root = try filePreparationTemporaryRoot()
    let endpointsURL = root.appendingPathComponent(ArchebasePublicEndpoints.endpointsFileName)

    let error = #expect(throws: DataGatewayClientError.self) {
        _ = try DataGatewayClientConfig.recommended(
            credentialBase64: "credential-base64",
            persistRootURL: root,
            endpointsURL: endpointsURL
        )
    }

    #expect(error == .endpointsNotInitialized(endpointsURL: endpointsURL.standardizedFileURL))
}

@Test func publicDeviceInitConfigStoresEndpointsURLWithoutLoadingIt() throws {
    let root = try filePreparationTemporaryRoot()
    let configURL = root.appendingPathComponent("archebase-config.json")
    let endpointsURL = root.appendingPathComponent(ArchebasePublicEndpoints.endpointsFileName)
    let config = DeviceInitClientConfig(configURL: configURL, endpointsURL: endpointsURL)

    #expect(config.configURL == configURL)
    #expect(config.endpointsURL == endpointsURL)
    #expect(config.tls == nil)
}

@Test func publicDeviceInitializerLoadsPersistedDeviceInitEndpoint() throws {
    let root = try filePreparationTemporaryRoot()
    let configURL = root.appendingPathComponent("archebase-config.json")
    let endpointsURL = root.appendingPathComponent(ArchebasePublicEndpoints.endpointsFileName)
    try DataGatewayClient.initialize(endpointsJSON: validEndpointsJSON(), endpointsURL: endpointsURL)

    _ = try ArchebaseDeviceInitializer(
        config: DeviceInitClientConfig(configURL: configURL, endpointsURL: endpointsURL)
    )
}

@Test func publicDeviceInitializerThrowsWhenEndpointsMissing() throws {
    let root = try filePreparationTemporaryRoot()
    let configURL = root.appendingPathComponent("archebase-config.json")
    let endpointsURL = root.appendingPathComponent(ArchebasePublicEndpoints.endpointsFileName)

    let error = #expect(throws: DataGatewayClientError.self) {
        _ = try ArchebaseDeviceInitializer(
            config: DeviceInitClientConfig(configURL: configURL, endpointsURL: endpointsURL)
        )
    }

    #expect(error == .endpointsNotInitialized(endpointsURL: endpointsURL.standardizedFileURL))
}

private func filePreparationTemporaryRoot() throws -> URL {
    let root = FileManager.default.temporaryDirectory
        .appendingPathComponent("archebase-file-preparation-tests", isDirectory: true)
        .appendingPathComponent(UUID().uuidString, isDirectory: true)
    try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
    return root
}

private func validEndpointsJSON(authHost: String = "auth.example.com") -> String {
    """
    {
      "auth": { "scheme": "http", "host": "\(authHost)", "port": 50051 },
      "gateway": { "scheme": "http", "host": "gateway.example.com", "port": 50053 },
      "deviceInit": { "scheme": "https", "host": "init.example.com", "port": 443 }
    }
    """
}

private extension Dictionary {
    func mapKeys<NewKey: Hashable>(_ transform: (Key) -> NewKey) -> [NewKey: Value] {
        Dictionary<NewKey, Value>(uniqueKeysWithValues: self.map { (transform($0.key), $0.value) })
    }
}

@Test func zeroByteFileFailsBeforeAnyRemoteWork() {
    let fileURL = URL(fileURLWithPath: "/tmp/zero.bin")
    let filesystem = MemoryFileSystem(files: [
        fileURL: .file(size: 0, modifiedAt: Date(timeIntervalSince1970: 100), data: Data()),
    ])
    let coordinator = FileStagingCoordinator(
        stagingRoot: URL(fileURLWithPath: "/staging"),
        fileSystem: filesystem,
        securityScopedAccessor: PassthroughSecurityScopedAccessor()
    )

    let error = #expect(throws: DataGatewayClientError.self) {
        try coordinator.prepare(
            request: UploadRequest(fileURL: fileURL, clientHints: [:], rawTags: [:], displayName: nil),
            persistence: makePersistencePolicy(copyExternalFileIntoManagedStaging: false)
        )
    }

    #expect(error == .zeroByteFile)
}

@Test func externalFileUsesSourceURLWithoutManagedStagingCopy() throws {
    let sourceURL = URL(fileURLWithPath: "/external/photo.heic")
    let stagingRoot = URL(fileURLWithPath: "/sandbox/staging")
    let data = Data("robot-camera-data".utf8)
    let modifiedAt = Date(timeIntervalSince1970: 200)
    let filesystem = MemoryFileSystem(files: [
        sourceURL: .file(size: UInt64(data.count), modifiedAt: modifiedAt, data: data),
    ])
    let coordinator = FileStagingCoordinator(
        stagingRoot: stagingRoot,
        fileSystem: filesystem,
        securityScopedAccessor: PassthroughSecurityScopedAccessor()
    )

    let prepared = try coordinator.prepare(
        request: UploadRequest(fileURL: sourceURL, clientHints: [:], rawTags: [:], displayName: nil),
        persistence: makePersistencePolicy(copyExternalFileIntoManagedStaging: true)
    )

    #expect(prepared.sourceFileURL == sourceURL)
    #expect(prepared.managedFileURL == sourceURL)
    #expect(filesystem.copiedItems().isEmpty)
    #expect(prepared.fileSize == UInt64(data.count))
    #expect(prepared.fingerprint == LocalFileFingerprint(
        size: UInt64(data.count),
        modifiedAt: modifiedAt,
        firstChunkMD5Hex: "115EEAF7F69D1BF8FA4FAB891CB724C7"
    ))
    #expect(prepared.bookmarkData == Data("bookmark:/external/photo.heic".utf8))
}

@Test func fileRangeInputStreamSupportsCFNetworkDelegateOperations() throws {
    let root = try filePreparationTemporaryRoot()
    let sourceURL = root.appendingPathComponent("range-stream.bin")
    try Data("0123456789".utf8).write(to: sourceURL)
    let coordinator = FileStagingCoordinator(stagingRoot: root.appendingPathComponent("staging", isDirectory: true))
    let delegate = RecordingStreamDelegate()
    let stream = try coordinator.inputStream(from: sourceURL, offset: 2, length: 5)
    var buffer = [UInt8](repeating: 0, count: 8)

    stream.delegate = delegate
    stream.schedule(in: .current, forMode: .default)
    stream.open()
    let count = stream.read(&buffer, maxLength: buffer.count)
    stream.close()
    stream.remove(from: .current, forMode: .default)
    stream.delegate = nil

    #expect(count == 5)
    #expect(Data(buffer.prefix(count)) == Data("23456".utf8))
}

@Test func missingManagedFileMakesResumeImpossible() {
    let managedURL = URL(fileURLWithPath: "/sandbox/staging/missing.bin")
    let coordinator = FileStagingCoordinator(
        stagingRoot: URL(fileURLWithPath: "/sandbox/staging"),
        fileSystem: MemoryFileSystem(files: [:]),
        securityScopedAccessor: PassthroughSecurityScopedAccessor()
    )

    let error = #expect(throws: DataGatewayClientError.self) {
        try coordinator.validatePreparedFile(
            managedFileURL: managedURL,
            expectedFingerprint: LocalFileFingerprint(
                size: 128,
                modifiedAt: Date(timeIntervalSince1970: 100),
                firstChunkMD5Hex: "ABCDEF0123456789ABCDEF0123456789"
            )
        )
    }

    #expect(error == .resumeNotPossible("source file missing: /sandbox/staging/missing.bin"))
}

@Test func fingerprintMismatchMakesResumeImpossible() throws {
    let managedURL = URL(fileURLWithPath: "/sandbox/staging/demo.bin")
    let data = Data("robot-data-v2".utf8)
    let filesystem = MemoryFileSystem(files: [
        managedURL: .file(size: UInt64(data.count), modifiedAt: Date(timeIntervalSince1970: 100), data: data),
    ])
    let coordinator = FileStagingCoordinator(
        stagingRoot: URL(fileURLWithPath: "/sandbox/staging"),
        fileSystem: filesystem,
        securityScopedAccessor: PassthroughSecurityScopedAccessor()
    )

    let error = #expect(throws: DataGatewayClientError.self) {
        try coordinator.validatePreparedFile(
            managedFileURL: managedURL,
            expectedFingerprint: LocalFileFingerprint(
                size: UInt64(data.count),
                modifiedAt: Date(timeIntervalSince1970: 100),
                firstChunkMD5Hex: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
            )
        )
    }

    #expect(error == .resumeNotPossible("local file fingerprint changed"))
}

@Test func fingerprintValidationUsesBookmarkScopedAccess() throws {
    let managedURL = URL(fileURLWithPath: "/external/scoped-demo.bin")
    let bookmark = Data("bookmark:/external/scoped-demo.bin".utf8)
    let data = Data("robot-data-scoped".utf8)
    let accessor = RecordingSecurityScopedAccessor()
    let filesystem = MemoryFileSystem(files: [
        managedURL: .file(size: UInt64(data.count), modifiedAt: Date(timeIntervalSince1970: 100), data: data),
    ])
    let coordinator = FileStagingCoordinator(
        stagingRoot: URL(fileURLWithPath: "/sandbox/staging"),
        fileSystem: filesystem,
        securityScopedAccessor: accessor
    )

    try coordinator.validatePreparedFile(
        managedFileURL: managedURL,
        bookmarkData: bookmark,
        expectedFingerprint: LocalFileFingerprint(
            size: UInt64(data.count),
            modifiedAt: Date(timeIntervalSince1970: 100),
            firstChunkMD5Hex: "E0156588AFEF4061755164862427C151"
        )
    )

    #expect(accessor.accessRecords().contains(RecordingSecurityScopedAccessor.AccessRecord(
        fileURL: managedURL,
        bookmarkData: bookmark
    )))
}

@Test func fingerprintValidationToleratesFilesystemModifiedAtPrecisionDrift() throws {
    let managedURL = URL(fileURLWithPath: "/sandbox/staging/demo-drift.bin")
    let data = Data("robot-data-drift".utf8)
    let filesystem = MemoryFileSystem(files: [
        managedURL: .file(size: UInt64(data.count), modifiedAt: Date(timeIntervalSince1970: 100.900), data: data),
    ])
    let coordinator = FileStagingCoordinator(
        stagingRoot: URL(fileURLWithPath: "/sandbox/staging"),
        fileSystem: filesystem,
        securityScopedAccessor: PassthroughSecurityScopedAccessor()
    )

    #expect(throws: Never.self) {
        try coordinator.validatePreparedFile(
            managedFileURL: managedURL,
            expectedFingerprint: LocalFileFingerprint(
                size: UInt64(data.count),
                modifiedAt: Date(timeIntervalSince1970: 100.100),
                firstChunkMD5Hex: "5B0A5149AD41E87A29BCF9B37AD42DC4"
            )
        )
    }
}
