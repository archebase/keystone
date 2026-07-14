@preconcurrency import AlibabaCloudOSS
import DGWOss
import DGWControlPlane
import DGWProto
import DGWStore
import Foundation
import GRPCCore
import Testing

@testable import DataGatewayClient

@Test func publicRecommendedDefaultsMatchDesignContract() {
    let persistence = LocalPersistencePolicy.recommended
    #expect(persistence.keepTerminalSnapshot)
    #expect(!persistence.keepCompletedSnapshot)
    #expect(persistence.completedSnapshotTTL == .seconds(0))
    #expect(persistence.terminalSnapshotTTL == .seconds(3600))
    #expect(!persistence.copyExternalFileIntoManagedStaging)

    let execution = UploadExecutionPolicy.recommended
    #expect(execution.maxRestartCount == 3)
    #expect(execution.autoResumeByFileURL)
    #expect(execution.reconcileRemotePartsOnResume)
    #expect(execution.cleanupOnTerminalFailure)
    #expect(execution.credentialRefreshSkew == .seconds(30))
    #expect(execution.persistence == .recommended)

    let retry = RetryPolicySet.recommended
    #expect(retry.controlPlane.maxAttempts == 5)
    #expect(retry.controlPlane.initialBackoff == Duration(secondsComponent: 0, attosecondsComponent: 500_000_000_000_000_000))
    #expect(retry.controlPlane.maxBackoff == .seconds(8))
    #expect(retry.dataPlane.maxAttempts == 8)
    #expect(retry.dataPlane.initialBackoff == .seconds(1))
    #expect(retry.dataPlane.maxBackoff == .seconds(30))
}

@Test func configValidationRejectsInvalidTlsAndEmptyCredential() {
    let persistRoot = URL(fileURLWithPath: "/tmp/data-gateway-config")

    let plaintextMismatch = DataGatewayClientConfig(
        authEndpoint: URL(string: "https://127.0.0.1:15055")!,
        gatewayEndpoint: URL(string: "http://127.0.0.1:15053")!,
        credentialBase64: "credential",
        authRefreshBefore: .seconds(60),
        requestTimeout: .seconds(10),
        persistRootURL: persistRoot,
        retryPolicy: .recommended,
        execution: .recommended,
        tls: .plaintext
    )
    let tlsMismatch = DataGatewayClientConfig(
        authEndpoint: URL(string: "http://127.0.0.1:15055")!,
        gatewayEndpoint: URL(string: "https://127.0.0.1:15053")!,
        credentialBase64: "credential",
        authRefreshBefore: .seconds(60),
        requestTimeout: .seconds(10),
        persistRootURL: persistRoot,
        retryPolicy: .recommended,
        execution: .recommended,
        tls: .tls
    )
    let emptyCredential = DataGatewayClientConfig.testRecommended(
        authEndpoint: URL(string: "http://127.0.0.1:15055")!,
        gatewayEndpoint: URL(string: "http://127.0.0.1:15053")!,
        credentialBase64: "   ",
        persistRootURL: persistRoot
    )

    #expect(throws: DataGatewayClientError.self) { try plaintextMismatch.validate() }
    #expect(throws: DataGatewayClientError.self) { try tlsMismatch.validate() }
    #expect(throws: DataGatewayClientError.self) { try emptyCredential.validate() }
}

@Test func observabilityRedactsSensitiveLogContent() async throws {
    let sourceURL = URL(fileURLWithPath: "/files/demo-observability.bin")
    let payload = Data("hello-observability".utf8)
    let fileSystem = MemoryFileSystem(files: [
        sourceURL: .file(size: UInt64(payload.count), modifiedAt: Date(timeIntervalSince1970: 100), data: payload),
    ])
    let fileCoordinator = FileStagingCoordinator(
        stagingRoot: URL(fileURLWithPath: "/staging"),
        fileSystem: fileSystem,
        securityScopedAccessor: PassthroughSecurityScopedAccessor()
    )
    let stateStore = UploadStateStore(
        persistRoot: FileManager.default.temporaryDirectory.appendingPathComponent("upload-observability-tests-\(UUID().uuidString)"),
        fileManager: .default,
        clock: FixedUploadCoordinatorStoreClock(now: Date(timeIntervalSince1970: 500))
    )
    let gatewayClient = MockUploadCoordinatorGatewayClient(
        createResponse: makeCreateLogicalUploadResponse(),
        recoveryResponse: makeContinueRecoveryResponse(),
        reissueResponse: makeReissueResponse(uploadID: "upload-1", credentials: makeCoordinatorUploadCredentials(expireAtUnix: 5_000, tokenSuffix: "fresh")),
        completeResponse: Archebase_DataGateway_V1_CompleteUploadResponse()
    )
    let ossSession = RefreshAwareMockOssSession(
        multipartUploadID: "multipart-1",
        uploadDescriptors: [
            UploadedPartDescriptor(partNumber: 1, etag: "\"etag-part-1\"", size: Int64(payload.count), lastModified: nil, hashCRC64: nil),
        ],
        completedETag: "\"etag-object\"",
        refreshResults: [true, false],
        expirations: [Date(timeIntervalSince1970: 1_000)]
    )
    let logRecorder = LogEventRecorder()
    let metricRecorder = MetricEventRecorder()
    let dependencies = UploadCoordinatorDependencies(
        gatewayClient: gatewayClient,
        stateStore: stateStore,
        fileCoordinator: fileCoordinator,
        ossClientFactory: { _ in ossSession },
        clock: FixedUploadCoordinatorClock(now: Date(timeIntervalSince1970: 500)),
        observability: DataGatewayClientObservability(
            onLog: { event in await logRecorder.record(event) },
            onMetric: { name, dimensions in await metricRecorder.record(name: name, dimensions: dimensions) }
        )
    )

    let coordinator = UploadCoordinator(
        executionPolicy: makeExecutionPolicy(),
        dependencies: dependencies
    )

    _ = try await coordinator.upload(
        UploadRequest(fileURL: sourceURL, clientHints: ["device": "iphone"], rawTags: ["scene": "robot"], displayName: "demo")
    )

    let logs = await logRecorder.events()
    let containsRedactedCredentialLog = logs.contains {
        $0.operation == "refresh_credentials" && $0.message == "[REDACTED]"
    }
    #expect(containsRedactedCredentialLog)

    let metrics = await metricRecorder.events()
    let recordedUploadPartMetric = metrics.contains { event in
        event.name == "upload_part" && event.dimensions["upload_id"] == "upload-1"
    }
    let recordedCredentialRefreshMetric = metrics.contains { event in
        event.name == "credentials_refresh" && event.dimensions["upload_id"] == "upload-1"
    }
    #expect(recordedUploadPartMetric)
    #expect(recordedCredentialRefreshMetric)
}

@Test func contractPartSizeBytesControlsChunkSplitting() async throws {
    let sourceURL = URL(fileURLWithPath: "/files/contract-part-size.bin")
    let payload = Data(repeating: 0x31, count: 18)
    let fileSystem = MemoryFileSystem(files: [
        sourceURL: .file(size: UInt64(payload.count), modifiedAt: Date(timeIntervalSince1970: 100), data: payload),
    ])
    let fileCoordinator = FileStagingCoordinator(
        stagingRoot: URL(fileURLWithPath: "/staging"),
        fileSystem: fileSystem,
        securityScopedAccessor: PassthroughSecurityScopedAccessor()
    )
    let stateStore = UploadStateStore(
        persistRoot: FileManager.default.temporaryDirectory.appendingPathComponent("contract-part-size-\(UUID().uuidString)"),
        fileManager: .default,
        clock: FixedUploadCoordinatorStoreClock(now: Date(timeIntervalSince1970: 4_000))
    )
    let gatewayClient = MockUploadCoordinatorGatewayClient(
        createResponse: makeCreateLogicalUploadResponse(uploadID: "upload-contract-part-size", objectKey: "opaque://prefix/demo?x=1", partSizeBytes: 5),
        recoveryResponse: makeContinueRecoveryResponse(currentUploadID: "upload-contract-part-size"),
        reissueResponse: makeReissueResponse(uploadID: "upload-contract-part-size", credentials: makeCoordinatorUploadCredentials(expireAtUnix: 9_000, tokenSuffix: "contract-part-size", objectKey: "opaque://prefix/demo?x=1", partSizeBytes: 5)),
        completeResponse: Archebase_DataGateway_V1_CompleteUploadResponse()
    )
    let ossSession = MockOssUploadSession(
        multipartUploadID: "multipart-contract-part-size",
        uploadedParts: [
            UploadedPartDescriptor(partNumber: 1, etag: "\"etag-1\"", size: 5, lastModified: nil, hashCRC64: nil),
            UploadedPartDescriptor(partNumber: 2, etag: "\"etag-2\"", size: 5, lastModified: nil, hashCRC64: nil),
            UploadedPartDescriptor(partNumber: 3, etag: "\"etag-3\"", size: 5, lastModified: nil, hashCRC64: nil),
            UploadedPartDescriptor(partNumber: 4, etag: "\"etag-4\"", size: 3, lastModified: nil, hashCRC64: nil),
        ],
        completedETag: "\"etag-object\""
    )
    let client = DataGatewayClient(
        uploadCoordinator: UploadCoordinator(
            executionPolicy: makeExecutionPolicy(),
            dependencies: UploadCoordinatorDependencies(
                gatewayClient: gatewayClient,
                stateStore: stateStore,
                fileCoordinator: fileCoordinator,
                ossClientFactory: { _ in ossSession },
                clock: FixedUploadCoordinatorClock(now: Date(timeIntervalSince1970: 4_000))
            )
        )
    )

    let result = try await client.upload(
        UploadRequest(fileURL: sourceURL, clientHints: ["kind": "contract"], rawTags: [:], displayName: nil)
    )

    #expect(result.objectKey == "opaque://prefix/demo?x=1")
    #expect(await ossSession.uploadCalls() == [
        UploadCall(multipartUploadID: "multipart-contract-part-size", partNumber: 1, size: 5),
        UploadCall(multipartUploadID: "multipart-contract-part-size", partNumber: 2, size: 5),
        UploadCall(multipartUploadID: "multipart-contract-part-size", partNumber: 3, size: 5),
        UploadCall(multipartUploadID: "multipart-contract-part-size", partNumber: 4, size: 3),
    ])
}

@Test func contractIdentifiersRemainSeparatedAcrossLogicalUploadAndMultipartUpload() async throws {
    let sourceURL = URL(fileURLWithPath: "/files/contract-identifiers.bin")
    let payload = Data(repeating: 0x32, count: 10)
    let fileSystem = MemoryFileSystem(files: [
        sourceURL: .file(size: UInt64(payload.count), modifiedAt: Date(timeIntervalSince1970: 110), data: payload),
    ])
    let fileCoordinator = FileStagingCoordinator(
        stagingRoot: URL(fileURLWithPath: "/staging"),
        fileSystem: fileSystem,
        securityScopedAccessor: PassthroughSecurityScopedAccessor()
    )
    let stateStore = UploadStateStore(
        persistRoot: FileManager.default.temporaryDirectory.appendingPathComponent("contract-identifiers-\(UUID().uuidString)"),
        fileManager: .default,
        clock: FixedUploadCoordinatorStoreClock(now: Date(timeIntervalSince1970: 4_100))
    )
    let gatewayClient = MockUploadCoordinatorGatewayClient(
        createResponse: makeCreateLogicalUploadResponse(logicalUploadID: "logical-contract", uploadID: "upload-contract", objectKey: "objects/contract.bin", partSizeBytes: 6),
        recoveryResponse: makeContinueRecoveryResponse(currentUploadID: "upload-contract"),
        reissueResponse: makeReissueResponse(uploadID: "upload-contract", credentials: makeCoordinatorUploadCredentials(expireAtUnix: 9_000, tokenSuffix: "contract-identifiers", objectKey: "objects/contract.bin", partSizeBytes: 6)),
        completeResponse: Archebase_DataGateway_V1_CompleteUploadResponse()
    )
    let ossSession = MockOssUploadSession(
        multipartUploadID: "multipart-contract",
        uploadedParts: [
            UploadedPartDescriptor(partNumber: 1, etag: "\"etag-1\"", size: 6, lastModified: nil, hashCRC64: nil),
            UploadedPartDescriptor(partNumber: 2, etag: "\"etag-2\"", size: 4, lastModified: nil, hashCRC64: nil),
        ],
        completedETag: "\"etag-object\""
    )
    let client = DataGatewayClient(
        uploadCoordinator: UploadCoordinator(
            executionPolicy: makeExecutionPolicy(),
            dependencies: UploadCoordinatorDependencies(
                gatewayClient: gatewayClient,
                stateStore: stateStore,
                fileCoordinator: fileCoordinator,
                ossClientFactory: { _ in ossSession },
                clock: FixedUploadCoordinatorClock(now: Date(timeIntervalSince1970: 4_100))
            )
        )
    )

    let result = try await client.upload(
        UploadRequest(fileURL: sourceURL, clientHints: [:], rawTags: [:], displayName: nil)
    )
    let snapshot = try await stateStore.loadSnapshot(logicalUploadID: "logical-contract")

    #expect(result.logicalUploadID == "logical-contract")
    #expect(result.uploadID == "upload-contract")
    #expect(snapshot?.logicalUploadID == "logical-contract")
    #expect(snapshot?.uploadID == "upload-contract")
    #expect(snapshot?.multipartUploadID == "multipart-contract")
}

@Test func contractCompleteUploadCanBeCalledIdempotentlyWithSameMetadata() async throws {
    let sourceURL = URL(fileURLWithPath: "/files/contract-idempotent.bin")
    let payload = Data(repeating: 0x33, count: 12)
    let fileSystem = MemoryFileSystem(files: [
        sourceURL: .file(size: UInt64(payload.count), modifiedAt: Date(timeIntervalSince1970: 120), data: payload),
    ])
    let fileCoordinator = FileStagingCoordinator(
        stagingRoot: URL(fileURLWithPath: "/staging"),
        fileSystem: fileSystem,
        securityScopedAccessor: PassthroughSecurityScopedAccessor()
    )
    let stateStore = UploadStateStore(
        persistRoot: FileManager.default.temporaryDirectory.appendingPathComponent("contract-idempotent-\(UUID().uuidString)"),
        fileManager: .default,
        clock: FixedUploadCoordinatorStoreClock(now: Date(timeIntervalSince1970: 4_200))
    )
    let gatewayClient = MockUploadCoordinatorGatewayClient(
        createResponse: makeCreateLogicalUploadResponse(logicalUploadID: "logical-idempotent", uploadID: "upload-idempotent", objectKey: "objects/idempotent.bin", partSizeBytes: 12),
        recoveryResponse: makeContinueRecoveryResponse(currentUploadID: "upload-idempotent"),
        reissueResponse: makeReissueResponse(uploadID: "upload-idempotent", credentials: makeCoordinatorUploadCredentials(expireAtUnix: 9_000, tokenSuffix: "contract-idempotent", objectKey: "objects/idempotent.bin", partSizeBytes: 12)),
        completeResponse: Archebase_DataGateway_V1_CompleteUploadResponse()
    )
    let ossSession = MockOssUploadSession(
        multipartUploadID: "multipart-idempotent",
        uploadedParts: [
            UploadedPartDescriptor(partNumber: 1, etag: "\"etag-1\"", size: 12, lastModified: nil, hashCRC64: nil),
        ],
        completedETag: "\"etag-object\""
    )
    let client = DataGatewayClient(
        uploadCoordinator: UploadCoordinator(
            executionPolicy: makeExecutionPolicy(),
            dependencies: UploadCoordinatorDependencies(
                gatewayClient: gatewayClient,
                stateStore: stateStore,
                fileCoordinator: fileCoordinator,
                ossClientFactory: { _ in ossSession },
                clock: FixedUploadCoordinatorClock(now: Date(timeIntervalSince1970: 4_200))
            )
        )
    )

    let result = try await client.upload(
        UploadRequest(fileURL: sourceURL, clientHints: [:], rawTags: ["scene": "robot"], displayName: nil)
    )

    let completedRawTags = sourceFileNameRawTags(fileName: "contract-idempotent.bin")
    _ = try await gatewayClient.completeUpload(
        uploadID: result.uploadID,
        fileSize: Int64(result.fileSize),
        rawTags: completedRawTags,
        completedPartCount: 1,
        ossObjectEtag: result.ossObjectETag,
        partSizeBytes: 12
    )

    #expect(await gatewayClient.completeInvocations() == [
        CompleteInvocation(uploadID: "upload-idempotent", fileSize: 12, rawTags: completedRawTags, completedPartCount: 1, ossObjectEtag: "\"etag-object\"", partSizeBytes: 12),
        CompleteInvocation(uploadID: "upload-idempotent", fileSize: 12, rawTags: completedRawTags, completedPartCount: 1, ossObjectEtag: "\"etag-object\"", partSizeBytes: 12),
    ])
    #expect(await ossSession.initiateCalls() == 0)
    #expect(await ossSession.putObjectCalls() == [payload.count])
    #expect(await ossSession.putObjectKinds() == [.file])
    #expect(await ossSession.uploadCalls().isEmpty)
}

