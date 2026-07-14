import DGWControlPlane
import Foundation
import Testing

@testable import DGWStore

@Suite(.serialized) struct UploadStateStoreTests {
    @Test func activeSnapshotWriteOverwriteAndLookupByLogicalUploadID() async throws {
        let sandbox = try makeSandbox()
        let store = UploadStateStore(persistRoot: sandbox.root)
        var state = sandbox.makeState(logicalUploadID: "logical-1", uploadID: "upload-1", phase: .sessionCreated)
        try await store.saveActive(state)

        state.phase = .uploading
        state.updatedAt = Date(timeIntervalSince1970: 3_000)
        try await store.saveActive(state)

        let loaded = try await store.loadActiveSnapshot(logicalUploadID: "logical-1")
        #expect(loaded == state)
        #expect(try Data(contentsOf: activeURL(root: sandbox.root, logicalUploadID: "logical-1")).count > 0)
    }

    @Test func snapshotRoundTripPreservesSubsecondDatesForFingerprintValidation() async throws {
        let sandbox = try makeSandbox()
        let store = UploadStateStore(persistRoot: sandbox.root)
        var state = sandbox.makeState(logicalUploadID: "logical-subsecond", uploadID: "upload-subsecond", phase: .sessionCreated)
        state.fileFingerprint.modifiedAt = Date(timeIntervalSince1970: 1_500.123456)
        state.lastKnownSTSExpireAt = Date(timeIntervalSince1970: 2_200.654321)
        state.createdAt = Date(timeIntervalSince1970: 1_000.111222)
        state.updatedAt = Date(timeIntervalSince1970: 2_000.333444)

        try await store.saveActive(state)

        let loaded = try await store.loadActiveSnapshot(logicalUploadID: "logical-subsecond")
        #expect(loaded == state)
        #expect(loaded?.fileFingerprint.modifiedAt == state.fileFingerprint.modifiedAt)
    }

    @Test func snapshotWritesUseAtomicReplacementWithoutLeavingTempFiles() async throws {
        let sandbox = try makeSandbox()
        let store = UploadStateStore(persistRoot: sandbox.root)
        var state = sandbox.makeState(logicalUploadID: "logical-atomic", uploadID: "upload-atomic", phase: .sessionCreated)
        try await store.saveActive(state)

        state.phase = .uploading
        try await store.saveActive(state)

        let files = try FileManager.default.contentsOfDirectory(
            at: activeURL(root: sandbox.root, logicalUploadID: "logical-atomic").deletingLastPathComponent(),
            includingPropertiesForKeys: nil,
            options: [.skipsHiddenFiles]
        )

        #expect(files.allSatisfy { !$0.lastPathComponent.contains(".tmp-") })
    }

    @Test func activeSnapshotCanBeFoundByFileURLIndex() async throws {
        let sandbox = try makeSandbox()
        let store = UploadStateStore(persistRoot: sandbox.root)
        let state = sandbox.makeState(logicalUploadID: "logical-2", uploadID: "upload-2", phase: .multipartInitiated)

        try await store.saveActive(state)

        let found = try await store.findByFileURL(state.managedFileURL)
        #expect(found == state)
    }

    @Test func moveToTerminalRemovesActiveSnapshotAndRetainsIndex() async throws {
        let sandbox = try makeSandbox()
        let store = UploadStateStore(persistRoot: sandbox.root)
        let state = sandbox.makeState(logicalUploadID: "logical-3", uploadID: "upload-3", phase: .terminalFailed)

        try await store.saveActive(state)
        try await store.moveToTerminal(state)

        #expect(try await store.loadActiveSnapshot(logicalUploadID: "logical-3") == nil)
        #expect(try Data(contentsOf: terminalURL(root: sandbox.root, logicalUploadID: "logical-3")).count > 0)
        #expect(try await store.findByFileURL(state.managedFileURL) == state)
    }

    @Test func moveToCompletedRemovesIndexAndPendingVisibility() async throws {
        let sandbox = try makeSandbox()
        let store = UploadStateStore(persistRoot: sandbox.root)
        let state = sandbox.makeState(logicalUploadID: "logical-4", uploadID: "upload-4", phase: .multipartCompleted)

        try await store.saveActive(state)
        try await store.moveToCompleted(state)

        #expect(try await store.findByFileURL(state.managedFileURL) == nil)
        #expect(try await store.listPendingUploads().isEmpty)
        #expect(try Data(contentsOf: completedURL(root: sandbox.root, logicalUploadID: "logical-4")).count > 0)
    }

    @Test func deleteLocalSnapshotRemovesSnapshotsAndIndex() async throws {
        let sandbox = try makeSandbox()
        let store = UploadStateStore(persistRoot: sandbox.root)
        let state = sandbox.makeState(logicalUploadID: "logical-5", uploadID: "upload-5", phase: .uploading)

        try await store.saveActive(state)
        try await store.deleteLocalSnapshot(logicalUploadID: "logical-5")

        #expect(try await store.loadSnapshot(logicalUploadID: "logical-5") == nil)
        #expect(try await store.findByFileURL(state.managedFileURL) == nil)
    }

    @Test func staleIndexEntryIsCleanedWhenActiveSnapshotMissing() async throws {
        let sandbox = try makeSandbox()
        let store = UploadStateStore(persistRoot: sandbox.root)
        let state = sandbox.makeState(logicalUploadID: "logical-6", uploadID: "upload-6", phase: .uploading)

        try await store.saveActive(state)
        try FileManager.default.removeItem(at: activeURL(root: sandbox.root, logicalUploadID: "logical-6"))

        let found = try await store.findByFileURL(state.managedFileURL)
        #expect(found == nil)

        let indexContents = try String(contentsOf: await store.indexFileURL(), encoding: .utf8)
        #expect(!indexContents.contains("logical-6"))
    }

