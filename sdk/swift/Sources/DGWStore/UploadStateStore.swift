import DGWControlPlane
import Foundation

private enum SnapshotNamespace: String, CaseIterable {
    case active
    case terminal
    case completed
}

private struct LocalFileIndex: Codable, Equatable {
    var entries: [String: String]

    static let empty = LocalFileIndex(entries: [:])
}

private struct JSONCodec: Sendable {
    func encode<T: Encodable>(_ value: T) throws -> Data {
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .custom { date, encoder in
            var container = encoder.singleValueContainer()
            try container.encode(date.timeIntervalSince1970)
        }
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        return try encoder.encode(value)
    }

    func decode<T: Decodable>(_ type: T.Type, from data: Data) throws -> T {
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .custom { decoder in
            let container = try decoder.singleValueContainer()
            if let value = try? container.decode(Double.self) {
                return Date(timeIntervalSince1970: value)
            }
            let value = try container.decode(String.self)
            if let date = Self.makeDateFormatter().date(from: value) ?? Self.makeLegacyDateFormatter().date(from: value) {
                return date
            }
            throw DecodingError.dataCorruptedError(in: container, debugDescription: "invalid ISO8601 date: \(value)")
        }
        return try decoder.decode(type, from: data)
    }

    private static func makeDateFormatter() -> ISO8601DateFormatter {
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return formatter
    }

    private static func makeLegacyDateFormatter() -> ISO8601DateFormatter {
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime]
        return formatter
    }
}

/// Retention policy for terminal and completed snapshots.
public struct SnapshotRetentionPolicy: Sendable {
    public var keepTerminalSnapshot: Bool
    public var keepCompletedSnapshot: Bool
    public var completedSnapshotTTL: Duration
    public var terminalSnapshotTTL: Duration

    public init(
        keepTerminalSnapshot: Bool,
        keepCompletedSnapshot: Bool,
        completedSnapshotTTL: Duration,
        terminalSnapshotTTL: Duration
    ) {
        self.keepTerminalSnapshot = keepTerminalSnapshot
        self.keepCompletedSnapshot = keepCompletedSnapshot
        self.completedSnapshotTTL = completedSnapshotTTL
        self.terminalSnapshotTTL = terminalSnapshotTTL
    }
}

public protocol UploadStateStoreClock: Sendable {
    func now() async -> Date
}

public struct SystemUploadStateStoreClock: UploadStateStoreClock {
    public init() {}

    public func now() async -> Date {
        Date()
    }
}