@Test func contractOssObjectETagMismatchPropagatesIntegrityFailure() async {
    let sourceURL = URL(fileURLWithPath: "/files/contract-etag-mismatch.bin")
    let payload = Data(repeating: 0x34, count: 7)
    let fileSystem = MemoryFileSystem(files: [
        sourceURL: .file(size: UInt64(payload.count), modifiedAt: Date(timeIntervalSince1970: 130), data: payload),
    ])
    let fileCoordinator = FileStagingCoordinator(
        stagingRoot: URL(fileURLWithPath: "/staging"),
        fileSystem: fileSystem,
        securityScopedAccessor: PassthroughSecurityScopedAccessor()
    )
    let stateStore = UploadStateStore(
        persistRoot: FileManager.default.temporaryDirectory.appendingPathComponent("contract-etag-mismatch-\(UUID().uuidString)"),
        fileManager: .default,
        clock: FixedUploadCoordinatorStoreClock(now: Date(timeIntervalSince1970: 4_300))
    )
    let gatewayClient = MockUploadCoordinatorGatewayClient(
        createResponse: makeCreateLogicalUploadResponse(logicalUploadID: "logical-etag-mismatch", uploadID: "upload-etag-mismatch", objectKey: "objects/etag-mismatch.bin", partSizeBytes: 7),
        recoveryResponse: makeContinueRecoveryResponse(currentUploadID: "upload-etag-mismatch"),
        reissueResponse: makeReissueResponse(uploadID: "upload-etag-mismatch", credentials: makeCoordinatorUploadCredentials(expireAtUnix: 9_000, tokenSuffix: "contract-etag-mismatch", objectKey: "objects/etag-mismatch.bin", partSizeBytes: 7)),
        completeResponse: Archebase_DataGateway_V1_CompleteUploadResponse(),
        completeError: DataGatewayClientError.integrityCheckFailed("oss_object_etag mismatch")
    )
    let ossSession = MockOssUploadSession(
        multipartUploadID: "multipart-etag-mismatch",
        uploadedParts: [
            UploadedPartDescriptor(partNumber: 1, etag: "\"etag-1\"", size: 7, lastModified: nil, hashCRC64: nil),
        ],
        completedETag: "\"etag-object\""
    )
    let client = DataGatewayClient(
        uploadCoordinator: UploadCoordinator(
            executionPolicy: makeExecutionPolicy(),
            dependencies: UploadCoordinatorDependencies(
                gatewayClient: gatewayClient,
                stateStore: stateStore,
                fileCoordinator: fileCoordinator,
                ossClientFactory: { _ in ossSession },
                clock: FixedUploadCoordinatorClock(now: Date(timeIntervalSince1970: 4_300))
            )
        )
    )

    let error = await #expect(throws: DataGatewayClientError.self) {
        try await client.upload(
            UploadRequest(fileURL: sourceURL, clientHints: [:], rawTags: [:], displayName: nil)
        )
    }

    #expect(error == .integrityCheckFailed("oss_object_etag mismatch"))
}

@Test func uploadCoordinatorHappyPathProducesExpectedEventsAndResult() async throws {
    let sourceURL = URL(fileURLWithPath: "/files/demo.bin")
    let payload = Data("hello-swift-data-gateway".utf8)
    let fileSystem = MemoryFileSystem(files: [
        sourceURL: .file(size: UInt64(payload.count), modifiedAt: Date(timeIntervalSince1970: 100), data: payload),
    ])
    let fileCoordinator = FileStagingCoordinator(
        stagingRoot: URL(fileURLWithPath: "/staging"),
        fileSystem: fileSystem,
        securityScopedAccessor: PassthroughSecurityScopedAccessor()
    )
    let stateStore = UploadStateStore(
        persistRoot: FileManager.default.temporaryDirectory.appendingPathComponent("upload-coordinator-tests-\(UUID().uuidString)"),
        fileManager: .default,
        clock: FixedUploadCoordinatorStoreClock(now: Date(timeIntervalSince1970: 500))
    )
    let gatewayClient = MockUploadCoordinatorGatewayClient(
        createResponse: makeCreateLogicalUploadResponse(),
        recoveryResponse: makeContinueRecoveryResponse(),
        reissueResponse: makeReissueResponse(uploadID: "upload-1", credentials: makeCoordinatorUploadCredentials(expireAtUnix: 5_000, tokenSuffix: "fresh")),
        completeResponse: Archebase_DataGateway_V1_CompleteUploadResponse()
    )
    let ossSession = MockOssUploadSession(
        multipartUploadID: "multipart-1",
        uploadedParts: [
            UploadedPartDescriptor(
                partNumber: 1,
                etag: "\"etag-part-1\"",
                size: Int64(payload.count),
                lastModified: nil,
                hashCRC64: nil
            ),
        ],
        completedETag: "\"etag-object\""
    )
    let dependencies = UploadCoordinatorDependencies(
        gatewayClient: gatewayClient,
        stateStore: stateStore,
        fileCoordinator: fileCoordinator,
        ossClientFactory: { _ in ossSession },
        clock: FixedUploadCoordinatorClock(now: Date(timeIntervalSince1970: 500))
    )
    let coordinator = UploadCoordinator(
        executionPolicy: makeExecutionPolicy(),
        dependencies: dependencies
    )

    let eventRecorder = UploadEventRecorder()
    let result = try await coordinator.upload(
        UploadRequest(fileURL: sourceURL, clientHints: ["device": "iphone"], rawTags: ["scene": "robot"], displayName: "demo")
    ) { event in
        await eventRecorder.record(event)
    }
    let recordedEvents = await eventRecorder.events()

    #expect(result == UploadResult(
        logicalUploadID: "logical-1",
        uploadID: "upload-1",
        bucket: "bucket-1",
        objectKey: "objects/demo.bin",
        fileSize: UInt64(payload.count),
        ossObjectETag: "\"etag-object\""
    ))
    #expect(recordedEvents == [
        .preparing,
        .authenticating,
        .creatingLogicalUpload,
        .uploadingPart(partNumber: 1, sentBytes: UInt64(payload.count), totalBytes: UInt64(payload.count)),
        .completingBusinessUpload(uploadID: "upload-1"),
        .completed(result),
    ])

    #expect(await gatewayClient.createInvocations() == [["device": "iphone"]])
    #expect(await gatewayClient.completeInvocations() == [
        CompleteInvocation(
            uploadID: "upload-1",
            fileSize: Int64(payload.count),
            rawTags: ["scene": "robot"],
            completedPartCount: 1,
            ossObjectEtag: "\"etag-object\"",
            partSizeBytes: 64 * 1024 * 1024
        ),
    ])
    #expect(await ossSession.initiateCalls() == 0)
    #expect(await ossSession.uploadCalls().isEmpty)
    #expect(await ossSession.putObjectCalls() == [payload.count])
    #expect(await ossSession.putObjectKinds() == [.file])
    #expect(await ossSession.completeCalls().isEmpty)

    let pending = try await stateStore.listPendingUploads()
    #expect(pending.isEmpty)
    let completedState = try await stateStore.loadSnapshot(logicalUploadID: "logical-1")
    #expect(completedState?.phase == .businessCompleting)
    #expect(completedState?.uploadedParts.count == 1)
}

@Test func dataGatewayClientUploadReturnsExpectedResultFields() async throws {
    let sourceURL = URL(fileURLWithPath: "/files/demo-small.bin")
    let payload = Data("small-file".utf8)
    let fileSystem = MemoryFileSystem(files: [
        sourceURL: .file(size: UInt64(payload.count), modifiedAt: Date(timeIntervalSince1970: 120), data: payload),
    ])
    let fileCoordinator = FileStagingCoordinator(
        stagingRoot: URL(fileURLWithPath: "/staging"),
        fileSystem: fileSystem,
        securityScopedAccessor: PassthroughSecurityScopedAccessor()
    )
    let stateStore = UploadStateStore(
        persistRoot: FileManager.default.temporaryDirectory.appendingPathComponent("data-gateway-client-e2-small-\(UUID().uuidString)"),
        fileManager: .default,
        clock: FixedUploadCoordinatorStoreClock(now: Date(timeIntervalSince1970: 700))
    )
    let gatewayClient = MockUploadCoordinatorGatewayClient(
        createResponse: makeCreateLogicalUploadResponse(uploadID: "upload-small", objectKey: "objects/small.bin", partSizeBytes: 1024),
        recoveryResponse: makeContinueRecoveryResponse(currentUploadID: "upload-small"),
        reissueResponse: makeReissueResponse(uploadID: "upload-small", credentials: makeCoordinatorUploadCredentials(expireAtUnix: 5_000, tokenSuffix: "fresh", objectKey: "objects/small.bin", partSizeBytes: 1024)),
        completeResponse: Archebase_DataGateway_V1_CompleteUploadResponse()
    )
    let ossSession = MockOssUploadSession(
        multipartUploadID: "multipart-small",
        uploadedParts: [
            UploadedPartDescriptor(
                partNumber: 1,
                etag: "\"etag-small-part-1\"",
                size: Int64(payload.count),
                lastModified: nil,
                hashCRC64: nil
            ),
        ],
        completedETag: "\"etag-small-object\""
    )
    let client = DataGatewayClient(
        uploadCoordinator: UploadCoordinator(
            executionPolicy: makeExecutionPolicy(),
            dependencies: UploadCoordinatorDependencies(
                gatewayClient: gatewayClient,
                stateStore: stateStore,
                fileCoordinator: fileCoordinator,
                ossClientFactory: { _ in ossSession },
                clock: FixedUploadCoordinatorClock(now: Date(timeIntervalSince1970: 700))
            )
        )
    )

    let result = try await client.upload(
        UploadRequest(fileURL: sourceURL, clientHints: ["kind": "small"], rawTags: ["scene": "robot"], displayName: "small")
    )

    #expect(result.logicalUploadID == "logical-1")
    #expect(result.uploadID == "upload-small")
    #expect(result.bucket == "bucket-1")
    #expect(result.objectKey == "objects/small.bin")
    #expect(result.fileSize == UInt64(payload.count))
    #expect(result.ossObjectETag == "\"etag-small-object\"")
    #expect(await ossSession.initiateCalls() == 0)
    #expect(await ossSession.putObjectCalls() == [payload.count])
    #expect(await ossSession.putObjectKinds() == [.file])
    #expect(await ossSession.uploadCalls().isEmpty)
}

@Test func singleObjectUploadReadsOriginalFileBodyDirectly() async throws {
    let root = FileManager.default.temporaryDirectory.appendingPathComponent("single-object-direct-\(UUID().uuidString)", isDirectory: true)
    try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
    let sourceURL = root.appendingPathComponent("demo-small.bin")
    let payload = Data("small-direct-file".utf8)
    try payload.write(to: sourceURL)
    let fileCoordinator = FileStagingCoordinator(stagingRoot: root.appendingPathComponent("staging", isDirectory: true))
    let stateStore = UploadStateStore(
        persistRoot: root.appendingPathComponent("state", isDirectory: true),
        fileManager: .default,
        clock: FixedUploadCoordinatorStoreClock(now: Date(timeIntervalSince1970: 705))
    )
    let gatewayClient = MockUploadCoordinatorGatewayClient(
        createResponse: makeCreateLogicalUploadResponse(uploadID: "upload-small-direct", objectKey: "objects/small-direct.bin", partSizeBytes: 1024),
        recoveryResponse: makeContinueRecoveryResponse(currentUploadID: "upload-small-direct"),
        reissueResponse: makeReissueResponse(uploadID: "upload-small-direct", credentials: makeCoordinatorUploadCredentials(expireAtUnix: 5_000, tokenSuffix: "fresh", objectKey: "objects/small-direct.bin", partSizeBytes: 1024)),
        completeResponse: Archebase_DataGateway_V1_CompleteUploadResponse()
    )
    let ossSession = MockOssUploadSession(
        multipartUploadID: "multipart-small-direct",
        uploadedParts: [],
        completedETag: "\"etag-small-direct-object\""
    )
    let client = DataGatewayClient(
        uploadCoordinator: UploadCoordinator(
            executionPolicy: makeExecutionPolicy(),
            dependencies: UploadCoordinatorDependencies(
                gatewayClient: gatewayClient,
                stateStore: stateStore,
                fileCoordinator: fileCoordinator,
                ossClientFactory: { _ in ossSession },
                clock: FixedUploadCoordinatorClock(now: Date(timeIntervalSince1970: 705))
            )
        )
    )

    _ = try await client.upload(
        UploadRequest(fileURL: sourceURL, clientHints: ["kind": "small"], rawTags: ["scene": "robot"], displayName: "small")
    )

    #expect(await ossSession.putObjectKinds() == [.file])
    #expect(await ossSession.putObjectBytes() == [payload])
}

@Test func dataGatewayClientUploadHandlesMultipartFiles() async throws {
    let sourceURL = URL(fileURLWithPath: "/files/demo-multipart.bin")
    let payload = Data(repeating: 0x42, count: 24)
    let fileSystem = MemoryFileSystem(files: [
        sourceURL: .file(size: UInt64(payload.count), modifiedAt: Date(timeIntervalSince1970: 140), data: payload),
    ])
    let securityAccessor = RecordingSecurityScopedAccessor()
    let fileCoordinator = FileStagingCoordinator(
        stagingRoot: URL(fileURLWithPath: "/staging"),
        fileSystem: fileSystem,
        securityScopedAccessor: securityAccessor
    )
    let stateStore = UploadStateStore(
        persistRoot: FileManager.default.temporaryDirectory.appendingPathComponent("data-gateway-client-e2-multipart-\(UUID().uuidString)"),
        fileManager: .default,
        clock: FixedUploadCoordinatorStoreClock(now: Date(timeIntervalSince1970: 900))
    )
    let gatewayClient = MockUploadCoordinatorGatewayClient(
        createResponse: makeCreateLogicalUploadResponse(uploadID: "upload-multipart", objectKey: "objects/multipart.bin", partSizeBytes: 8),
        recoveryResponse: makeContinueRecoveryResponse(currentUploadID: "upload-multipart", completedPartCount: 0),
        reissueResponse: makeReissueResponse(uploadID: "upload-multipart", credentials: makeCoordinatorUploadCredentials(expireAtUnix: 5_000, tokenSuffix: "fresh", objectKey: "objects/multipart.bin", partSizeBytes: 8)),
        completeResponse: Archebase_DataGateway_V1_CompleteUploadResponse()
    )
    let ossSession = MockOssUploadSession(
        multipartUploadID: "multipart-24",
        uploadedParts: [
            UploadedPartDescriptor(partNumber: 1, etag: "\"etag-part-1\"", size: 8, lastModified: nil, hashCRC64: nil),
            UploadedPartDescriptor(partNumber: 2, etag: "\"etag-part-2\"", size: 8, lastModified: nil, hashCRC64: nil),
            UploadedPartDescriptor(partNumber: 3, etag: "\"etag-part-3\"", size: 8, lastModified: nil, hashCRC64: nil),
        ],
        completedETag: "\"etag-multipart-object\""
    )
    let client = DataGatewayClient(
        uploadCoordinator: UploadCoordinator(
            executionPolicy: makeExecutionPolicy(),
            dependencies: UploadCoordinatorDependencies(
                gatewayClient: gatewayClient,
                stateStore: stateStore,
                fileCoordinator: fileCoordinator,
                ossClientFactory: { _ in ossSession },
                clock: FixedUploadCoordinatorClock(now: Date(timeIntervalSince1970: 900))
            )
        )
    )

    let result = try await client.upload(
        UploadRequest(fileURL: sourceURL, clientHints: ["kind": "multipart"], rawTags: ["scene": "robot"], displayName: "multipart")
    )

    #expect(result.uploadID == "upload-multipart")
    #expect(await ossSession.initiateCalls() == 1)
    #expect(await ossSession.putObjectCalls().isEmpty)
    #expect(await ossSession.uploadCalls() == [
        UploadCall(multipartUploadID: "multipart-24", partNumber: 1, size: 8),
        UploadCall(multipartUploadID: "multipart-24", partNumber: 2, size: 8),
        UploadCall(multipartUploadID: "multipart-24", partNumber: 3, size: 8),
    ])
    #expect(await ossSession.uploadedBodyBytes() == [
        Data(payload[0 ..< 8]),
        Data(payload[8 ..< 16]),
        Data(payload[16 ..< 24]),
    ])
    let bookmark = Data("bookmark:/files/demo-multipart.bin".utf8)
    #expect(securityAccessor.accessRecords().filter { $0.bookmarkData == bookmark }.count >= 4)
    #expect(await ossSession.completeCalls() == [[1, 2, 3]])
    #expect(await gatewayClient.completeInvocations() == [
        CompleteInvocation(
            uploadID: "upload-multipart",
            fileSize: 24,
            rawTags: sourceFileNameRawTags(fileName: "demo-multipart.bin"),
            completedPartCount: 3,
            ossObjectEtag: "\"etag-multipart-object\"",
            partSizeBytes: 8
        ),
    ])
    let completedState = try await stateStore.loadSnapshot(logicalUploadID: "logical-1")
    #expect(completedState?.uploadedParts.count == 3)
}

