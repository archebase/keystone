import Foundation

/// Stable module marker for DGWStore.
public enum DGWStoreModule {
    public static let name = "DGWStore"
}

/// Fingerprint captured from the local file for resume validation.
public struct LocalFileFingerprint: Codable, Sendable, Equatable {
    public var size: UInt64
    public var modifiedAt: Date?
    public var firstChunkMD5Hex: String

    public init(size: UInt64, modifiedAt: Date?, firstChunkMD5Hex: String) {
        self.size = size
        self.modifiedAt = modifiedAt
        self.firstChunkMD5Hex = firstChunkMD5Hex
    }
}

/// One uploaded multipart part persisted for resume and reconciliation.
public struct PersistedUploadedPart: Codable, Sendable, Equatable {
    public var partNumber: Int
    public var etag: String
    public var offsetStart: UInt64
    public var partSize: UInt64
    public var md5Hex: String

    public init(
        partNumber: Int,
        etag: String,
        offsetStart: UInt64,
        partSize: UInt64,
        md5Hex: String
    ) {
        self.partNumber = partNumber
        self.etag = etag
        self.offsetStart = offsetStart
        self.partSize = partSize
        self.md5Hex = md5Hex
    }
}

/// Persisted upload phase used by local state snapshots.
public enum PersistedUploadPhase: String, Codable, Sendable, Equatable {
    case sessionCreated
    case multipartInitiated
    case uploading
    case multipartCompleted
    case businessCompleting
    case terminalFailed
}

/// User-visible pending upload summary derived from persisted state.
public struct PendingUploadInfo: Sendable, Equatable {
    public var logicalUploadID: String
    public var uploadID: String
    public var fileURL: URL
    public var fileSize: UInt64
    public var phase: PersistedUploadPhase
    public var restartCount: Int
    public var updatedAt: Date

    public init(
        logicalUploadID: String,
        uploadID: String,
        fileURL: URL,
        fileSize: UInt64,
        phase: PersistedUploadPhase,
        restartCount: Int,
        updatedAt: Date
    ) {
        self.logicalUploadID = logicalUploadID
        self.uploadID = uploadID
        self.fileURL = fileURL
        self.fileSize = fileSize
        self.phase = phase
        self.restartCount = restartCount
        self.updatedAt = updatedAt
    }
}

/// Full persisted upload snapshot used for resume, reconciliation, and cleanup.
public struct PersistedUploadState: Codable, Sendable, Equatable {
    public var version: Int
    public var logicalUploadID: String
    public var uploadID: String
    public var restartCount: Int
    public var multipartUploadID: String?
    public var bucket: String
    public var endpoint: String
    public var objectKey: String
    public var fileURLBookmarkData: Data?
    public var managedFileURL: URL
    public var fileSize: UInt64
    public var fileFingerprint: LocalFileFingerprint
    public var partSizeBytes: UInt64
    public var uploadedParts: [PersistedUploadedPart]
    public var clientHints: [String: String]
    public var rawTags: [String: String]
    public var phase: PersistedUploadPhase
    public var lastKnownSTSExpireAt: Date?
    public var createdAt: Date
    public var updatedAt: Date

    public init(
        version: Int,
        logicalUploadID: String,
        uploadID: String,
        restartCount: Int,
        multipartUploadID: String?,
        bucket: String,
        endpoint: String,
        objectKey: String,
        fileURLBookmarkData: Data?,
        managedFileURL: URL,
        fileSize: UInt64,
        fileFingerprint: LocalFileFingerprint,
        partSizeBytes: UInt64,
        uploadedParts: [PersistedUploadedPart],
        clientHints: [String: String],
        rawTags: [String: String],
        phase: PersistedUploadPhase,
        lastKnownSTSExpireAt: Date?,
        createdAt: Date,
        updatedAt: Date
    ) {
        self.version = version
        self.logicalUploadID = logicalUploadID
        self.uploadID = uploadID
        self.restartCount = restartCount
        self.multipartUploadID = multipartUploadID
        self.bucket = bucket
        self.endpoint = endpoint
        self.objectKey = objectKey
        self.fileURLBookmarkData = fileURLBookmarkData
        self.managedFileURL = managedFileURL
        self.fileSize = fileSize
        self.fileFingerprint = fileFingerprint
        self.partSizeBytes = partSizeBytes
        self.uploadedParts = uploadedParts
        self.clientHints = clientHints
        self.rawTags = rawTags
        self.phase = phase
        self.lastKnownSTSExpireAt = lastKnownSTSExpireAt
        self.createdAt = createdAt
        self.updatedAt = updatedAt
    }

    public var pendingUploadInfo: PendingUploadInfo {
        PendingUploadInfo(
            logicalUploadID: self.logicalUploadID,
            uploadID: self.uploadID,
            fileURL: self.managedFileURL,
            fileSize: self.fileSize,
            phase: self.phase,
            restartCount: self.restartCount,
            updatedAt: self.updatedAt
        )
    }
}
