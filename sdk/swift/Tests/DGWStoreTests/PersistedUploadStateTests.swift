import Foundation
import Testing

@testable import DGWStore

@Test func dgwStoreModuleNameIsStable() {
    #expect(DGWStoreModule.name == "DGWStore")
}

@Test func persistedUploadStateRoundTripsThroughCodable() throws {
    let state = makePersistedUploadState()
    let encoder = JSONEncoder()
    encoder.dateEncodingStrategy = .iso8601
    encoder.outputFormatting = [.sortedKeys]

    let data = try encoder.encode(state)

    let decoder = JSONDecoder()
    decoder.dateDecodingStrategy = .iso8601
    let decoded = try decoder.decode(PersistedUploadState.self, from: data)

    #expect(decoded == state)
}

@Test func persistedUploadPhaseAndOptionalFieldsRoundTrip() throws {
    let phases: [PersistedUploadPhase] = [
        .sessionCreated,
        .multipartInitiated,
        .uploading,
        .multipartCompleted,
        .businessCompleting,
        .terminalFailed,
    ]
    let encoder = JSONEncoder()
    encoder.dateEncodingStrategy = .iso8601
    let decoder = JSONDecoder()
    decoder.dateDecodingStrategy = .iso8601

    for (index, phase) in phases.enumerated() {
        var state = makePersistedUploadState()
        state.phase = phase
        state.multipartUploadID = index.isMultiple(of: 2) ? nil : "multipart-1"
        state.fileURLBookmarkData = index.isMultiple(of: 2) ? nil : Data([0x01, 0x02, 0x03])
        state.lastKnownSTSExpireAt = index.isMultiple(of: 2) ? nil : Date(timeIntervalSince1970: 2_200)

        let data = try encoder.encode(state)
        let decoded = try decoder.decode(PersistedUploadState.self, from: data)
        #expect(decoded.phase == phase)
        #expect(decoded.multipartUploadID == state.multipartUploadID)
        #expect(decoded.fileURLBookmarkData == state.fileURLBookmarkData)
        #expect(decoded.lastKnownSTSExpireAt == state.lastKnownSTSExpireAt)
    }
}

@Test func pendingUploadInfoProjectsUserVisibleFields() {
    let state = makePersistedUploadState()

    #expect(
        state.pendingUploadInfo == PendingUploadInfo(
            logicalUploadID: "logical-1",
            uploadID: "upload-1",
            fileURL: URL(fileURLWithPath: "/tmp/managed/demo.bin"),
            fileSize: 123_456,
            phase: .uploading,
            restartCount: 2,
            updatedAt: Date(timeIntervalSince1970: 2_000)
        )
    )
}

private func makePersistedUploadState() -> PersistedUploadState {
    PersistedUploadState(
        version: 1,
        logicalUploadID: "logical-1",
        uploadID: "upload-1",
        restartCount: 2,
        multipartUploadID: "multipart-1",
        bucket: "bucket-1",
        endpoint: "https://oss-cn-shanghai.aliyuncs.com",
        objectKey: "objects/demo.bin",
        fileURLBookmarkData: Data([0xCA, 0xFE]),
        managedFileURL: URL(fileURLWithPath: "/tmp/managed/demo.bin"),
        fileSize: 123_456,
        fileFingerprint: LocalFileFingerprint(
            size: 123_456,
            modifiedAt: Date(timeIntervalSince1970: 1_500),
            firstChunkMD5Hex: "ABCDEF0123456789ABCDEF0123456789"
        ),
        partSizeBytes: 64 * 1024 * 1024,
        uploadedParts: [
            PersistedUploadedPart(
                partNumber: 1,
                etag: "\"etag-1\"",
                offsetStart: 0,
                partSize: 64,
                md5Hex: "00112233445566778899AABBCCDDEEFF"
            ),
            PersistedUploadedPart(
                partNumber: 2,
                etag: "\"etag-2\"",
                offsetStart: 64,
                partSize: 32,
                md5Hex: "FFEEDDCCBBAA99887766554433221100"
            ),
        ],
        clientHints: ["device": "iphone"],
        rawTags: ["scene": "robot"],
        phase: .uploading,
        lastKnownSTSExpireAt: Date(timeIntervalSince1970: 2_200),
        createdAt: Date(timeIntervalSince1970: 1_000),
        updatedAt: Date(timeIntervalSince1970: 2_000)
    )
}