@Test func uploadEventsPreservesEventOrderingAndCompletedEvent() async throws {
    let sourceURL = URL(fileURLWithPath: "/files/demo-events.bin")
    let payload = Data("events-flow".utf8)
    let fileSystem = MemoryFileSystem(files: [
        sourceURL: .file(size: UInt64(payload.count), modifiedAt: Date(timeIntervalSince1970: 160), data: payload),
    ])
    let fileCoordinator = FileStagingCoordinator(
        stagingRoot: URL(fileURLWithPath: "/staging"),
        fileSystem: fileSystem,
        securityScopedAccessor: PassthroughSecurityScopedAccessor()
    )
    let stateStore = UploadStateStore(
        persistRoot: FileManager.default.temporaryDirectory.appendingPathComponent("data-gateway-client-e3-events-\(UUID().uuidString)"),
        fileManager: .default,
        clock: FixedUploadCoordinatorStoreClock(now: Date(timeIntervalSince1970: 950))
    )
    let gatewayClient = MockUploadCoordinatorGatewayClient(
        createResponse: makeCreateLogicalUploadResponse(uploadID: "upload-events", objectKey: "objects/events.bin", partSizeBytes: 1024),
        recoveryResponse: makeContinueRecoveryResponse(currentUploadID: "upload-events"),
        reissueResponse: makeReissueResponse(uploadID: "upload-events", credentials: makeCoordinatorUploadCredentials(expireAtUnix: 5_000, tokenSuffix: "events", objectKey: "objects/events.bin", partSizeBytes: 1024)),
        completeResponse: Archebase_DataGateway_V1_CompleteUploadResponse()
    )
    let ossSession = MockOssUploadSession(
        multipartUploadID: "multipart-events",
        uploadedParts: [
            UploadedPartDescriptor(partNumber: 1, etag: "\"etag-events-part\"", size: Int64(payload.count), lastModified: nil, hashCRC64: nil),
        ],
        completedETag: "\"etag-events-object\""
    )
    let client = DataGatewayClient(
        uploadCoordinator: UploadCoordinator(
            executionPolicy: makeExecutionPolicy(),
            dependencies: UploadCoordinatorDependencies(
                gatewayClient: gatewayClient,
                stateStore: stateStore,
                fileCoordinator: fileCoordinator,
                ossClientFactory: { _ in ossSession },
                clock: FixedUploadCoordinatorClock(now: Date(timeIntervalSince1970: 950))
            )
        )
    )

    var events: [UploadEvent] = []
    for try await event in await client.uploadEvents(
        UploadRequest(fileURL: sourceURL, clientHints: ["kind": "events"], rawTags: ["scene": "robot"], displayName: "events")
    ) {
        events.append(event)
    }

    #expect(events == [
        .preparing,
        .authenticating,
        .creatingLogicalUpload,
        .uploadingPart(partNumber: 1, sentBytes: UInt64(payload.count), totalBytes: UInt64(payload.count)),
        .completingBusinessUpload(uploadID: "upload-events"),
        .completed(UploadResult(
            logicalUploadID: "logical-1",
            uploadID: "upload-events",
            bucket: "bucket-1",
            objectKey: "objects/events.bin",
            fileSize: UInt64(payload.count),
            ossObjectETag: "\"etag-events-object\""
        )),
    ])
}

@Test func uploadEventsTerminatesWithError() async {
    let sourceURL = URL(fileURLWithPath: "/files/demo-events-error.bin")
    let payload = Data("events-error".utf8)
    let fileSystem = MemoryFileSystem(files: [
        sourceURL: .file(size: UInt64(payload.count), modifiedAt: Date(timeIntervalSince1970: 180), data: payload),
    ])
    let fileCoordinator = FileStagingCoordinator(
        stagingRoot: URL(fileURLWithPath: "/staging"),
        fileSystem: fileSystem,
        securityScopedAccessor: PassthroughSecurityScopedAccessor()
    )
    let stateStore = UploadStateStore(
        persistRoot: FileManager.default.temporaryDirectory.appendingPathComponent("data-gateway-client-e3-error-\(UUID().uuidString)"),
        fileManager: .default,
        clock: FixedUploadCoordinatorStoreClock(now: Date(timeIntervalSince1970: 980))
    )
    let gatewayClient = MockUploadCoordinatorGatewayClient(
        createResponse: makeCreateLogicalUploadResponse(uploadID: "upload-events-error", objectKey: "objects/events-error.bin", partSizeBytes: 1024),
        recoveryResponse: makeContinueRecoveryResponse(currentUploadID: "upload-events-error"),
        reissueResponse: makeReissueResponse(uploadID: "upload-events-error", credentials: makeCoordinatorUploadCredentials(expireAtUnix: 5_000, tokenSuffix: "events-error", objectKey: "objects/events-error.bin", partSizeBytes: 1024)),
        completeResponse: Archebase_DataGateway_V1_CompleteUploadResponse()
    )
    let ossSession = MockOssUploadSession(
        multipartUploadID: "multipart-events-error",
        uploadedParts: [
            UploadedPartDescriptor(partNumber: 1, etag: "\"etag-events-error-part\"", size: Int64(payload.count), lastModified: nil, hashCRC64: nil),
        ],
        completedETag: "\"etag-events-error-object\"",
        failOnPutObject: DataGatewayClientError.ossFailed(httpStatus: 500, ossCode: "InternalError", message: "put failed")
    )
    let client = DataGatewayClient(
        uploadCoordinator: UploadCoordinator(
            executionPolicy: makeExecutionPolicy(),
            dependencies: UploadCoordinatorDependencies(
                gatewayClient: gatewayClient,
                stateStore: stateStore,
                fileCoordinator: fileCoordinator,
                ossClientFactory: { _ in ossSession },
                clock: FixedUploadCoordinatorClock(now: Date(timeIntervalSince1970: 980))
            )
        )
    )

    var events: [UploadEvent] = []
    do {
        for try await event in await client.uploadEvents(
            UploadRequest(fileURL: sourceURL, clientHints: ["kind": "events-error"], rawTags: ["scene": "robot"], displayName: "events-error")
        ) {
            events.append(event)
        }
        Issue.record("expected uploadEvents to throw")
    } catch let error as DataGatewayClientError {
        #expect(error == .ossFailed(httpStatus: 500, ossCode: "InternalError", message: "put failed"))
    } catch {
        Issue.record("unexpected error: \(error)")
    }

    #expect(events == [
        .preparing,
        .authenticating,
        .creatingLogicalUpload,
        .uploadingPart(partNumber: 1, sentBytes: UInt64(payload.count), totalBytes: UInt64(payload.count)),
    ])
}

@Test func resumeUploadFailsWhenSourceFileMissing() async {
    let stateStore = UploadStateStore(
        persistRoot: FileManager.default.temporaryDirectory.appendingPathComponent("data-gateway-client-e4-missing-\(UUID().uuidString)"),
        fileManager: .default,
        clock: FixedUploadCoordinatorStoreClock(now: Date(timeIntervalSince1970: 1_100))
    )
    let state = makePersistedResumeState(
        managedFileURL: URL(fileURLWithPath: "/missing/demo.bin"),
        uploadedParts: []
    )
    try? await stateStore.saveActive(state)

    let coordinator = UploadCoordinator(
        executionPolicy: makeExecutionPolicy(),
        dependencies: UploadCoordinatorDependencies(
            gatewayClient: MockUploadCoordinatorGatewayClient(
                createResponse: makeCreateLogicalUploadResponse(),
                recoveryResponse: makeContinueRecoveryResponse(),
                reissueResponse: makeReissueResponse(uploadID: "upload-1", credentials: makeCoordinatorUploadCredentials(expireAtUnix: 8_000, tokenSuffix: "resume")),
                completeResponse: Archebase_DataGateway_V1_CompleteUploadResponse()
            ),
            stateStore: stateStore,
            fileCoordinator: FileStagingCoordinator(
                stagingRoot: URL(fileURLWithPath: "/staging"),
                fileSystem: MemoryFileSystem(files: [:]),
                securityScopedAccessor: PassthroughSecurityScopedAccessor()
            ),
            ossClientFactory: { _ in MockOssUploadSession(multipartUploadID: "multipart-resume", uploadedParts: [], completedETag: "\"etag\"") },
            clock: FixedUploadCoordinatorClock(now: Date(timeIntervalSince1970: 1_100))
        )
    )
    let client = DataGatewayClient(uploadCoordinator: coordinator)

    let error = await #expect(throws: DataGatewayClientError.self) {
        try await client.resumeUpload(logicalUploadID: "logical-resume")
    }

    #expect(error == .resumeNotPossible("source file missing: /missing/demo.bin"))
}

@Test func resumeUploadFailsWhenFingerprintChanges() async throws {
    let managedURL = URL(fileURLWithPath: "/staging/demo-resume.bin")
    let data = Data("changed-data".utf8)
    let stateStore = UploadStateStore(
        persistRoot: FileManager.default.temporaryDirectory.appendingPathComponent("data-gateway-client-e4-fingerprint-\(UUID().uuidString)"),
        fileManager: .default,
        clock: FixedUploadCoordinatorStoreClock(now: Date(timeIntervalSince1970: 1_200))
    )
    try await stateStore.saveActive(
        makePersistedResumeState(
            managedFileURL: managedURL,
            uploadedParts: []
        )
    )

    let coordinator = UploadCoordinator(
        executionPolicy: makeExecutionPolicy(),
        dependencies: UploadCoordinatorDependencies(
            gatewayClient: MockUploadCoordinatorGatewayClient(
                createResponse: makeCreateLogicalUploadResponse(),
                recoveryResponse: makeContinueRecoveryResponse(),
                reissueResponse: makeReissueResponse(uploadID: "upload-1", credentials: makeCoordinatorUploadCredentials(expireAtUnix: 8_000, tokenSuffix: "resume")),
                completeResponse: Archebase_DataGateway_V1_CompleteUploadResponse()
            ),
            stateStore: stateStore,
            fileCoordinator: FileStagingCoordinator(
                stagingRoot: URL(fileURLWithPath: "/staging"),
                fileSystem: MemoryFileSystem(files: [
                    managedURL: .file(size: UInt64(data.count), modifiedAt: Date(timeIntervalSince1970: 200), data: data),
                ]),
                securityScopedAccessor: PassthroughSecurityScopedAccessor()
            ),
            ossClientFactory: { _ in MockOssUploadSession(multipartUploadID: "multipart-resume", uploadedParts: [], completedETag: "\"etag\"") },
            clock: FixedUploadCoordinatorClock(now: Date(timeIntervalSince1970: 1_200))
        )
    )
    let client = DataGatewayClient(uploadCoordinator: coordinator)

    let error = await #expect(throws: DataGatewayClientError.self) {
        try await client.resumeUpload(logicalUploadID: "logical-resume")
    }

    #expect(error == .resumeNotPossible("local file fingerprint changed"))
}

@Test func resumeUploadContinuePathSkipsExistingPartsAndCompletes() async throws {
    let managedURL = URL(fileURLWithPath: "/staging/demo-resume-continue.bin")
    let payload = Data(repeating: 0x33, count: 24)
    let stateStore = UploadStateStore(
        persistRoot: FileManager.default.temporaryDirectory.appendingPathComponent("data-gateway-client-e4-continue-\(UUID().uuidString)"),
        fileManager: .default,
        clock: FixedUploadCoordinatorStoreClock(now: Date(timeIntervalSince1970: 1_300))
    )
    try await stateStore.saveActive(
        makePersistedResumeState(
            managedFileURL: managedURL,
            multipartUploadID: "multipart-existing",
            uploadedParts: [
                PersistedUploadedPart(
                    partNumber: 1,
                    etag: "\"etag-existing-part-1\"",
                    offsetStart: 0,
                    partSize: 8,
                    md5Hex: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
                ),
            ]
        )
    )

    let gatewayClient = MockUploadCoordinatorGatewayClient(
        createResponse: makeCreateLogicalUploadResponse(),
        recoveryResponse: makeContinueRecoveryResponse(
            currentUploadID: "upload-resume",
            completedPartCount: 1,
            credentialRefreshCount: 2,
            sessionExpireAtUnix: 9_000
        ),
        reissueResponse: makeReissueResponse(
            uploadID: "upload-resume",
            credentials: makeCoordinatorUploadCredentials(
                expireAtUnix: 9_000,
                tokenSuffix: "resume",
                objectKey: "objects/resume.bin",
                partSizeBytes: 8
            )
        ),
        completeResponse: Archebase_DataGateway_V1_CompleteUploadResponse()
    )
    let ossSession = MockOssUploadSession(
        multipartUploadID: "multipart-existing",
        uploadedParts: [
            UploadedPartDescriptor(partNumber: 1, etag: "\"etag-part-1\"", size: 8, lastModified: nil, hashCRC64: nil),
        ],
        completedETag: "\"etag-resume-object\"",
        listedParts: [
            UploadedPartDescriptor(partNumber: 2, etag: "\"etag-part-2\"", size: 8, lastModified: nil, hashCRC64: nil),
            UploadedPartDescriptor(partNumber: 3, etag: "\"etag-part-3\"", size: 8, lastModified: nil, hashCRC64: nil),
        ]
    )
    let coordinator = UploadCoordinator(
        executionPolicy: makeExecutionPolicy(),
        dependencies: UploadCoordinatorDependencies(
            gatewayClient: gatewayClient,
            stateStore: stateStore,
            fileCoordinator: FileStagingCoordinator(
                stagingRoot: URL(fileURLWithPath: "/staging"),
                fileSystem: MemoryFileSystem(files: [
                    managedURL: .file(size: UInt64(payload.count), modifiedAt: Date(timeIntervalSince1970: 250), data: payload),
                ]),
                securityScopedAccessor: PassthroughSecurityScopedAccessor()
            ),
            ossClientFactory: { _ in ossSession },
            clock: FixedUploadCoordinatorClock(now: Date(timeIntervalSince1970: 1_300))
        )
    )
    let client = DataGatewayClient(uploadCoordinator: coordinator)

    let result = try await client.resumeUpload(logicalUploadID: "logical-resume")

    #expect(result.uploadID == "upload-resume")
    #expect(result.objectKey == "objects/resume.bin")
    #expect(await gatewayClient.reissueInvocations() == ["upload-resume"])
    #expect(await gatewayClient.getRecoveryInvocations() == ["logical-resume"])
    #expect(await ossSession.uploadCalls() == [
        UploadCall(multipartUploadID: "multipart-existing", partNumber: 1, size: 8),
    ])
    #expect(await gatewayClient.completeInvocations() == [
        CompleteInvocation(
            uploadID: "upload-resume",
            fileSize: 24,
            rawTags: ["scene": "robot"],
            completedPartCount: 3,
            ossObjectEtag: "\"etag-resume-object\"",
            partSizeBytes: 8
        ),
    ])
    let completedState = try await stateStore.loadSnapshot(logicalUploadID: "logical-resume")
    #expect(completedState?.uploadedParts.count == 3)
}