/// Persistence boundary for upload snapshots and local-file indexes.
public actor UploadStateStore {
    private let persistRoot: URL
    private let fileManager: FileManager
    private let jsonCodec: JSONCodec
    private let clock: any UploadStateStoreClock

    public init(
        persistRoot: URL,
        fileManager: FileManager = .default
    ) {
        self.init(persistRoot: persistRoot, fileManager: fileManager, clock: SystemUploadStateStoreClock())
    }

    public init(
        persistRoot: URL,
        fileManager: FileManager,
        clock: any UploadStateStoreClock
    ) {
        self.persistRoot = persistRoot
        self.fileManager = fileManager
        self.jsonCodec = JSONCodec()
        self.clock = clock
    }

    public func saveActive(_ state: PersistedUploadState) throws {
        try self.ensureLayout()
        try self.removeSnapshotIfPresent(logicalUploadID: state.logicalUploadID, in: .terminal)
        try self.removeSnapshotIfPresent(logicalUploadID: state.logicalUploadID, in: .completed)
        try self.writeSnapshot(state, namespace: .active)
        try self.upsertIndexEntry(for: state)
    }

    public func moveToTerminal(_ state: PersistedUploadState) throws {
        try self.ensureLayout()
        try self.removeSnapshotIfPresent(logicalUploadID: state.logicalUploadID, in: .active)
        try self.removeSnapshotIfPresent(logicalUploadID: state.logicalUploadID, in: .completed)
        try self.writeSnapshot(state, namespace: .terminal)
        try self.upsertIndexEntry(for: state)
    }

    public func moveToCompleted(_ state: PersistedUploadState) throws {
        try self.ensureLayout()
        try self.removeSnapshotIfPresent(logicalUploadID: state.logicalUploadID, in: .active)
        try self.removeSnapshotIfPresent(logicalUploadID: state.logicalUploadID, in: .terminal)
        try self.writeSnapshot(state, namespace: .completed)
        try self.removeIndexEntry(fileURL: state.managedFileURL)
    }

    public func loadSnapshot(logicalUploadID: String) throws -> PersistedUploadState? {
        try self.ensureLayout()
        for namespace in SnapshotNamespace.allCases {
            if let state = try self.loadSnapshot(logicalUploadID: logicalUploadID, namespace: namespace) {
                return state
            }
        }
        return nil
    }

    public func loadActiveSnapshot(logicalUploadID: String) throws -> PersistedUploadState? {
        try self.ensureLayout()
        return try self.loadSnapshot(logicalUploadID: logicalUploadID, namespace: .active)
    }

    public func listPendingUploads() throws -> [PendingUploadInfo] {
        try self.ensureLayout()
        let activeDirectory = self.snapshotDirectory(for: .active)
        let urls = try self.fileManager.contentsOfDirectory(
            at: activeDirectory,
            includingPropertiesForKeys: nil,
            options: [.skipsHiddenFiles]
        )

        return try urls
            .filter { $0.pathExtension == "json" }
            .map { try self.jsonCodec.decode(PersistedUploadState.self, from: try Data(contentsOf: $0)) }
            .sorted { $0.updatedAt > $1.updatedAt }
            .map(\.pendingUploadInfo)
    }

    public func findByFileURL(_ fileURL: URL) throws -> PersistedUploadState? {
        try self.ensureLayout()
        let index = try self.readIndex()
        guard let logicalUploadID = index.entries[self.indexKey(for: fileURL)] else {
            return nil
        }

        guard let state = try self.loadSnapshot(logicalUploadID: logicalUploadID) else {
            try self.removeIndexEntry(fileURL: fileURL)
            return nil
        }
        return state
    }

    public func deleteLocalSnapshot(logicalUploadID: String) throws {
        try self.ensureLayout()
        let state = try self.loadSnapshot(logicalUploadID: logicalUploadID)
        try self.removeSnapshotIfPresent(logicalUploadID: logicalUploadID, in: .active)
        try self.removeSnapshotIfPresent(logicalUploadID: logicalUploadID, in: .terminal)
        try self.removeSnapshotIfPresent(logicalUploadID: logicalUploadID, in: .completed)
        if let state {
            try self.removeIndexEntry(fileURL: state.managedFileURL)
        }
    }

    public func performGarbageCollection(retentionPolicy: SnapshotRetentionPolicy) async throws {
        try self.ensureLayout()
        let now = await self.clock.now()

        try self.collect(
            namespace: .terminal,
            keepSnapshots: retentionPolicy.keepTerminalSnapshot,
            ttl: retentionPolicy.terminalSnapshotTTL,
            now: now
        )
        try self.collect(
            namespace: .completed,
            keepSnapshots: retentionPolicy.keepCompletedSnapshot,
            ttl: retentionPolicy.completedSnapshotTTL,
            now: now
        )
    }

    public func stateDirectories() -> [URL] {
        SnapshotNamespace.allCases.map { self.snapshotDirectory(for: $0) } + [self.indexDirectory()]
    }

    public func indexFileURL() -> URL {
        self.indexDirectory().appendingPathComponent("by-local-file.json")
    }

    private func ensureLayout() throws {
        for directory in self.stateDirectories() {
            try self.fileManager.createDirectory(at: directory, withIntermediateDirectories: true)
        }
        if !self.fileManager.fileExists(atPath: self.indexFileURL().path) {
            try self.atomicWrite(LocalFileIndex.empty, to: self.indexFileURL())
        }
    }

    private func writeSnapshot(_ state: PersistedUploadState, namespace: SnapshotNamespace) throws {
        try self.atomicWrite(state, to: self.snapshotURL(logicalUploadID: state.logicalUploadID, namespace: namespace))
    }

    private func loadSnapshot(
        logicalUploadID: String,
        namespace: SnapshotNamespace
    ) throws -> PersistedUploadState? {
        let url = self.snapshotURL(logicalUploadID: logicalUploadID, namespace: namespace)
        guard self.fileManager.fileExists(atPath: url.path) else {
            return nil
        }
        return try self.jsonCodec.decode(PersistedUploadState.self, from: Data(contentsOf: url))
    }

    private func upsertIndexEntry(for state: PersistedUploadState) throws {
        var index = try self.readIndex()
        index.entries[self.indexKey(for: state.managedFileURL)] = state.logicalUploadID
        try self.atomicWrite(index, to: self.indexFileURL())
    }

    private func removeIndexEntry(fileURL: URL) throws {
        var index = try self.readIndex()
        index.entries.removeValue(forKey: self.indexKey(for: fileURL))
        try self.atomicWrite(index, to: self.indexFileURL())
    }

    private func readIndex() throws -> LocalFileIndex {
        let url = self.indexFileURL()
        guard self.fileManager.fileExists(atPath: url.path) else {
            return .empty
        }
        return try self.jsonCodec.decode(LocalFileIndex.self, from: Data(contentsOf: url))
    }

    private func removeSnapshotIfPresent(logicalUploadID: String, in namespace: SnapshotNamespace) throws {
        let url = self.snapshotURL(logicalUploadID: logicalUploadID, namespace: namespace)
        if self.fileManager.fileExists(atPath: url.path) {
            try self.fileManager.removeItem(at: url)
        }
    }

    private func collect(
        namespace: SnapshotNamespace,
        keepSnapshots: Bool,
        ttl: Duration,
        now: Date
    ) throws {
        let directory = self.snapshotDirectory(for: namespace)
        let snapshotURLs = try self.fileManager.contentsOfDirectory(
            at: directory,
            includingPropertiesForKeys: nil,
            options: [.skipsHiddenFiles]
        )

        for snapshotURL in snapshotURLs where snapshotURL.pathExtension == "json" {
            let state = try self.jsonCodec.decode(PersistedUploadState.self, from: Data(contentsOf: snapshotURL))
            let expired = state.updatedAt.addingTimeInterval(ttl.timeInterval) <= now
            if !keepSnapshots || expired {
                try self.fileManager.removeItem(at: snapshotURL)
            }
        }
    }

    private func atomicWrite<T: Encodable>(_ value: T, to destination: URL) throws {
        let data = try self.jsonCodec.encode(value)
        let directory = destination.deletingLastPathComponent()
        try self.fileManager.createDirectory(at: directory, withIntermediateDirectories: true)

        let temporaryURL = directory.appendingPathComponent("\(destination.lastPathComponent).tmp-\(UUID().uuidString)")
        #if os(iOS) || os(tvOS) || os(watchOS) || os(visionOS)
        let writeOptions: Data.WritingOptions = .completeFileProtectionUnlessOpen
        #else
        let writeOptions: Data.WritingOptions = []
        #endif
        try data.write(to: temporaryURL, options: writeOptions)

        try self.replaceOrMoveTemporaryItem(temporaryURL, to: destination)
    }

    private func replaceOrMoveTemporaryItem(_ temporaryURL: URL, to destination: URL) throws {
        if self.fileManager.fileExists(atPath: destination.path) {
            _ = try self.fileManager.replaceItemAt(destination, withItemAt: temporaryURL)
            return
        }

        do {
            try self.fileManager.moveItem(at: temporaryURL, to: destination)
        } catch {
            if self.fileManager.fileExists(atPath: destination.path) {
                _ = try self.fileManager.replaceItemAt(destination, withItemAt: temporaryURL)
                return
            }
            throw error
        }
    }

    private func snapshotURL(logicalUploadID: String, namespace: SnapshotNamespace) -> URL {
        self.snapshotDirectory(for: namespace).appendingPathComponent("\(logicalUploadID).json")
    }

    private func snapshotDirectory(for namespace: SnapshotNamespace) -> URL {
        self.uploadsDirectory().appendingPathComponent(namespace.rawValue, isDirectory: true)
    }

    private func uploadsDirectory() -> URL {
        self.storeRoot().appendingPathComponent("uploads", isDirectory: true)
    }

    private func indexDirectory() -> URL {
        self.storeRoot().appendingPathComponent("index", isDirectory: true)
    }

    private func storeRoot() -> URL {
        self.persistRoot.appendingPathComponent("data-gateway-client", isDirectory: true)
    }

    private func indexKey(for fileURL: URL) -> String {
        fileURL.standardizedFileURL.path
    }
}

private extension Duration {
    var timeInterval: TimeInterval {
        let components = self.components
        return Double(components.seconds) + Double(components.attoseconds) / 1_000_000_000_000_000_000
    }
}