    @Test func ttlExpiredSnapshotsAreGarbageCollected() async throws {
        let sandbox = try makeSandbox()
        let clock = FixedUploadStateStoreClock(now: Date(timeIntervalSince1970: 10_000))
        let store = UploadStateStore(persistRoot: sandbox.root, fileManager: .default, clock: clock)

        var terminal = sandbox.makeState(logicalUploadID: "logical-7", uploadID: "upload-7", phase: .terminalFailed)
        terminal.updatedAt = Date(timeIntervalSince1970: 9_000)
        try await store.moveToTerminal(terminal)

        var completed = sandbox.makeState(logicalUploadID: "logical-8", uploadID: "upload-8", phase: .multipartCompleted)
        completed.updatedAt = Date(timeIntervalSince1970: 9_500)
        try await store.moveToCompleted(completed)

        try await store.performGarbageCollection(
            retentionPolicy: SnapshotRetentionPolicy(
                keepTerminalSnapshot: true,
                keepCompletedSnapshot: true,
                completedSnapshotTTL: .seconds(100),
                terminalSnapshotTTL: .seconds(500)
            )
        )

        #expect(!FileManager.default.fileExists(atPath: terminalURL(root: sandbox.root, logicalUploadID: "logical-7").path))
        #expect(!FileManager.default.fileExists(atPath: completedURL(root: sandbox.root, logicalUploadID: "logical-8").path))
    }

    @Test func keepFlagsPreventRemovalBeforeTTLButDisableRetentionWhenFalse() async throws {
        let sandbox = try makeSandbox()
        let clock = FixedUploadStateStoreClock(now: Date(timeIntervalSince1970: 10_000))
        let store = UploadStateStore(persistRoot: sandbox.root, fileManager: .default, clock: clock)

        var terminal = sandbox.makeState(logicalUploadID: "logical-9", uploadID: "upload-9", phase: .terminalFailed)
        terminal.updatedAt = Date(timeIntervalSince1970: 9_990)
        try await store.moveToTerminal(terminal)

        var completed = sandbox.makeState(logicalUploadID: "logical-10", uploadID: "upload-10", phase: .multipartCompleted)
        completed.updatedAt = Date(timeIntervalSince1970: 9_990)
        try await store.moveToCompleted(completed)

        try await store.performGarbageCollection(
            retentionPolicy: SnapshotRetentionPolicy(
                keepTerminalSnapshot: true,
                keepCompletedSnapshot: false,
                completedSnapshotTTL: .seconds(3600),
                terminalSnapshotTTL: .seconds(3600)
            )
        )

        #expect(FileManager.default.fileExists(atPath: terminalURL(root: sandbox.root, logicalUploadID: "logical-9").path))
        #expect(!FileManager.default.fileExists(atPath: completedURL(root: sandbox.root, logicalUploadID: "logical-10").path))
    }
}

private struct StoreSandbox {
    let root: URL

    func makeState(
        logicalUploadID: String,
        uploadID: String,
        phase: PersistedUploadPhase
    ) -> PersistedUploadState {
        PersistedUploadState(
            version: 1,
            logicalUploadID: logicalUploadID,
            uploadID: uploadID,
            restartCount: 1,
            multipartUploadID: "multipart-\(logicalUploadID)",
            bucket: "bucket-1",
            endpoint: "https://oss-cn-shanghai.aliyuncs.com",
            objectKey: "objects/\(logicalUploadID).bin",
            fileURLBookmarkData: nil,
            managedFileURL: self.root.appendingPathComponent("managed/\(logicalUploadID).bin"),
            fileSize: 1_024,
            fileFingerprint: LocalFileFingerprint(
                size: 1_024,
                modifiedAt: Date(timeIntervalSince1970: 1_500),
                firstChunkMD5Hex: "ABCDEF0123456789ABCDEF0123456789"
            ),
            partSizeBytes: 64 * 1024 * 1024,
            uploadedParts: [
                PersistedUploadedPart(
                    partNumber: 1,
                    etag: "\"etag-1\"",
                    offsetStart: 0,
                    partSize: 1_024,
                    md5Hex: "00112233445566778899AABBCCDDEEFF"
                ),
            ],
            clientHints: ["device": "iphone"],
            rawTags: ["scene": "robot"],
            phase: phase,
            lastKnownSTSExpireAt: Date(timeIntervalSince1970: 2_200),
            createdAt: Date(timeIntervalSince1970: 1_000),
            updatedAt: Date(timeIntervalSince1970: 2_000)
        )
    }
}

private func makeSandbox() throws -> StoreSandbox {
    let root = FileManager.default.temporaryDirectory.appendingPathComponent("dgw-store-tests-\(UUID().uuidString)", isDirectory: true)
    try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
    return StoreSandbox(root: root)
}

private struct FixedUploadStateStoreClock: UploadStateStoreClock {
    let nowValue: Date

    init(now: Date) {
        self.nowValue = now
    }

    func now() async -> Date {
        self.nowValue
    }
}

private func activeURL(root: URL, logicalUploadID: String) -> URL {
    root
        .appendingPathComponent("data-gateway-client/uploads/active", isDirectory: true)
        .appendingPathComponent("\(logicalUploadID).json")
}

private func terminalURL(root: URL, logicalUploadID: String) -> URL {
    root
        .appendingPathComponent("data-gateway-client/uploads/terminal", isDirectory: true)
        .appendingPathComponent("\(logicalUploadID).json")
}

private func completedURL(root: URL, logicalUploadID: String) -> URL {
    root
        .appendingPathComponent("data-gateway-client/uploads/completed", isDirectory: true)
        .appendingPathComponent("\(logicalUploadID).json")
}