@Test func listPendingUploadsReturnsOnlyActiveSnapshots() async throws {
    let stateStore = UploadStateStore(
        persistRoot: FileManager.default.temporaryDirectory.appendingPathComponent("data-gateway-client-e5-list-\(UUID().uuidString)"),
        fileManager: .default,
        clock: FixedUploadCoordinatorStoreClock(now: Date(timeIntervalSince1970: 1_400))
    )
    let oldActive = makePersistedResumeState(
        logicalUploadID: "logical-active-1",
        uploadID: "upload-active-1",
        managedFileURL: URL(fileURLWithPath: "/staging/active-1.bin"),
        updatedAt: Date(timeIntervalSince1970: 100),
        uploadedParts: []
    )
    let newActive = makePersistedResumeState(
        logicalUploadID: "logical-active-2",
        uploadID: "upload-active-2",
        managedFileURL: URL(fileURLWithPath: "/staging/active-2.bin"),
        updatedAt: Date(timeIntervalSince1970: 200),
        uploadedParts: []
    )
    let terminal = makePersistedResumeState(
        logicalUploadID: "logical-terminal",
        uploadID: "upload-terminal",
        managedFileURL: URL(fileURLWithPath: "/staging/terminal.bin"),
        phase: .terminalFailed,
        updatedAt: Date(timeIntervalSince1970: 300),
        uploadedParts: []
    )
    let completed = makePersistedResumeState(
        logicalUploadID: "logical-completed",
        uploadID: "upload-completed",
        managedFileURL: URL(fileURLWithPath: "/staging/completed.bin"),
        phase: .businessCompleting,
        updatedAt: Date(timeIntervalSince1970: 400),
        uploadedParts: []
    )
    try await stateStore.saveActive(oldActive)
    try await stateStore.saveActive(newActive)
    try await stateStore.moveToTerminal(terminal)
    try await stateStore.moveToCompleted(completed)

    let client = DataGatewayClient(
        uploadCoordinator: UploadCoordinator(
            executionPolicy: makeExecutionPolicy(),
            dependencies: UploadCoordinatorDependencies(
                gatewayClient: MockUploadCoordinatorGatewayClient(
                    createResponse: makeCreateLogicalUploadResponse(),
                    recoveryResponse: makeContinueRecoveryResponse(),
                    reissueResponse: makeReissueResponse(uploadID: "upload-1", credentials: makeCoordinatorUploadCredentials(expireAtUnix: 5_000, tokenSuffix: "fresh")),
                    completeResponse: Archebase_DataGateway_V1_CompleteUploadResponse()
                ),
                stateStore: stateStore,
                fileCoordinator: FileStagingCoordinator(
                    stagingRoot: URL(fileURLWithPath: "/staging"),
                    fileSystem: MemoryFileSystem(files: [:]),
                    securityScopedAccessor: PassthroughSecurityScopedAccessor()
                ),
                ossClientFactory: { _ in MockOssUploadSession(multipartUploadID: "multipart-unused", uploadedParts: [], completedETag: "\"etag\"") },
                clock: FixedUploadCoordinatorClock(now: Date(timeIntervalSince1970: 1_400))
            )
        )
    )

    let pending = try await client.listPendingUploads()

    #expect(pending.map(\.logicalUploadID) == ["logical-active-2", "logical-active-1"])
    #expect(pending.map(\.uploadID) == ["upload-active-2", "upload-active-1"])
    #expect(pending.allSatisfy { $0.phase == .uploading })
}

@Test func listPendingUploadsExcludesTerminalAndCompletedSnapshots() async throws {
    let stateStore = UploadStateStore(
        persistRoot: FileManager.default.temporaryDirectory.appendingPathComponent("data-gateway-client-e5-filter-\(UUID().uuidString)"),
        fileManager: .default,
        clock: FixedUploadCoordinatorStoreClock(now: Date(timeIntervalSince1970: 1_500))
    )
    try await stateStore.moveToTerminal(
        makePersistedResumeState(
            logicalUploadID: "logical-terminal-only",
            uploadID: "upload-terminal-only",
            managedFileURL: URL(fileURLWithPath: "/staging/terminal-only.bin"),
            phase: .terminalFailed,
            uploadedParts: []
        )
    )
    try await stateStore.moveToCompleted(
        makePersistedResumeState(
            logicalUploadID: "logical-completed-only",
            uploadID: "upload-completed-only",
            managedFileURL: URL(fileURLWithPath: "/staging/completed-only.bin"),
            phase: .businessCompleting,
            uploadedParts: []
        )
    )

    let client = DataGatewayClient(
        uploadCoordinator: UploadCoordinator(
            executionPolicy: makeExecutionPolicy(),
            dependencies: UploadCoordinatorDependencies(
                gatewayClient: MockUploadCoordinatorGatewayClient(
                    createResponse: makeCreateLogicalUploadResponse(),
                    recoveryResponse: makeContinueRecoveryResponse(),
                    reissueResponse: makeReissueResponse(uploadID: "upload-1", credentials: makeCoordinatorUploadCredentials(expireAtUnix: 5_000, tokenSuffix: "fresh")),
                    completeResponse: Archebase_DataGateway_V1_CompleteUploadResponse()
                ),
                stateStore: stateStore,
                fileCoordinator: FileStagingCoordinator(
                    stagingRoot: URL(fileURLWithPath: "/staging"),
                    fileSystem: MemoryFileSystem(files: [:]),
                    securityScopedAccessor: PassthroughSecurityScopedAccessor()
                ),
                ossClientFactory: { _ in MockOssUploadSession(multipartUploadID: "multipart-unused", uploadedParts: [], completedETag: "\"etag\"") },
                clock: FixedUploadCoordinatorClock(now: Date(timeIntervalSince1970: 1_500))
            )
        )
    )

    let pending = try await client.listPendingUploads()
    #expect(pending.isEmpty)
}

@Test func abortUploadCallsGatewayAndCleansLocalState() async throws {
    let stateStore = UploadStateStore(
        persistRoot: FileManager.default.temporaryDirectory.appendingPathComponent("data-gateway-client-e6-abort-\(UUID().uuidString)"),
        fileManager: .default,
        clock: FixedUploadCoordinatorStoreClock(now: Date(timeIntervalSince1970: 1_600))
    )
    try await stateStore.saveActive(
        makePersistedResumeState(
            logicalUploadID: "logical-abort",
            uploadID: "upload-abort",
            managedFileURL: URL(fileURLWithPath: "/staging/abort.bin"),
            uploadedParts: []
        )
    )
    let gatewayClient = MockUploadCoordinatorGatewayClient(
        createResponse: makeCreateLogicalUploadResponse(),
        recoveryResponse: makeContinueRecoveryResponse(),
        reissueResponse: makeReissueResponse(uploadID: "upload-1", credentials: makeCoordinatorUploadCredentials(expireAtUnix: 5_000, tokenSuffix: "fresh")),
        abortResponse: makeAbortResponse(logicalUploadID: "logical-abort", uploadID: "upload-abort"),
        completeResponse: Archebase_DataGateway_V1_CompleteUploadResponse()
    )
    let client = DataGatewayClient(
        uploadCoordinator: UploadCoordinator(
            executionPolicy: makeExecutionPolicy(),
            dependencies: UploadCoordinatorDependencies(
                gatewayClient: gatewayClient,
                stateStore: stateStore,
                fileCoordinator: FileStagingCoordinator(
                    stagingRoot: URL(fileURLWithPath: "/staging"),
                    fileSystem: MemoryFileSystem(files: [:]),
                    securityScopedAccessor: PassthroughSecurityScopedAccessor()
                ),
                ossClientFactory: { _ in MockOssUploadSession(multipartUploadID: "multipart-unused", uploadedParts: [], completedETag: "\"etag\"") },
                clock: FixedUploadCoordinatorClock(now: Date(timeIntervalSince1970: 1_600))
            )
        )
    )

    try await client.abortUpload(logicalUploadID: "logical-abort")

    #expect(await gatewayClient.abortInvocations() == [AbortInvocation(logicalUploadID: "logical-abort", reason: "aborted by client")])
    #expect(try await stateStore.loadSnapshot(logicalUploadID: "logical-abort") == nil)
}

@Test func abortUploadCleansLocalStateWhenGatewayReportsNotFound() async throws {
    let stateStore = UploadStateStore(
        persistRoot: FileManager.default.temporaryDirectory.appendingPathComponent("data-gateway-client-e6-not-found-\(UUID().uuidString)"),
        fileManager: .default,
        clock: FixedUploadCoordinatorStoreClock(now: Date(timeIntervalSince1970: 1_700))
    )
    try await stateStore.saveActive(
        makePersistedResumeState(
            logicalUploadID: "logical-abort-missing",
            uploadID: "upload-abort-missing",
            managedFileURL: URL(fileURLWithPath: "/staging/abort-missing.bin"),
            uploadedParts: []
        )
    )
    let gatewayClient = MockUploadCoordinatorGatewayClient(
        createResponse: makeCreateLogicalUploadResponse(),
        recoveryResponse: makeContinueRecoveryResponse(),
        reissueResponse: makeReissueResponse(uploadID: "upload-1", credentials: makeCoordinatorUploadCredentials(expireAtUnix: 5_000, tokenSuffix: "fresh")),
        abortError: DataGatewayClientError.gatewayFailed(
            statusCode: RPCError.Code.notFound.rawValue,
            detailCode: "DATA_GATEWAY_UPLOAD_NOT_FOUND",
            message: "upload not found"
        ),
        completeResponse: Archebase_DataGateway_V1_CompleteUploadResponse()
    )
    let client = DataGatewayClient(
        uploadCoordinator: UploadCoordinator(
            executionPolicy: makeExecutionPolicy(),
            dependencies: UploadCoordinatorDependencies(
                gatewayClient: gatewayClient,
                stateStore: stateStore,
                fileCoordinator: FileStagingCoordinator(
                    stagingRoot: URL(fileURLWithPath: "/staging"),
                    fileSystem: MemoryFileSystem(files: [:]),
                    securityScopedAccessor: PassthroughSecurityScopedAccessor()
                ),
                ossClientFactory: { _ in MockOssUploadSession(multipartUploadID: "multipart-unused", uploadedParts: [], completedETag: "\"etag\"") },
                clock: FixedUploadCoordinatorClock(now: Date(timeIntervalSince1970: 1_700))
            )
        )
    )

    try await client.abortUpload(logicalUploadID: "logical-abort-missing")

    #expect(try await stateStore.loadSnapshot(logicalUploadID: "logical-abort-missing") == nil)
}

@Test func abortUploadPropagatesUnexpectedGatewayErrorWithoutCleaningLocalState() async throws {
    let stateStore = UploadStateStore(
        persistRoot: FileManager.default.temporaryDirectory.appendingPathComponent("data-gateway-client-e6-error-\(UUID().uuidString)"),
        fileManager: .default,
        clock: FixedUploadCoordinatorStoreClock(now: Date(timeIntervalSince1970: 1_800))
    )
    let state = makePersistedResumeState(
        logicalUploadID: "logical-abort-error",
        uploadID: "upload-abort-error",
        managedFileURL: URL(fileURLWithPath: "/staging/abort-error.bin"),
        uploadedParts: []
    )
    try await stateStore.saveActive(state)
    let gatewayClient = MockUploadCoordinatorGatewayClient(
        createResponse: makeCreateLogicalUploadResponse(),
        recoveryResponse: makeContinueRecoveryResponse(),
        reissueResponse: makeReissueResponse(uploadID: "upload-1", credentials: makeCoordinatorUploadCredentials(expireAtUnix: 5_000, tokenSuffix: "fresh")),
        abortError: DataGatewayClientError.gatewayFailed(
            statusCode: RPCError.Code.permissionDenied.rawValue,
            detailCode: "DATA_GATEWAY_UPLOAD_NOT_OWNED",
            message: "upload not owned"
        ),
        completeResponse: Archebase_DataGateway_V1_CompleteUploadResponse()
    )
    let client = DataGatewayClient(
        uploadCoordinator: UploadCoordinator(
            executionPolicy: makeExecutionPolicy(),
            dependencies: UploadCoordinatorDependencies(
                gatewayClient: gatewayClient,
                stateStore: stateStore,
                fileCoordinator: FileStagingCoordinator(
                    stagingRoot: URL(fileURLWithPath: "/staging"),
                    fileSystem: MemoryFileSystem(files: [:]),
                    securityScopedAccessor: PassthroughSecurityScopedAccessor()
                ),
                ossClientFactory: { _ in MockOssUploadSession(multipartUploadID: "multipart-unused", uploadedParts: [], completedETag: "\"etag\"") },
                clock: FixedUploadCoordinatorClock(now: Date(timeIntervalSince1970: 1_800))
            )
        )
    )

    let error = await #expect(throws: DataGatewayClientError.self) {
        try await client.abortUpload(logicalUploadID: "logical-abort-error")
    }

    #expect(error == .gatewayFailed(
        statusCode: RPCError.Code.permissionDenied.rawValue,
        detailCode: "DATA_GATEWAY_UPLOAD_NOT_OWNED",
        message: "upload not owned"
    ))
    #expect(try await stateStore.loadSnapshot(logicalUploadID: "logical-abort-error") == state)
}

@Test func deleteLocalSnapshotRemovesOnlyLocalState() async throws {
    let stateStore = UploadStateStore(
        persistRoot: FileManager.default.temporaryDirectory.appendingPathComponent("data-gateway-client-e7-delete-\(UUID().uuidString)"),
        fileManager: .default,
        clock: FixedUploadCoordinatorStoreClock(now: Date(timeIntervalSince1970: 1_900))
    )
    try await stateStore.saveActive(
        makePersistedResumeState(
            logicalUploadID: "logical-delete-local",
            uploadID: "upload-delete-local",
            managedFileURL: URL(fileURLWithPath: "/staging/delete-local.bin"),
            uploadedParts: []
        )
    )
    let gatewayClient = MockUploadCoordinatorGatewayClient(
        createResponse: makeCreateLogicalUploadResponse(),
        recoveryResponse: makeContinueRecoveryResponse(),
        reissueResponse: makeReissueResponse(uploadID: "upload-1", credentials: makeCoordinatorUploadCredentials(expireAtUnix: 5_000, tokenSuffix: "fresh")),
        completeResponse: Archebase_DataGateway_V1_CompleteUploadResponse()
    )
    let client = DataGatewayClient(
        uploadCoordinator: UploadCoordinator(
            executionPolicy: makeExecutionPolicy(),
            dependencies: UploadCoordinatorDependencies(
                gatewayClient: gatewayClient,
                stateStore: stateStore,
                fileCoordinator: FileStagingCoordinator(
                    stagingRoot: URL(fileURLWithPath: "/staging"),
                    fileSystem: MemoryFileSystem(files: [:]),
                    securityScopedAccessor: PassthroughSecurityScopedAccessor()
                ),
                ossClientFactory: { _ in MockOssUploadSession(multipartUploadID: "multipart-unused", uploadedParts: [], completedETag: "\"etag\"") },
                clock: FixedUploadCoordinatorClock(now: Date(timeIntervalSince1970: 1_900))
            )
        )
    )

    try await client.deleteLocalSnapshot(logicalUploadID: "logical-delete-local")

    #expect(try await stateStore.loadSnapshot(logicalUploadID: "logical-delete-local") == nil)
    #expect(await gatewayClient.abortInvocations().isEmpty)
}

@Test func deleteLocalSnapshotDoesNotCallRemoteGateway() async throws {
    let stateStore = UploadStateStore(
        persistRoot: FileManager.default.temporaryDirectory.appendingPathComponent("data-gateway-client-e7-no-remote-\(UUID().uuidString)"),
        fileManager: .default,
        clock: FixedUploadCoordinatorStoreClock(now: Date(timeIntervalSince1970: 2_000))
    )
    try await stateStore.saveActive(
        makePersistedResumeState(
            logicalUploadID: "logical-no-remote",
            uploadID: "upload-no-remote",
            managedFileURL: URL(fileURLWithPath: "/staging/no-remote.bin"),
            uploadedParts: []
        )
    )
    let gatewayClient = MockUploadCoordinatorGatewayClient(
        createResponse: makeCreateLogicalUploadResponse(),
        recoveryResponse: makeContinueRecoveryResponse(),
        reissueResponse: makeReissueResponse(uploadID: "upload-1", credentials: makeCoordinatorUploadCredentials(expireAtUnix: 5_000, tokenSuffix: "fresh")),
        completeResponse: Archebase_DataGateway_V1_CompleteUploadResponse()
    )
    let client = DataGatewayClient(
        uploadCoordinator: UploadCoordinator(
            executionPolicy: makeExecutionPolicy(),
            dependencies: UploadCoordinatorDependencies(
                gatewayClient: gatewayClient,
                stateStore: stateStore,
                fileCoordinator: FileStagingCoordinator(
                    stagingRoot: URL(fileURLWithPath: "/staging"),
                    fileSystem: MemoryFileSystem(files: [:]),
                    securityScopedAccessor: PassthroughSecurityScopedAccessor()
                ),
                ossClientFactory: { _ in MockOssUploadSession(multipartUploadID: "multipart-unused", uploadedParts: [], completedETag: "\"etag\"") },
                clock: FixedUploadCoordinatorClock(now: Date(timeIntervalSince1970: 2_000))
            )
        )
    )

    try await client.deleteLocalSnapshot(logicalUploadID: "logical-no-remote")

    #expect(await gatewayClient.createInvocations().isEmpty)
    #expect(await gatewayClient.getRecoveryInvocations().isEmpty)
    #expect(await gatewayClient.reissueInvocations().isEmpty)
    #expect(await gatewayClient.abortInvocations().isEmpty)
    #expect(await gatewayClient.completeInvocations().isEmpty)
}

@Test func recoveryDecisionMapsAllGatewayBranches() {
    let state = makePersistedResumeState(
        managedFileURL: URL(fileURLWithPath: "/staging/recovery.bin"),
        uploadedParts: []
    )

    var `continue` = makeContinueRecoveryResponse(currentUploadID: "upload-continue")
    `continue`.nextAction = .continue
    #expect(
        UploadCoordinator.decideResumeAction(state: state, recovery: `continue`) == .continueExisting(uploadID: "upload-continue")
    )

    var completeOnly = makeContinueRecoveryResponse(currentUploadID: "upload-complete-only")
    completeOnly.nextAction = .completeOnly
    completeOnly.ossObjectEtag = "\"etag-expected\""
    #expect(
        UploadCoordinator.decideResumeAction(state: state, recovery: completeOnly) == .completeOnly(
            uploadID: "upload-complete-only",
            expectedObjectETag: "\"etag-expected\""
        )
    )

    var restart = makeContinueRecoveryResponse(currentUploadID: "upload-restart")
    restart.nextAction = .restart
    #expect(
        UploadCoordinator.decideResumeAction(state: state, recovery: restart) == .restartUpload(previousUploadID: "upload-restart")
    )

    var abort = makeContinueRecoveryResponse(currentUploadID: "upload-abort")
    abort.nextAction = .abort
    abort.terminalReason = "terminal by gateway"
    #expect(
        UploadCoordinator.decideResumeAction(state: state, recovery: abort) == .permanentFailure(reason: "terminal by gateway")
    )
}

@Test func reconcileRemotePartsSkipsExistingAndBackfillsRemoteOnlyParts() async throws {
    let managedURL = URL(fileURLWithPath: "/staging/reconcile-remote-only.bin")
    let payload = Data(repeating: 0x44, count: 24)
    let stateStore = UploadStateStore(
        persistRoot: FileManager.default.temporaryDirectory.appendingPathComponent("data-gateway-client-f2-remote-only-\(UUID().uuidString)"),
        fileManager: .default,
        clock: FixedUploadCoordinatorStoreClock(now: Date(timeIntervalSince1970: 2_100))
    )
    try await stateStore.saveActive(
        makePersistedResumeState(
            managedFileURL: managedURL,
            multipartUploadID: "multipart-reconcile",
            firstChunkMD5Hex: "6FF958F4163C8636BC55A1B888DBF5FD",
            uploadedParts: []
        )
    )

    let gatewayClient = MockUploadCoordinatorGatewayClient(
        createResponse: makeCreateLogicalUploadResponse(),
        recoveryResponse: makeContinueRecoveryResponse(currentUploadID: "upload-reconcile", completedPartCount: 0),
        reissueResponse: makeReissueResponse(
            uploadID: "upload-reconcile",
            credentials: makeCoordinatorUploadCredentials(expireAtUnix: 9_000, tokenSuffix: "reconcile", objectKey: "objects/reconcile.bin", partSizeBytes: 8)
        ),
        completeResponse: Archebase_DataGateway_V1_CompleteUploadResponse()
    )
    let ossSession = MockOssUploadSession(
        multipartUploadID: "multipart-reconcile",
        uploadedParts: [
            UploadedPartDescriptor(partNumber: 2, etag: "\"etag-local-upload-part-2\"", size: 8, lastModified: nil, hashCRC64: nil),
            UploadedPartDescriptor(partNumber: 3, etag: "\"etag-local-upload-part-3\"", size: 8, lastModified: nil, hashCRC64: nil),
        ],
        completedETag: "\"etag-reconcile-object\"",
        listedParts: [
            UploadedPartDescriptor(partNumber: 1, etag: "\"etag-remote-part-1\"", size: 8, lastModified: nil, hashCRC64: nil),
        ]
    )
    let client = DataGatewayClient(
        uploadCoordinator: UploadCoordinator(
            executionPolicy: makeExecutionPolicy(),
            dependencies: UploadCoordinatorDependencies(
                gatewayClient: gatewayClient,
                stateStore: stateStore,
                fileCoordinator: FileStagingCoordinator(
                    stagingRoot: URL(fileURLWithPath: "/staging"),
                    fileSystem: MemoryFileSystem(files: [
                        managedURL: .file(size: UInt64(payload.count), modifiedAt: Date(timeIntervalSince1970: 250), data: payload),
                    ]),
                    securityScopedAccessor: PassthroughSecurityScopedAccessor()
                ),
                ossClientFactory: { _ in ossSession },
                clock: FixedUploadCoordinatorClock(now: Date(timeIntervalSince1970: 2_100))
            )
        )
    )

    let result = try await client.resumeUpload(logicalUploadID: "logical-resume")

    #expect(result.uploadID == "upload-reconcile")
    #expect(await ossSession.uploadCalls() == [
        UploadCall(multipartUploadID: "multipart-reconcile", partNumber: 2, size: 8),
        UploadCall(multipartUploadID: "multipart-reconcile", partNumber: 3, size: 8),
    ])
    let completedState = try await stateStore.loadSnapshot(logicalUploadID: "logical-resume")
    #expect(completedState?.uploadedParts.map(\.partNumber) == [1, 2, 3])
}

@Test func reconcileRemotePartsConflictTriggersRestartError() async throws {
    let managedURL = URL(fileURLWithPath: "/staging/reconcile-conflict.bin")
    let payload = Data(repeating: 0x55, count: 16)
    let stateStore = UploadStateStore(
        persistRoot: FileManager.default.temporaryDirectory.appendingPathComponent("data-gateway-client-f2-conflict-\(UUID().uuidString)"),
        fileManager: .default,
        clock: FixedUploadCoordinatorStoreClock(now: Date(timeIntervalSince1970: 2_200))
    )
    try await stateStore.saveActive(
        makePersistedResumeState(
            managedFileURL: managedURL,
            multipartUploadID: "multipart-conflict",
            fileSize: 16,
            firstChunkMD5Hex: "13248EBD454606E66FA158C3FB452987",
            uploadedParts: [
                PersistedUploadedPart(partNumber: 1, etag: "\"etag-local\"", offsetStart: 0, partSize: 8, md5Hex: "A")
            ]
        )
    )

    let gatewayClient = MockUploadCoordinatorGatewayClient(
        createResponse: makeCreateLogicalUploadResponse(),
        recoveryResponse: makeContinueRecoveryResponse(currentUploadID: "upload-conflict", completedPartCount: 1),
        reissueResponse: makeReissueResponse(
            uploadID: "upload-conflict",
            credentials: makeCoordinatorUploadCredentials(expireAtUnix: 9_000, tokenSuffix: "conflict", objectKey: "objects/conflict.bin", partSizeBytes: 8)
        ),
        completeResponse: Archebase_DataGateway_V1_CompleteUploadResponse()
    )
    let ossSession = MockOssUploadSession(
        multipartUploadID: "multipart-conflict",
        uploadedParts: [],
        completedETag: "\"etag-unused\"",
        listedParts: [
            UploadedPartDescriptor(partNumber: 1, etag: "\"etag-remote\"", size: 8, lastModified: nil, hashCRC64: nil),
        ]
    )
    let client = DataGatewayClient(
        uploadCoordinator: UploadCoordinator(
            executionPolicy: makeExecutionPolicy(),
            dependencies: UploadCoordinatorDependencies(
                gatewayClient: gatewayClient,
                stateStore: stateStore,
                fileCoordinator: FileStagingCoordinator(
                    stagingRoot: URL(fileURLWithPath: "/staging"),
                    fileSystem: MemoryFileSystem(files: [
                        managedURL: .file(size: UInt64(payload.count), modifiedAt: Date(timeIntervalSince1970: 250), data: payload),
                    ]),
                    securityScopedAccessor: PassthroughSecurityScopedAccessor()
                ),
                ossClientFactory: { _ in ossSession },
                clock: FixedUploadCoordinatorClock(now: Date(timeIntervalSince1970: 2_200))
            )
        )
    )

    let error = await #expect(throws: DataGatewayClientError.self) {
        try await client.resumeUpload(logicalUploadID: "logical-resume")
    }
    #expect(error == .uploadRestartExceeded)
}

@Test func reconcileRemotePartsMissingMultipartTriggersRestartError() async throws {
    let managedURL = URL(fileURLWithPath: "/staging/reconcile-missing.bin")
    let payload = Data(repeating: 0x66, count: 16)
    let stateStore = UploadStateStore(
        persistRoot: FileManager.default.temporaryDirectory.appendingPathComponent("data-gateway-client-f2-missing-\(UUID().uuidString)"),
        fileManager: .default,
        clock: FixedUploadCoordinatorStoreClock(now: Date(timeIntervalSince1970: 2_300))
    )
    try await stateStore.saveActive(
        makePersistedResumeState(
            managedFileURL: managedURL,
            multipartUploadID: "multipart-missing",
            fileSize: 16,
            firstChunkMD5Hex: "527848B310B793E41E0616F38EE85059",
            uploadedParts: []
        )
    )

    let gatewayClient = MockUploadCoordinatorGatewayClient(
        createResponse: makeCreateLogicalUploadResponse(),
        recoveryResponse: makeContinueRecoveryResponse(currentUploadID: "upload-missing", completedPartCount: 0),
        reissueResponse: makeReissueResponse(
            uploadID: "upload-missing",
            credentials: makeCoordinatorUploadCredentials(expireAtUnix: 9_000, tokenSuffix: "missing", objectKey: "objects/missing.bin", partSizeBytes: 8)
        ),
        completeResponse: Archebase_DataGateway_V1_CompleteUploadResponse()
    )
    let ossSession = MockOssUploadSession(
        multipartUploadID: "multipart-missing",
        uploadedParts: [],
        completedETag: "\"etag-unused\"",
        listedParts: []
    )
    let client = DataGatewayClient(
        uploadCoordinator: UploadCoordinator(
            executionPolicy: makeExecutionPolicy(),
            dependencies: UploadCoordinatorDependencies(
                gatewayClient: gatewayClient,
                stateStore: stateStore,
                fileCoordinator: FileStagingCoordinator(
                    stagingRoot: URL(fileURLWithPath: "/staging"),
                    fileSystem: MemoryFileSystem(files: [
                        managedURL: .file(size: UInt64(payload.count), modifiedAt: Date(timeIntervalSince1970: 250), data: payload),
                    ]),
                    securityScopedAccessor: PassthroughSecurityScopedAccessor()
                ),
                ossClientFactory: { _ in ossSession },
                clock: FixedUploadCoordinatorClock(now: Date(timeIntervalSince1970: 2_300))
            )
        )
    )

    let error = await #expect(throws: DataGatewayClientError.self) {
        try await client.resumeUpload(logicalUploadID: "logical-resume")
    }
    #expect(error == .uploadRestartExceeded)
}

@Test func completeOnlyHeadObjectMatchCompletesBusinessUpload() async throws {
    let managedURL = URL(fileURLWithPath: "/staging/complete-only-success.bin")
    let payload = Data(repeating: 0x77, count: 16)
    let stateStore = UploadStateStore(
        persistRoot: FileManager.default.temporaryDirectory.appendingPathComponent("data-gateway-client-f3-success-\(UUID().uuidString)"),
        fileManager: .default,
        clock: FixedUploadCoordinatorStoreClock(now: Date(timeIntervalSince1970: 2_400))
    )
    try await stateStore.saveActive(
        makePersistedResumeState(
            managedFileURL: managedURL,
            fileSize: 16,
            firstChunkMD5Hex: "4F198E0478B5C35BF31990247AB23889",
            uploadedParts: [
                PersistedUploadedPart(partNumber: 1, etag: "\"etag-part-1\"", offsetStart: 0, partSize: 8, md5Hex: "A"),
                PersistedUploadedPart(partNumber: 2, etag: "\"etag-part-2\"", offsetStart: 8, partSize: 8, md5Hex: "B"),
            ]
        )
    )

    var recovery = makeContinueRecoveryResponse(currentUploadID: "upload-complete-only", completedPartCount: 2)
    recovery.nextAction = .completeOnly
    recovery.ossObjectEtag = "\"etag-head-match\""

    let gatewayClient = MockUploadCoordinatorGatewayClient(
        createResponse: makeCreateLogicalUploadResponse(),
        recoveryResponse: recovery,
        reissueResponse: makeReissueResponse(
            uploadID: "upload-complete-only",
            credentials: makeCoordinatorUploadCredentials(expireAtUnix: 9_000, tokenSuffix: "complete-only", objectKey: "objects/complete-only.bin", partSizeBytes: 8)
        ),
        completeResponse: Archebase_DataGateway_V1_CompleteUploadResponse()
    )
    let ossSession = MockOssUploadSession(
        multipartUploadID: "multipart-complete-only",
        uploadedParts: [],
        completedETag: "\"etag-unused\"",
        listedParts: [],
        headObjectETag: "\"etag-head-match\""
    )
    let client = DataGatewayClient(
        uploadCoordinator: UploadCoordinator(
            executionPolicy: makeExecutionPolicy(),
            dependencies: UploadCoordinatorDependencies(
                gatewayClient: gatewayClient,
                stateStore: stateStore,
                fileCoordinator: FileStagingCoordinator(
                    stagingRoot: URL(fileURLWithPath: "/staging"),
                    fileSystem: MemoryFileSystem(files: [
                        managedURL: .file(size: UInt64(payload.count), modifiedAt: Date(timeIntervalSince1970: 250), data: payload),
                    ]),
                    securityScopedAccessor: PassthroughSecurityScopedAccessor()
                ),
                ossClientFactory: { _ in ossSession },
                clock: FixedUploadCoordinatorClock(now: Date(timeIntervalSince1970: 2_400))
            )
        )
    )

    let result = try await client.resumeUpload(logicalUploadID: "logical-resume")

    #expect(result.ossObjectETag == "\"etag-head-match\"")
    #expect(await gatewayClient.completeInvocations() == [
        CompleteInvocation(
            uploadID: "upload-complete-only",
            fileSize: 16,
            rawTags: ["scene": "robot"],
            completedPartCount: 2,
            ossObjectEtag: "\"etag-head-match\"",
            partSizeBytes: 8
        ),
    ])
}

@Test func completeOnlyObjectMissingTriggersRestartError() async throws {
    let managedURL = URL(fileURLWithPath: "/staging/complete-only-missing.bin")
    let payload = Data(repeating: 0x78, count: 16)
    let stateStore = UploadStateStore(
        persistRoot: FileManager.default.temporaryDirectory.appendingPathComponent("data-gateway-client-f3-missing-\(UUID().uuidString)"),
        fileManager: .default,
        clock: FixedUploadCoordinatorStoreClock(now: Date(timeIntervalSince1970: 2_500))
    )
    try await stateStore.saveActive(
        makePersistedResumeState(
            managedFileURL: managedURL,
            fileSize: 16,
            firstChunkMD5Hex: "45ED9CC2F92B77CD8B2F5BD59FF635F8",
            uploadedParts: []
        )
    )

    var recovery = makeContinueRecoveryResponse(currentUploadID: "upload-complete-missing", completedPartCount: 0)
    recovery.nextAction = .completeOnly
    recovery.ossObjectEtag = "\"etag-expected\""

    let gatewayClient = MockUploadCoordinatorGatewayClient(
        createResponse: makeCreateLogicalUploadResponse(),
        recoveryResponse: recovery,
        reissueResponse: makeReissueResponse(
            uploadID: "upload-complete-missing",
            credentials: makeCoordinatorUploadCredentials(expireAtUnix: 9_000, tokenSuffix: "complete-missing", objectKey: "objects/complete-missing.bin", partSizeBytes: 8)
        ),
        completeResponse: Archebase_DataGateway_V1_CompleteUploadResponse()
    )
    let ossSession = MockOssUploadSession(
        multipartUploadID: "multipart-complete-missing",
        uploadedParts: [],
        completedETag: "\"etag-unused\"",
        listedParts: [],
        headObjectError: DataGatewayClientError.ossFailed(httpStatus: 404, ossCode: "NoSuchKey", message: "not found")
    )
    let client = DataGatewayClient(
        uploadCoordinator: UploadCoordinator(
            executionPolicy: makeExecutionPolicy(),
            dependencies: UploadCoordinatorDependencies(
                gatewayClient: gatewayClient,
                stateStore: stateStore,
                fileCoordinator: FileStagingCoordinator(
                    stagingRoot: URL(fileURLWithPath: "/staging"),
                    fileSystem: MemoryFileSystem(files: [
                        managedURL: .file(size: UInt64(payload.count), modifiedAt: Date(timeIntervalSince1970: 250), data: payload),
                    ]),
                    securityScopedAccessor: PassthroughSecurityScopedAccessor()
                ),
                ossClientFactory: { _ in ossSession },
                clock: FixedUploadCoordinatorClock(now: Date(timeIntervalSince1970: 2_500))
            )
        )
    )

    let error = await #expect(throws: DataGatewayClientError.self) {
        try await client.resumeUpload(logicalUploadID: "logical-resume")
    }
    #expect(error == .uploadRestartExceeded)
}

@Test func completeOnlyETagMismatchTriggersRestartError() async throws {
    let managedURL = URL(fileURLWithPath: "/staging/complete-only-mismatch.bin")
    let payload = Data(repeating: 0x79, count: 16)
    let stateStore = UploadStateStore(
        persistRoot: FileManager.default.temporaryDirectory.appendingPathComponent("data-gateway-client-f3-mismatch-\(UUID().uuidString)"),
        fileManager: .default,
        clock: FixedUploadCoordinatorStoreClock(now: Date(timeIntervalSince1970: 2_600))
    )
    try await stateStore.saveActive(
        makePersistedResumeState(
            managedFileURL: managedURL,
            fileSize: 16,
            firstChunkMD5Hex: "7D6347B403E1CB54BA71087F74D3EBBB",
            uploadedParts: []
        )
    )

    var recovery = makeContinueRecoveryResponse(currentUploadID: "upload-complete-mismatch", completedPartCount: 0)
    recovery.nextAction = .completeOnly
    recovery.ossObjectEtag = "\"etag-expected\""

    let gatewayClient = MockUploadCoordinatorGatewayClient(
        createResponse: makeCreateLogicalUploadResponse(),
        recoveryResponse: recovery,
        reissueResponse: makeReissueResponse(
            uploadID: "upload-complete-mismatch",
            credentials: makeCoordinatorUploadCredentials(expireAtUnix: 9_000, tokenSuffix: "complete-mismatch", objectKey: "objects/complete-mismatch.bin", partSizeBytes: 8)
        ),
        completeResponse: Archebase_DataGateway_V1_CompleteUploadResponse()
    )
    let ossSession = MockOssUploadSession(
        multipartUploadID: "multipart-complete-mismatch",
        uploadedParts: [],
        completedETag: "\"etag-unused\"",
        listedParts: [],
        headObjectETag: "\"etag-other\""
    )
    let client = DataGatewayClient(
        uploadCoordinator: UploadCoordinator(
            executionPolicy: makeExecutionPolicy(),
            dependencies: UploadCoordinatorDependencies(
                gatewayClient: gatewayClient,
                stateStore: stateStore,
                fileCoordinator: FileStagingCoordinator(
                    stagingRoot: URL(fileURLWithPath: "/staging"),
                    fileSystem: MemoryFileSystem(files: [
                        managedURL: .file(size: UInt64(payload.count), modifiedAt: Date(timeIntervalSince1970: 250), data: payload),
                    ]),
                    securityScopedAccessor: PassthroughSecurityScopedAccessor()
                ),
                ossClientFactory: { _ in ossSession },
                clock: FixedUploadCoordinatorClock(now: Date(timeIntervalSince1970: 2_600))
            )
        )
    )

    let error = await #expect(throws: DataGatewayClientError.self) {
        try await client.resumeUpload(logicalUploadID: "logical-resume")
    }
    #expect(error == .uploadRestartExceeded)
}

@Test func putObjectResumeCompletedStateUsesHeadObjectAndCompletesBusinessUpload() async throws {
    let managedURL = URL(fileURLWithPath: "/staging/put-resume-complete.bin")
    let payload = Data("resume".utf8)
    let stateStore = UploadStateStore(
        persistRoot: FileManager.default.temporaryDirectory.appendingPathComponent("data-gateway-client-put-complete-\(UUID().uuidString)"),
        fileManager: .default,
        clock: FixedUploadCoordinatorStoreClock(now: Date(timeIntervalSince1970: 2_700))
    )
    try await stateStore.saveActive(
        makePersistedResumeState(
            managedFileURL: managedURL,
            multipartUploadID: nil,
            phase: .multipartCompleted,
            fileSize: UInt64(payload.count),
            firstChunkMD5Hex: "69F2AFC2390CEC954F7C208B07212D39",
            uploadedParts: [
                PersistedUploadedPart(partNumber: 1, etag: "etag-put", offsetStart: 0, partSize: UInt64(payload.count), md5Hex: "69F2AFC2390CEC954F7C208B07212D39"),
            ]
        )
    )

    let gatewayClient = MockUploadCoordinatorGatewayClient(
        createResponse: makeCreateLogicalUploadResponse(),
        recoveryResponse: makeContinueRecoveryResponse(currentUploadID: "upload-put-complete", completedPartCount: 0),
        reissueResponse: makeReissueResponse(
            uploadID: "upload-put-complete",
            credentials: makeCoordinatorUploadCredentials(expireAtUnix: 9_000, tokenSuffix: "put-complete", objectKey: "objects/put-complete.bin", partSizeBytes: 8)
        ),
        completeResponse: Archebase_DataGateway_V1_CompleteUploadResponse()
    )
    let ossSession = MockOssUploadSession(
        multipartUploadID: "multipart-unused",
        uploadedParts: [],
        completedETag: "\"etag-unused\"",
        headObjectETag: "\"etag-put\""
    )
    let client = DataGatewayClient(
        uploadCoordinator: UploadCoordinator(
            executionPolicy: makeExecutionPolicy(),
            dependencies: UploadCoordinatorDependencies(
                gatewayClient: gatewayClient,
                stateStore: stateStore,
                fileCoordinator: FileStagingCoordinator(
                    stagingRoot: URL(fileURLWithPath: "/staging"),
                    fileSystem: MemoryFileSystem(files: [
                        managedURL: .file(size: UInt64(payload.count), modifiedAt: Date(timeIntervalSince1970: 250), data: payload),
                    ]),
                    securityScopedAccessor: PassthroughSecurityScopedAccessor()
                ),
                ossClientFactory: { _ in ossSession },
                clock: FixedUploadCoordinatorClock(now: Date(timeIntervalSince1970: 2_700))
            )
        )
    )

    let result = try await client.resumeUpload(logicalUploadID: "logical-resume")

    #expect(result.uploadID == "upload-put-complete")
    #expect(result.ossObjectETag == "\"etag-put\"")
    #expect(await ossSession.initiateCalls() == 0)
    #expect(await ossSession.putObjectCalls().isEmpty)
    #expect(await ossSession.uploadCalls().isEmpty)
    #expect(await gatewayClient.completeInvocations() == [
        CompleteInvocation(
            uploadID: "upload-put-complete",
            fileSize: Int64(payload.count),
            rawTags: ["scene": "robot"],
            completedPartCount: 1,
            ossObjectEtag: "\"etag-put\"",
            partSizeBytes: 8
        ),
    ])
}

@Test func putObjectResumeMissingObjectRestartsWithPutObject() async throws {
    let managedURL = URL(fileURLWithPath: "/staging/put-resume-missing.bin")
    let payload = Data("missing".utf8)
    let stateStore = UploadStateStore(
        persistRoot: FileManager.default.temporaryDirectory.appendingPathComponent("data-gateway-client-put-missing-\(UUID().uuidString)"),
        fileManager: .default,
        clock: FixedUploadCoordinatorStoreClock(now: Date(timeIntervalSince1970: 2_800))
    )
    try await stateStore.saveActive(
        makePersistedResumeState(
            managedFileURL: managedURL,
            multipartUploadID: nil,
            phase: .multipartCompleted,
            fileSize: UInt64(payload.count),
            firstChunkMD5Hex: "EA21841DA70E6405AF19FABC4FF8BDD9",
            uploadedParts: [
                PersistedUploadedPart(partNumber: 1, etag: "\"etag-old\"", offsetStart: 0, partSize: UInt64(payload.count), md5Hex: "EA21841DA70E6405AF19FABC4FF8BDD9"),
            ]
        )
    )

    let gatewayClient = MockUploadCoordinatorGatewayClient(
        createResponse: makeCreateLogicalUploadResponse(uploadID: "upload-put-restarted", objectKey: "objects/put-restarted.bin", partSizeBytes: 8),
        recoveryResponse: makeContinueRecoveryResponse(currentUploadID: "upload-put-missing", completedPartCount: 0),
        reissueResponse: makeReissueResponse(
            uploadID: "upload-put-missing",
            credentials: makeCoordinatorUploadCredentials(expireAtUnix: 9_000, tokenSuffix: "put-missing", objectKey: "objects/put-missing.bin", partSizeBytes: 8)
        ),
        completeResponse: Archebase_DataGateway_V1_CompleteUploadResponse()
    )
    let ossSession = MockOssUploadSession(
        multipartUploadID: "multipart-unused",
        uploadedParts: [],
        completedETag: "\"etag-restarted\"",
        headObjectError: DataGatewayClientError.ossFailed(httpStatus: 404, ossCode: "NoSuchKey", message: "not found")
    )
    let client = DataGatewayClient(
        uploadCoordinator: UploadCoordinator(
            executionPolicy: makeExecutionPolicy(),
            dependencies: UploadCoordinatorDependencies(
                gatewayClient: gatewayClient,
                stateStore: stateStore,
                fileCoordinator: FileStagingCoordinator(
                    stagingRoot: URL(fileURLWithPath: "/staging"),
                    fileSystem: MemoryFileSystem(files: [
                        managedURL: .file(size: UInt64(payload.count), modifiedAt: Date(timeIntervalSince1970: 250), data: payload),
                    ]),
                    securityScopedAccessor: PassthroughSecurityScopedAccessor()
                ),
                ossClientFactory: { _ in ossSession },
                clock: FixedUploadCoordinatorClock(now: Date(timeIntervalSince1970: 2_800))
            )
        )
    )

    let result = try await client.resumeUpload(logicalUploadID: "logical-resume")

    #expect(result.uploadID == "upload-put-restarted")
    #expect(result.ossObjectETag == "\"etag-restarted\"")
    #expect(await gatewayClient.createRestartInvocations() == [CreateInvocation(clientHints: ["device": "iphone"], restartFromUploadID: "upload-put-missing")])
    #expect(await ossSession.putObjectCalls() == [payload.count])
    #expect(await ossSession.initiateCalls() == 0)
    #expect(await ossSession.uploadCalls().isEmpty)
    #expect(await gatewayClient.completeInvocations() == [
        CompleteInvocation(
            uploadID: "upload-put-restarted",
            fileSize: Int64(payload.count),
            rawTags: ["scene": "robot"],
            completedPartCount: 1,
            ossObjectEtag: "\"etag-restarted\"",
            partSizeBytes: 8
        ),
    ])
}

@Test func putObjectResumeETagMismatchRestartsWithPutObject() async throws {
    let managedURL = URL(fileURLWithPath: "/staging/put-resume-mismatch.bin")
    let payload = Data("mismatch".utf8)
    let stateStore = UploadStateStore(
        persistRoot: FileManager.default.temporaryDirectory.appendingPathComponent("data-gateway-client-put-mismatch-\(UUID().uuidString)"),
        fileManager: .default,
        clock: FixedUploadCoordinatorStoreClock(now: Date(timeIntervalSince1970: 2_900))
    )
    try await stateStore.saveActive(
        makePersistedResumeState(
            managedFileURL: managedURL,
            multipartUploadID: nil,
            phase: .multipartCompleted,
            fileSize: UInt64(payload.count),
            firstChunkMD5Hex: "1D1C5B76DA944B44DB42C9D0558021C5",
            uploadedParts: [
                PersistedUploadedPart(partNumber: 1, etag: "\"etag-old\"", offsetStart: 0, partSize: UInt64(payload.count), md5Hex: "1D1C5B76DA944B44DB42C9D0558021C5"),
            ]
        )
    )

    let gatewayClient = MockUploadCoordinatorGatewayClient(
        createResponse: makeCreateLogicalUploadResponse(uploadID: "upload-put-mismatch-new", objectKey: "objects/put-mismatch-new.bin", partSizeBytes: 8),
        recoveryResponse: makeContinueRecoveryResponse(currentUploadID: "upload-put-mismatch", completedPartCount: 0),
        reissueResponse: makeReissueResponse(
            uploadID: "upload-put-mismatch",
            credentials: makeCoordinatorUploadCredentials(expireAtUnix: 9_000, tokenSuffix: "put-mismatch", objectKey: "objects/put-mismatch.bin", partSizeBytes: 8)
        ),
        completeResponse: Archebase_DataGateway_V1_CompleteUploadResponse()
    )
    let ossSession = MockOssUploadSession(
        multipartUploadID: "multipart-unused",
        uploadedParts: [],
        completedETag: "\"etag-mismatch-new\"",
        headObjectETag: "\"etag-other\""
    )
    let client = DataGatewayClient(
        uploadCoordinator: UploadCoordinator(
            executionPolicy: makeExecutionPolicy(),
            dependencies: UploadCoordinatorDependencies(
                gatewayClient: gatewayClient,
                stateStore: stateStore,
                fileCoordinator: FileStagingCoordinator(
                    stagingRoot: URL(fileURLWithPath: "/staging"),
                    fileSystem: MemoryFileSystem(files: [
                        managedURL: .file(size: UInt64(payload.count), modifiedAt: Date(timeIntervalSince1970: 250), data: payload),
                    ]),
                    securityScopedAccessor: PassthroughSecurityScopedAccessor()
                ),
                ossClientFactory: { _ in ossSession },
                clock: FixedUploadCoordinatorClock(now: Date(timeIntervalSince1970: 2_900))
            )
        )
    )

    let result = try await client.resumeUpload(logicalUploadID: "logical-resume")

    #expect(result.uploadID == "upload-put-mismatch-new")
    #expect(result.ossObjectETag == "\"etag-mismatch-new\"")
    #expect(await gatewayClient.createRestartInvocations() == [CreateInvocation(clientHints: ["device": "iphone"], restartFromUploadID: "upload-put-mismatch")])
    #expect(await ossSession.putObjectCalls() == [payload.count])
    #expect(await ossSession.initiateCalls() == 0)
    #expect(await ossSession.uploadCalls().isEmpty)
}

@Test func refreshesCredentialsBeforeNextPartWhenTtlIsLow() async throws {
    let sourceURL = URL(fileURLWithPath: "/files/refresh-next-part.bin")
    let payload = Data(repeating: 0x80, count: 24)
    let fileSystem = MemoryFileSystem(files: [
        sourceURL: .file(size: UInt64(payload.count), modifiedAt: Date(timeIntervalSince1970: 300), data: payload),
    ])
    let fileCoordinator = FileStagingCoordinator(
        stagingRoot: URL(fileURLWithPath: "/staging"),
        fileSystem: fileSystem,
        securityScopedAccessor: PassthroughSecurityScopedAccessor()
    )
    let stateStore = UploadStateStore(
        persistRoot: FileManager.default.temporaryDirectory.appendingPathComponent("data-gateway-client-f4-next-part-\(UUID().uuidString)"),
        fileManager: .default,
        clock: FixedUploadCoordinatorStoreClock(now: Date(timeIntervalSince1970: 3_000))
    )
    let gatewayClient = MockUploadCoordinatorGatewayClient(
        createResponse: makeCreateLogicalUploadResponse(uploadID: "upload-refresh-part", objectKey: "objects/refresh-part.bin", partSizeBytes: 8),
        recoveryResponse: makeContinueRecoveryResponse(currentUploadID: "upload-refresh-part"),
        reissueResponse: makeReissueResponse(uploadID: "upload-refresh-part", credentials: makeCoordinatorUploadCredentials(expireAtUnix: 5_000, tokenSuffix: "refresh-part", objectKey: "objects/refresh-part.bin", partSizeBytes: 8)),
        completeResponse: Archebase_DataGateway_V1_CompleteUploadResponse()
    )
    let ossSession = RefreshAwareMockOssSession(
        multipartUploadID: "multipart-refresh-part",
        uploadDescriptors: [
            UploadedPartDescriptor(partNumber: 1, etag: "\"etag-1\"", size: 8, lastModified: nil, hashCRC64: nil),
            UploadedPartDescriptor(partNumber: 2, etag: "\"etag-2\"", size: 8, lastModified: nil, hashCRC64: nil),
            UploadedPartDescriptor(partNumber: 3, etag: "\"etag-3\"", size: 8, lastModified: nil, hashCRC64: nil),
        ],
        completedETag: "\"etag-refresh-part\"",
        refreshResults: [false, true, false, false],
        expirations: [
            Date(timeIntervalSince1970: 3_010),
            Date(timeIntervalSince1970: 5_000),
        ]
    )
    let eventRecorder = UploadEventRecorder()
    let client = DataGatewayClient(
        uploadCoordinator: UploadCoordinator(
            executionPolicy: makeExecutionPolicy(),
            dependencies: UploadCoordinatorDependencies(
                gatewayClient: gatewayClient,
                stateStore: stateStore,
                fileCoordinator: fileCoordinator,
                ossClientFactory: { _ in ossSession },
                clock: FixedUploadCoordinatorClock(now: Date(timeIntervalSince1970: 3_000))
            )
        )
    )

    _ = try await client.uploadEvents(
        UploadRequest(fileURL: sourceURL, clientHints: ["kind": "refresh-part"], rawTags: ["scene": "robot"], displayName: nil)
    ).reduce(into: ()) { _, event in
        await eventRecorder.record(event)
    }

    let events = await eventRecorder.events()
    #expect(events.contains(.refreshingCredentials(uploadID: "upload-refresh-part")))
    #expect(await ossSession.refreshCheckCount() == 4)
    let snapshot = try await stateStore.loadSnapshot(logicalUploadID: "logical-1")
    #expect(snapshot?.lastKnownSTSExpireAt == Date(timeIntervalSince1970: 5_000))
}

@Test func refreshesCredentialsBeforeCompleteWhenTtlIsLow() async throws {
    let sourceURL = URL(fileURLWithPath: "/files/refresh-before-complete.bin")
    let payload = Data(repeating: 0x81, count: 8)
    let fileSystem = MemoryFileSystem(files: [
        sourceURL: .file(size: UInt64(payload.count), modifiedAt: Date(timeIntervalSince1970: 320), data: payload),
    ])
    let fileCoordinator = FileStagingCoordinator(
        stagingRoot: URL(fileURLWithPath: "/staging"),
        fileSystem: fileSystem,
        securityScopedAccessor: PassthroughSecurityScopedAccessor()
    )
    let stateStore = UploadStateStore(
        persistRoot: FileManager.default.temporaryDirectory.appendingPathComponent("data-gateway-client-f4-before-complete-\(UUID().uuidString)"),
        fileManager: .default,
        clock: FixedUploadCoordinatorStoreClock(now: Date(timeIntervalSince1970: 3_100))
    )
    let gatewayClient = MockUploadCoordinatorGatewayClient(
        createResponse: makeCreateLogicalUploadResponse(uploadID: "upload-refresh-complete", objectKey: "objects/refresh-complete.bin", partSizeBytes: 4),
        recoveryResponse: makeContinueRecoveryResponse(currentUploadID: "upload-refresh-complete"),
        reissueResponse: makeReissueResponse(uploadID: "upload-refresh-complete", credentials: makeCoordinatorUploadCredentials(expireAtUnix: 5_200, tokenSuffix: "refresh-complete", objectKey: "objects/refresh-complete.bin", partSizeBytes: 4)),
        completeResponse: Archebase_DataGateway_V1_CompleteUploadResponse()
    )
    let ossSession = RefreshAwareMockOssSession(
        multipartUploadID: "multipart-refresh-complete",
        uploadDescriptors: [
            UploadedPartDescriptor(partNumber: 1, etag: "\"etag-1\"", size: 4, lastModified: nil, hashCRC64: nil),
            UploadedPartDescriptor(partNumber: 2, etag: "\"etag-2\"", size: 4, lastModified: nil, hashCRC64: nil),
        ],
        completedETag: "\"etag-refresh-complete\"",
        refreshResults: [false, false, true],
        expirations: [
            Date(timeIntervalSince1970: 3_110),
            Date(timeIntervalSince1970: 5_200),
        ]
    )
    let eventRecorder = UploadEventRecorder()
    let client = DataGatewayClient(
        uploadCoordinator: UploadCoordinator(
            executionPolicy: makeExecutionPolicy(),
            dependencies: UploadCoordinatorDependencies(
                gatewayClient: gatewayClient,
                stateStore: stateStore,
                fileCoordinator: fileCoordinator,
                ossClientFactory: { _ in ossSession },
                clock: FixedUploadCoordinatorClock(now: Date(timeIntervalSince1970: 3_100))
            )
        )
    )

    _ = try await client.uploadEvents(
        UploadRequest(fileURL: sourceURL, clientHints: ["kind": "refresh-complete"], rawTags: ["scene": "robot"], displayName: nil)
    ).reduce(into: ()) { _, event in
        await eventRecorder.record(event)
    }

    let events = await eventRecorder.events()
    #expect(events.contains(.refreshingCredentials(uploadID: "upload-refresh-complete")))
    #expect(await ossSession.refreshCheckCount() == 3)
    let snapshot = try await stateStore.loadSnapshot(logicalUploadID: "logical-1")
    #expect(snapshot?.lastKnownSTSExpireAt == Date(timeIntervalSince1970: 5_200))
}

@Test func refreshFailurePropagatesAsTerminalError() async throws {
    let sourceURL = URL(fileURLWithPath: "/files/refresh-failure.bin")
    let payload = Data(repeating: 0x82, count: 8)
    let fileSystem = MemoryFileSystem(files: [
        sourceURL: .file(size: UInt64(payload.count), modifiedAt: Date(timeIntervalSince1970: 340), data: payload),
    ])
    let fileCoordinator = FileStagingCoordinator(
        stagingRoot: URL(fileURLWithPath: "/staging"),
        fileSystem: fileSystem,
        securityScopedAccessor: PassthroughSecurityScopedAccessor()
    )
    let stateStore = UploadStateStore(
        persistRoot: FileManager.default.temporaryDirectory.appendingPathComponent("data-gateway-client-f4-refresh-failure-\(UUID().uuidString)"),
        fileManager: .default,
        clock: FixedUploadCoordinatorStoreClock(now: Date(timeIntervalSince1970: 3_200))
    )
    let gatewayClient = MockUploadCoordinatorGatewayClient(
        createResponse: makeCreateLogicalUploadResponse(uploadID: "upload-refresh-failure", objectKey: "objects/refresh-failure.bin", partSizeBytes: 8),
        recoveryResponse: makeContinueRecoveryResponse(currentUploadID: "upload-refresh-failure"),
        reissueResponse: makeReissueResponse(uploadID: "upload-refresh-failure", credentials: makeCoordinatorUploadCredentials(expireAtUnix: 3_210, tokenSuffix: "refresh-failure", objectKey: "objects/refresh-failure.bin", partSizeBytes: 8)),
        completeResponse: Archebase_DataGateway_V1_CompleteUploadResponse()
    )
    let refreshError = DataGatewayClientError.authenticationFailed(code: "AUTH_INTERNAL_ERROR", message: "refresh failed")
    let ossSession = RefreshAwareMockOssSession(
        multipartUploadID: "multipart-refresh-failure",
        uploadDescriptors: [
            UploadedPartDescriptor(partNumber: 1, etag: "\"etag-1\"", size: 8, lastModified: nil, hashCRC64: nil),
        ],
        completedETag: "\"etag-refresh-failure\"",
        refreshResults: [],
        expirations: [Date(timeIntervalSince1970: 3_210)],
        refreshError: refreshError
    )
    let client = DataGatewayClient(
        uploadCoordinator: UploadCoordinator(
            executionPolicy: makeExecutionPolicy(),
            dependencies: UploadCoordinatorDependencies(
                gatewayClient: gatewayClient,
                stateStore: stateStore,
                fileCoordinator: fileCoordinator,
                ossClientFactory: { _ in ossSession },
                clock: FixedUploadCoordinatorClock(now: Date(timeIntervalSince1970: 3_200))
            )
        )
    )

    let error = await #expect(throws: DataGatewayClientError.self) {
        try await client.upload(
            UploadRequest(fileURL: sourceURL, clientHints: ["kind": "refresh-failure"], rawTags: ["scene": "robot"], displayName: nil)
        )
    }

    #expect(error == refreshError)
}

@Test func restartUploadCreatesNewSessionAndPersistsIncrementedRestartCount() async throws {
    let managedURL = URL(fileURLWithPath: "/staging/restart-success.bin")
    let payload = Data(repeating: 0x83, count: 16)
    let stateStore = UploadStateStore(
        persistRoot: FileManager.default.temporaryDirectory.appendingPathComponent("data-gateway-client-f5-restart-success-\(UUID().uuidString)"),
        fileManager: .default,
        clock: FixedUploadCoordinatorStoreClock(now: Date(timeIntervalSince1970: 3_300))
    )
    try await stateStore.saveActive(
        makePersistedResumeState(
            logicalUploadID: "logical-restart-success",
            uploadID: "upload-old",
            managedFileURL: managedURL,
            fileSize: 16,
            firstChunkMD5Hex: "88A45B76E039CF396907131B542A18CD",
            uploadedParts: []
        )
    )

    var recovery = makeContinueRecoveryResponse(currentUploadID: "upload-old")
    recovery.nextAction = .restart

    let gatewayClient = MockUploadCoordinatorGatewayClient(
        createResponse: makeCreateLogicalUploadResponse(uploadID: "upload-new", objectKey: "objects/restart.bin", partSizeBytes: 8),
        recoveryResponse: recovery,
        reissueResponse: makeReissueResponse(uploadID: "upload-old", credentials: makeCoordinatorUploadCredentials(expireAtUnix: 9_000, tokenSuffix: "restart", objectKey: "objects/restart.bin", partSizeBytes: 8)),
        completeResponse: Archebase_DataGateway_V1_CompleteUploadResponse()
    )
    let ossSession = RefreshAwareMockOssSession(
        multipartUploadID: "multipart-restart-new",
        uploadDescriptors: [
            UploadedPartDescriptor(partNumber: 1, etag: "\"etag-restart-1\"", size: 8, lastModified: nil, hashCRC64: nil),
            UploadedPartDescriptor(partNumber: 2, etag: "\"etag-restart-2\"", size: 8, lastModified: nil, hashCRC64: nil),
        ],
        completedETag: "\"etag-restart-object\"",
        refreshResults: [false, false, false],
        expirations: [Date(timeIntervalSince1970: 9_000)]
    )
    let client = DataGatewayClient(
        uploadCoordinator: UploadCoordinator(
            executionPolicy: makeExecutionPolicy(),
            dependencies: UploadCoordinatorDependencies(
                gatewayClient: gatewayClient,
                stateStore: stateStore,
                fileCoordinator: FileStagingCoordinator(
                    stagingRoot: URL(fileURLWithPath: "/staging"),
                    fileSystem: MemoryFileSystem(files: [
                        managedURL: .file(size: UInt64(payload.count), modifiedAt: Date(timeIntervalSince1970: 250), data: payload),
                    ]),
                    securityScopedAccessor: PassthroughSecurityScopedAccessor()
                ),
                ossClientFactory: { _ in ossSession },
                clock: FixedUploadCoordinatorClock(now: Date(timeIntervalSince1970: 3_300))
            )
        )
    )

    let result = try await client.resumeUpload(logicalUploadID: "logical-restart-success")

    #expect(result.uploadID == "upload-new")
    #expect(await gatewayClient.createRestartInvocations() == [CreateInvocation(clientHints: ["device": "iphone"], restartFromUploadID: "upload-old")])
    let completedState = try await stateStore.loadSnapshot(logicalUploadID: "logical-1")
    #expect(completedState?.restartCount == 1)
}

@Test func restartUploadCountExceededDeletesSnapshotAndFails() async throws {
    let managedURL = URL(fileURLWithPath: "/staging/restart-exceeded.bin")
    let payload = Data(repeating: 0x84, count: 16)
    let stateStore = UploadStateStore(
        persistRoot: FileManager.default.temporaryDirectory.appendingPathComponent("data-gateway-client-f5-restart-exceeded-\(UUID().uuidString)"),
        fileManager: .default,
        clock: FixedUploadCoordinatorStoreClock(now: Date(timeIntervalSince1970: 3_400))
    )
    try await stateStore.saveActive(
        makePersistedResumeState(
            logicalUploadID: "logical-restart-exceeded",
            uploadID: "upload-old",
            managedFileURL: managedURL,
            fileSize: 16,
            storedRestartCount: 3,
            firstChunkMD5Hex: "4EF70D5B048D0D9EE8F9FA42686522BA",
            uploadedParts: []
        )
    )

    var recovery = makeContinueRecoveryResponse(currentUploadID: "upload-old")
    recovery.nextAction = .restart

    let gatewayClient = MockUploadCoordinatorGatewayClient(
        createResponse: makeCreateLogicalUploadResponse(uploadID: "upload-new", objectKey: "objects/restart.bin", partSizeBytes: 8),
        recoveryResponse: recovery,
        reissueResponse: makeReissueResponse(uploadID: "upload-old", credentials: makeCoordinatorUploadCredentials(expireAtUnix: 9_000, tokenSuffix: "restart", objectKey: "objects/restart.bin", partSizeBytes: 8)),
        completeResponse: Archebase_DataGateway_V1_CompleteUploadResponse()
    )
    let client = DataGatewayClient(
        uploadCoordinator: UploadCoordinator(
            executionPolicy: makeExecutionPolicy(customMaxRestartCount: 3),
            dependencies: UploadCoordinatorDependencies(
                gatewayClient: gatewayClient,
                stateStore: stateStore,
                fileCoordinator: FileStagingCoordinator(
                    stagingRoot: URL(fileURLWithPath: "/staging"),
                    fileSystem: MemoryFileSystem(files: [
                        managedURL: .file(size: UInt64(payload.count), modifiedAt: Date(timeIntervalSince1970: 250), data: payload),
                    ]),
                    securityScopedAccessor: PassthroughSecurityScopedAccessor()
                ),
                ossClientFactory: { _ in RefreshAwareMockOssSession(
                    multipartUploadID: "multipart-unused",
                    uploadDescriptors: [],
                    completedETag: "\"etag-unused\"",
                    refreshResults: [],
                    expirations: []
                ) },
                clock: FixedUploadCoordinatorClock(now: Date(timeIntervalSince1970: 3_400))
            )
        )
    )

    let error = await #expect(throws: DataGatewayClientError.self) {
        try await client.resumeUpload(logicalUploadID: "logical-restart-exceeded")
    }

    #expect(error == .uploadRestartExceeded)
    #expect(try await stateStore.loadSnapshot(logicalUploadID: "logical-restart-exceeded") == nil)
    #expect(await gatewayClient.createRestartInvocations().isEmpty)
}

private struct AbortInvocation: Equatable, Sendable {
    let logicalUploadID: String
    let reason: String
}

private actor UploadEventRecorder {
    private var recordedEvents: [UploadEvent] = []

    func record(_ event: UploadEvent) {
        self.recordedEvents.append(event)
    }

    func events() -> [UploadEvent] {
        self.recordedEvents
    }
}

private actor LogEventRecorder {
    private var recordedEvents: [DataGatewayClientLogEvent] = []

    func record(_ event: DataGatewayClientLogEvent) {
        self.recordedEvents.append(event)
    }

    func events() -> [DataGatewayClientLogEvent] {
        self.recordedEvents
    }
}

private actor MetricEventRecorder {
    struct Event: Sendable, Equatable {
        let name: String
        let dimensions: [String: String]
    }

    private var recordedEvents: [Event] = []

    func record(name: String, dimensions: [String: String]) {
        self.recordedEvents.append(Event(name: name, dimensions: dimensions))
    }

    func events() -> [Event] {
        self.recordedEvents
    }
}

private actor MockUploadCoordinatorGatewayClient: UploadCoordinatorGatewayClient {
    private let createResponse: Archebase_DataGateway_V1_CreateLogicalUploadResponse
    private let recoveryResponse: Archebase_DataGateway_V1_GetUploadRecoveryResponse
    private let reissueResponse: Archebase_DataGateway_V1_ReissueUploadCredentialsResponse
    private let abortResponse: Archebase_DataGateway_V1_AbortUploadResponse
    private let abortError: DataGatewayClientError?
    private let completeResponse: Archebase_DataGateway_V1_CompleteUploadResponse
    private let completeError: DataGatewayClientError?
    private var createCalls: [CreateInvocation] = []
    private var recoveryCalls: [String] = []
    private var reissueCalls: [String] = []
    private var abortCalls: [AbortInvocation] = []
    private var completeCalls: [CompleteInvocation] = []

    init(
        createResponse: Archebase_DataGateway_V1_CreateLogicalUploadResponse,
        recoveryResponse: Archebase_DataGateway_V1_GetUploadRecoveryResponse,
        reissueResponse: Archebase_DataGateway_V1_ReissueUploadCredentialsResponse,
        abortResponse: Archebase_DataGateway_V1_AbortUploadResponse = Archebase_DataGateway_V1_AbortUploadResponse(),
        abortError: DataGatewayClientError? = nil,
        completeResponse: Archebase_DataGateway_V1_CompleteUploadResponse,
        completeError: DataGatewayClientError? = nil
    ) {
        self.createResponse = createResponse
        self.recoveryResponse = recoveryResponse
        self.reissueResponse = reissueResponse
        self.abortResponse = abortResponse
        self.abortError = abortError
        self.completeResponse = completeResponse
        self.completeError = completeError
    }

    func createLogicalUpload(
        clientHints: [String : String],
        restartFromUploadID: String?
    ) async throws -> Archebase_DataGateway_V1_CreateLogicalUploadResponse {
        self.createCalls.append(
            CreateInvocation(
                clientHints: clientHints,
                restartFromUploadID: restartFromUploadID
            )
        )
        return self.createResponse
    }

    func getUploadRecovery(
        logicalUploadID: String
    ) async throws -> Archebase_DataGateway_V1_GetUploadRecoveryResponse {
        self.recoveryCalls.append(logicalUploadID)
        return self.recoveryResponse
    }

    func reissueUploadCredentials(
        uploadID: String
    ) async throws -> Archebase_DataGateway_V1_ReissueUploadCredentialsResponse {
        self.reissueCalls.append(uploadID)
        return self.reissueResponse
    }

    func abortUpload(
        logicalUploadID: String,
        reason: String
    ) async throws -> Archebase_DataGateway_V1_AbortUploadResponse {
        self.abortCalls.append(AbortInvocation(logicalUploadID: logicalUploadID, reason: reason))
        if let abortError {
            throw abortError
        }
        return self.abortResponse
    }

    func completeUpload(
        uploadID: String,
        fileSize: Int64,
        rawTags: [String : String],
        completedPartCount: Int32,
        ossObjectEtag: String,
        partSizeBytes: Int64
    ) async throws -> Archebase_DataGateway_V1_CompleteUploadResponse {
        self.completeCalls.append(
            CompleteInvocation(
                uploadID: uploadID,
                fileSize: fileSize,
                rawTags: rawTags,
                completedPartCount: completedPartCount,
                ossObjectEtag: ossObjectEtag,
                partSizeBytes: partSizeBytes
            )
        )
        if let completeError {
            throw completeError
        }
        return self.completeResponse
    }

    func createInvocations() -> [[String: String]] {
        self.createCalls.map(\.clientHints)
    }

    func createRestartInvocations() -> [CreateInvocation] {
        self.createCalls
    }

    func getRecoveryInvocations() -> [String] {
        self.recoveryCalls
    }

    func reissueInvocations() -> [String] {
        self.reissueCalls
    }

    func abortInvocations() -> [AbortInvocation] {
        self.abortCalls
    }

    func completeInvocations() -> [CompleteInvocation] {
        self.completeCalls
    }
}


private actor RefreshAwareMockOssSession: UploadCoordinatorMultipartSessionProtocol {
    private let multipartUploadID: String
    private let uploadDescriptors: [UploadedPartDescriptor]
    private let completedETag: String
    private let refreshResults: [Bool]
    private let expirations: [Date]
    private let refreshError: DataGatewayClientError?
    private var refreshChecks = 0
    private var refreshIndex = 0
    private var putObjectInvocations: [Int] = []
    private var putObjectBodyKinds: [OssUploadBody.Kind] = []

    init(
        multipartUploadID: String,
        uploadDescriptors: [UploadedPartDescriptor],
        completedETag: String,
        refreshResults: [Bool],
        expirations: [Date],
        refreshError: DataGatewayClientError? = nil
    ) {
        self.multipartUploadID = multipartUploadID
        self.uploadDescriptors = uploadDescriptors
        self.completedETag = completedETag
        self.refreshResults = refreshResults
        self.expirations = expirations
        self.refreshError = refreshError
    }

    func ensureFreshCredentialsIfNeeded() async throws -> Bool {
        self.refreshChecks += 1
        if let refreshError {
            throw refreshError
        }
        guard self.refreshIndex < self.refreshResults.count else {
            return false
        }
        let refreshed = self.refreshResults[self.refreshIndex]
        self.refreshIndex += 1
        return refreshed
    }

    func lastKnownCredentialExpiration() async -> Date? {
        let index = min(max(self.refreshIndex, 1) - 1, self.expirations.count - 1)
        guard !self.expirations.isEmpty else {
            return nil
        }
        return self.expirations[index]
    }

    func initiateMultipartUpload() async throws -> String {
        self.multipartUploadID
    }

    func uploadPart(
        multipartUploadID: String,
        partNumber: Int,
        body: OssUploadBody
    ) async throws -> UploadedPartDescriptor {
        _ = multipartUploadID
        _ = body
        guard let descriptor = self.uploadDescriptors.first(where: { $0.partNumber == partNumber }) else {
            fatalError("missing refresh-aware uploaded part fixture for partNumber=\(partNumber)")
        }
        return descriptor
    }

    func putObject(body: OssUploadBody) async throws -> UploadedPartDescriptor {
        self.putObjectInvocations.append(Int(body.sizeBytes))
        self.putObjectBodyKinds.append(body.kind)
        return UploadedPartDescriptor(
            partNumber: 1,
            etag: self.completedETag,
            size: body.sizeBytes,
            lastModified: nil,
            hashCRC64: nil
        )
    }

    func listParts(multipartUploadID: String) async throws -> [UploadedPartDescriptor] {
        _ = multipartUploadID
        return []
    }

    func headObjectETag() async throws -> String {
        self.completedETag
    }

    func completeMultipartUpload(
        multipartUploadID: String,
        parts: [UploadedPartDescriptor]
    ) async throws -> String {
        _ = multipartUploadID
        _ = parts
        return self.completedETag
    }

    func refreshCheckCount() -> Int {
        self.refreshChecks
    }

    func putObjectCalls() -> [Int] {
        self.putObjectInvocations
    }

    func putObjectKinds() -> [OssUploadBody.Kind] {
        self.putObjectBodyKinds
    }
}

private actor MockOssUploadSession: UploadCoordinatorMultipartSessionProtocol {
    private let multipartUploadID: String
    private let uploadedParts: [UploadedPartDescriptor]
    private let completedETag: String
    private let failOnComplete: DataGatewayClientError?
    private let failOnPutObject: DataGatewayClientError?
    private let listedParts: [UploadedPartDescriptor]
    private let headObjectETagValue: String?
    private let headObjectError: DataGatewayClientError?
    private var initiateInvocations = 0
    private var uploadInvocations: [UploadCall] = []
    private var uploadBodyBytes: [Data] = []
    private var putObjectInvocations: [Int] = []
    private var putObjectBodyKinds: [OssUploadBody.Kind] = []
    private var putObjectBodyBytes: [Data] = []
    private var completeInvocations: [[Int]] = []

    init(
        multipartUploadID: String,
        uploadedParts: [UploadedPartDescriptor],
        completedETag: String,
        failOnComplete: DataGatewayClientError? = nil,
        failOnPutObject: DataGatewayClientError? = nil,
        listedParts: [UploadedPartDescriptor]? = nil,
        headObjectETag: String? = nil,
        headObjectError: DataGatewayClientError? = nil
    ) {
        self.multipartUploadID = multipartUploadID
        self.uploadedParts = uploadedParts
        self.completedETag = completedETag
        self.failOnComplete = failOnComplete
        self.failOnPutObject = failOnPutObject
        self.listedParts = listedParts ?? uploadedParts
        self.headObjectETagValue = headObjectETag
        self.headObjectError = headObjectError
    }

    func ensureFreshCredentialsIfNeeded() async throws -> Bool {
        false
    }

    func lastKnownCredentialExpiration() async -> Date? {
        nil
    }

    func initiateMultipartUpload() async throws -> String {
        self.initiateInvocations += 1
        return self.multipartUploadID
    }

    func uploadPart(
        multipartUploadID: String,
        partNumber: Int,
        body: OssUploadBody
    ) async throws -> UploadedPartDescriptor {
        if let bodyBytes = try readUploadBodyBytes(body) {
            self.uploadBodyBytes.append(bodyBytes)
        }
        self.uploadInvocations.append(
            UploadCall(
                multipartUploadID: multipartUploadID,
                partNumber: partNumber,
                size: Int(body.sizeBytes),
                bodyKind: body.kind
            )
        )
        guard let descriptor = self.uploadedParts.first(where: { $0.partNumber == partNumber }) else {
            fatalError("missing uploaded part fixture for partNumber=\(partNumber)")
        }
        return descriptor
    }

    func putObject(body: OssUploadBody) async throws -> UploadedPartDescriptor {
        self.putObjectInvocations.append(Int(body.sizeBytes))
        self.putObjectBodyKinds.append(body.kind)
        if let bodyBytes = try readUploadBodyBytes(body) {
            self.putObjectBodyBytes.append(bodyBytes)
        }
        if let failOnPutObject {
            throw failOnPutObject
        }
        return UploadedPartDescriptor(
            partNumber: 1,
            etag: self.completedETag,
            size: body.sizeBytes,
            lastModified: nil,
            hashCRC64: nil
        )
    }

    func listParts(multipartUploadID: String) async throws -> [UploadedPartDescriptor] {
        _ = multipartUploadID
        return self.listedParts
    }

    func headObjectETag() async throws -> String {
        if let headObjectError {
            throw headObjectError
        }
        return self.headObjectETagValue ?? self.completedETag
    }

    func completeMultipartUpload(
        multipartUploadID: String,
        parts: [UploadedPartDescriptor]
    ) async throws -> String {
        _ = multipartUploadID
        self.completeInvocations.append(parts.map(\.partNumber))
        if let failOnComplete {
            throw failOnComplete
        }
        return self.completedETag
    }

    func initiateCalls() -> Int {
        self.initiateInvocations
    }

    func uploadCalls() -> [UploadCall] {
        self.uploadInvocations
    }

    func uploadedBodyBytes() -> [Data] {
        self.uploadBodyBytes
    }

    func putObjectCalls() -> [Int] {
        self.putObjectInvocations
    }

    func putObjectKinds() -> [OssUploadBody.Kind] {
        self.putObjectBodyKinds
    }

    func putObjectBytes() -> [Data] {
        self.putObjectBodyBytes
    }

    func completeCalls() -> [[Int]] {
        self.completeInvocations
    }
}

private struct CompleteInvocation: Equatable, Sendable {
    let uploadID: String
    let fileSize: Int64
    let rawTags: [String: String]
    let completedPartCount: Int32
    let ossObjectEtag: String
    let partSizeBytes: Int64
}

private func sourceFileNameRawTags(fileName: String) -> [String: String] {
    [
        "scene": "robot",
        RawTagsMerger.sourceFileNameRawTagKey: fileName,
    ]
}

private struct CreateInvocation: Equatable, Sendable {
    let clientHints: [String: String]
    let restartFromUploadID: String?
}

private struct UploadCall: Equatable, Sendable {
    let multipartUploadID: String
    let partNumber: Int
    let size: Int
    let bodyKind: OssUploadBody.Kind

    init(
        multipartUploadID: String,
        partNumber: Int,
        size: Int,
        bodyKind: OssUploadBody.Kind = .stream
    ) {
        self.multipartUploadID = multipartUploadID
        self.partNumber = partNumber
        self.size = size
        self.bodyKind = bodyKind
    }
}

private func readUploadBodyBytes(_ body: OssUploadBody) throws -> Data? {
    switch try body.byteStream() {
    case .none:
        return nil
    case .data(let data):
        return data
    case .file(let fileURL):
        guard FileManager.default.fileExists(atPath: fileURL.path) else {
            return nil
        }
        return try Data(contentsOf: fileURL)
    case .stream(let stream):
        return try readInputStreamBytes(stream)
    }
}

private func readInputStreamBytes(_ stream: InputStream) throws -> Data {
    var output = Data()
    var buffer = [UInt8](repeating: 0, count: 4096)

    stream.open()
    defer { stream.close() }

    while true {
        let count = stream.read(&buffer, maxLength: buffer.count)
        if count > 0 {
            output.append(buffer, count: count)
        } else if count == 0 {
            return output
        } else {
            throw stream.streamError ?? CocoaError(.fileReadUnknown)
        }
    }
}

private struct FixedUploadCoordinatorClock: UploadCoordinatorClock {
    let nowValue: Date

    init(now: Date) {
        self.nowValue = now
    }

    func now() async -> Date {
        self.nowValue
    }
}

private struct FixedUploadCoordinatorStoreClock: UploadStateStoreClock {
    let nowValue: Date

    init(now: Date) {
        self.nowValue = now
    }

    func now() async -> Date {
        self.nowValue
    }
}

private func makeCreateLogicalUploadResponse(
    logicalUploadID: String = "logical-1",
    uploadID: String = "upload-1",
    objectKey: String = "objects/demo.bin",
    partSizeBytes: Int64 = 64 * 1024 * 1024
) -> Archebase_DataGateway_V1_CreateLogicalUploadResponse {
    var response = Archebase_DataGateway_V1_CreateLogicalUploadResponse()
    response.logicalUploadID = logicalUploadID
    response.uploadID = uploadID
    response.credentials = makeCoordinatorUploadCredentials(
        expireAtUnix: 5_000,
        tokenSuffix: "fresh",
        objectKey: objectKey,
        partSizeBytes: partSizeBytes
    )
    return response
}

private func makeContinueRecoveryResponse(
    currentUploadID: String = "upload-1",
    completedPartCount: Int32 = 0,
    credentialRefreshCount: Int32 = 0,
    sessionExpireAtUnix: Int64 = 8_000
) -> Archebase_DataGateway_V1_GetUploadRecoveryResponse {
    var response = Archebase_DataGateway_V1_GetUploadRecoveryResponse()
    response.logicalUploadID = "logical-resume"
    response.logicalUploadStatus = .active
    response.currentUploadID = currentUploadID
    response.bucket = "bucket-1"
    response.endpoint = "https://oss-cn-shanghai.aliyuncs.com"
    response.objectKey = "objects/resume.bin"
    response.canRefreshCredentials = true
    response.restartAllowed = true
    response.credentialRefreshCount = credentialRefreshCount
    response.sessionExpireAtUnix = sessionExpireAtUnix
    response.nextAction = .continue
    response.completedPartCount = completedPartCount
    return response
}

private func makeReissueResponse(
    uploadID: String,
    credentials: Archebase_DataGateway_V1_UploadCredentials
) -> Archebase_DataGateway_V1_ReissueUploadCredentialsResponse {
    var response = Archebase_DataGateway_V1_ReissueUploadCredentialsResponse()
    response.logicalUploadID = "logical-resume"
    response.uploadID = uploadID
    response.credentials = credentials
    return response
}

private func makeCoordinatorUploadCredentials(
    expireAtUnix: Int64,
    tokenSuffix: String,
    endpoint: String = "https://oss-cn-shanghai.aliyuncs.com",
    bucket: String = "bucket-1",
    objectKey: String = "objects/demo.bin",
    partSizeBytes: Int64 = 64 * 1024 * 1024
) -> Archebase_DataGateway_V1_UploadCredentials {
    var credentials = Archebase_DataGateway_V1_UploadCredentials()
    credentials.bucket = bucket
    credentials.endpoint = endpoint
    credentials.objectKey = objectKey
    credentials.stsAccessKeyID = "ak-\(tokenSuffix)"
    credentials.stsAccessKeySecret = "sk-\(tokenSuffix)"
    credentials.stsSecurityToken = "token-\(tokenSuffix)"
    credentials.stsExpireAtUnix = expireAtUnix
    credentials.partSizeBytes = partSizeBytes
    return credentials
}

private func makeExecutionPolicy(customMaxRestartCount: Int = 3) -> UploadExecutionPolicy {
    UploadExecutionPolicy(
        maxRestartCount: customMaxRestartCount,
        autoResumeByFileURL: false,
        reconcileRemotePartsOnResume: true,
        cleanupOnTerminalFailure: true,
        credentialRefreshSkew: .seconds(30),
        persistence: makePersistencePolicy(copyExternalFileIntoManagedStaging: false)
    )
}

private func makeAbortResponse(
    logicalUploadID: String,
    uploadID: String
) -> Archebase_DataGateway_V1_AbortUploadResponse {
    var response = Archebase_DataGateway_V1_AbortUploadResponse()
    response.logicalUploadID = logicalUploadID
    response.uploadID = uploadID
    return response
}

private func makePersistedResumeState(
    logicalUploadID: String = "logical-resume",
    uploadID: String = "upload-1",
    managedFileURL: URL,
    multipartUploadID: String? = "multipart-existing",
    phase: PersistedUploadPhase = .uploading,
    updatedAt: Date = Date(timeIntervalSince1970: 300),
    fileSize: UInt64 = 24,
    storedRestartCount: Int = 0,
    firstChunkMD5Hex: String = "4AB60D2F88D28EFB0BEF8A05FE06580C",
    uploadedParts: [PersistedUploadedPart]
) -> PersistedUploadState {
    PersistedUploadState(
        version: 1,
        logicalUploadID: logicalUploadID,
        uploadID: uploadID,
        restartCount: storedRestartCount,
        multipartUploadID: multipartUploadID,
        bucket: "bucket-1",
        endpoint: "https://oss-cn-shanghai.aliyuncs.com",
        objectKey: "objects/resume.bin",
        fileURLBookmarkData: nil,
        managedFileURL: managedFileURL,
        fileSize: fileSize,
        fileFingerprint: LocalFileFingerprint(
            size: fileSize,
            modifiedAt: Date(timeIntervalSince1970: 250),
            firstChunkMD5Hex: firstChunkMD5Hex
        ),
        partSizeBytes: 8,
        uploadedParts: uploadedParts,
        clientHints: ["device": "iphone"],
        rawTags: ["scene": "robot"],
        phase: phase,
        lastKnownSTSExpireAt: Date(timeIntervalSince1970: 7_000),
        createdAt: Date(timeIntervalSince1970: 200),
        updatedAt: updatedAt
    )
}
