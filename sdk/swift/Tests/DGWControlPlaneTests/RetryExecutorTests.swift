import Foundation
import Testing

import DGWAuth
import DGWControlPlane
import DGWProto
import GRPCCore

@Test func retryClassifierMatchesControlPlaneMatrix() {
    #expect(ControlPlaneRetryClassifier.classify(RPCError(code: .unavailable, message: "down")).decision == .retry)
    #expect(ControlPlaneRetryClassifier.classify(RPCError(code: .deadlineExceeded, message: "timeout")).decision == .retry)
    #expect(ControlPlaneRetryClassifier.classify(RPCError(code: .internalError, message: "boom")).decision == .retry)
    #expect(ControlPlaneRetryClassifier.classify(RPCError(code: .invalidArgument, message: "bad request")).decision == .fail)
    #expect(ControlPlaneRetryClassifier.classify(RPCError(code: .permissionDenied, message: "denied")).decision == .fail)
    #expect(ControlPlaneRetryClassifier.classify(RPCError(code: .notFound, message: "missing")).decision == .fail)
    #expect(
        ControlPlaneRetryClassifier.classify(
            makeDetailedRPCError(
                code: .failedPrecondition,
                message: "upload not refreshable",
                detailCode: "DATA_GATEWAY_UPLOAD_NOT_REFRESHABLE",
                detailMessage: "upload not refreshable"
            )
        ).decision == .fail
    )
    #expect(ControlPlaneRetryClassifier.classify(RPCError(code: .unauthenticated, message: "expired token")).decision == .refreshAuthorization)
}

@Test func retryExecutorUsesExponentialBackoffForRetriableErrors() async throws {
    let sleeper = RecordingSleeper()
    let events = RetryEventRecorder()
    let executor = RetryExecutor(sleeper: sleeper) { event in
        await events.record(event)
    }

    let operation = FailableOperation<String>(results: [
        .failure(RPCError(code: .unavailable, message: "down-1")),
        .failure(RPCError(code: .unavailable, message: "down-2")),
        .success("ok"),
    ])

    let value = try await executor.execute(policy: RetryPolicy(maxAttempts: 5, initialBackoff: .seconds(1), maxBackoff: .seconds(8))) {
        try await operation.run()
    }

    #expect(value == "ok")
    #expect(await operation.attemptCount() == 3)
    #expect(await sleeper.durations() == [.seconds(1), .seconds(2)])
    #expect(await events.events().map(\.action) == [.retry, .retry])
}

@Test func authenticatedGatewayClientRefreshesJwtOnlyOnce() async throws {
    let authProvider = MockAuthorizationHeaderProvider(headers: ["Bearer stale", "Bearer fresh"])
    let gateway = MockGatewayClient(results: [
        .create(.failure(RPCError(code: .unauthenticated, message: "expired token"))),
        .create(.success(makeCreateResponse())),
    ])
    let sleeper = RecordingSleeper()
    let client = AuthenticatedGatewayControlPlaneClient(
        authProvider: authProvider,
        gatewayClient: gateway,
        retryExecutor: RetryExecutor(sleeper: sleeper),
        retryPolicy: RetryPolicy(maxAttempts: 3, initialBackoff: .seconds(1), maxBackoff: .seconds(8))
    )

    let response = try await client.createLogicalUpload(clientHints: ["device": "iphone"])

    #expect(response.logicalUploadID == "logical-1")
    #expect(await authProvider.authorizationRequests() == 2)
    #expect(await authProvider.invalidationCount() == 1)
    #expect(await gateway.createInvocations() == ["Bearer stale", "Bearer fresh"])
    #expect(await sleeper.durations().isEmpty)
}

@Test func nonRetriableErrorsFailImmediately() async {
    let authProvider = MockAuthorizationHeaderProvider(headers: ["Bearer token-1"])
    let gateway = MockGatewayClient(results: [
        .getRecovery(.failure(makeDetailedRPCError(
            code: .notFound,
            message: "missing upload",
            detailCode: "DATA_GATEWAY_UPLOAD_NOT_FOUND",
            detailMessage: "missing upload"
        ))),
    ])
    let sleeper = RecordingSleeper()
    let client = AuthenticatedGatewayControlPlaneClient(
        authProvider: authProvider,
        gatewayClient: gateway,
        retryExecutor: RetryExecutor(sleeper: sleeper),
        retryPolicy: RetryPolicy(maxAttempts: 5, initialBackoff: .seconds(1), maxBackoff: .seconds(8))
    )

    let error = await #expect(throws: RPCError.self) {
        try await client.getUploadRecovery(logicalUploadID: "logical-1")
    }

    #expect(error?.code == .notFound)
    #expect(await authProvider.invalidationCount() == 0)
    #expect(await sleeper.durations().isEmpty)
    #expect(await gateway.getRecoveryInvocations() == ["Bearer token-1"])
}

@Test func retryingObjectClientRetriesTransientErrorsWithSameUserBearer() async throws {
    let objectClient = MockObjectClient(results: [
        .failure(RPCError(code: .unavailable, message: "gateway unavailable")),
        .success(makeListObjectsResponse()),
    ])
    let sleeper = RecordingSleeper()
    let client = RetryingObjectControlPlaneClient(
        objectClient: objectClient,
        retryExecutor: RetryExecutor(sleeper: sleeper),
        retryPolicy: RetryPolicy(maxAttempts: 3, initialBackoff: .seconds(1), maxBackoff: .seconds(8))
    )

    let response = try await client.listObjects(
        pageSize: 10,
        pageToken: "page-1",
        filter: "status:verified",
        authorizationHeader: "Bearer user-token"
    )

    #expect(response.objects.map(\.fileID) == ["file-1"])
    #expect(await objectClient.invocations() == [
        "10:page-1:status:verified:Bearer user-token",
        "10:page-1:status:verified:Bearer user-token",
    ])
    #expect(await sleeper.durations() == [.seconds(1)])
}

@Test func retryingObjectClientDoesNotRetryUnauthenticatedUserBearer() async {
    let objectClient = MockObjectClient(results: [
        .failure(RPCError(code: .unauthenticated, message: "invalid user token")),
        .success(makeListObjectsResponse()),
    ])
    let sleeper = RecordingSleeper()
    let client = RetryingObjectControlPlaneClient(
        objectClient: objectClient,
        retryExecutor: RetryExecutor(sleeper: sleeper),
        retryPolicy: RetryPolicy(maxAttempts: 3, initialBackoff: .seconds(1), maxBackoff: .seconds(8))
    )

    let error = await #expect(throws: RPCError.self) {
        try await client.listObjects(
            pageSize: 10,
            pageToken: "",
            filter: "",
            authorizationHeader: "Bearer stale-user-token"
        )
    }

    #expect(error?.code == .unauthenticated)
    #expect(await objectClient.invocations() == ["10:::Bearer stale-user-token"])
    #expect(await sleeper.durations().isEmpty)
}

private actor RecordingSleeper: RetrySleeper {
    private var recordedDurations: [Duration] = []

    func sleep(for duration: Duration) async throws {
        self.recordedDurations.append(duration)
    }

    func durations() -> [Duration] {
        self.recordedDurations
    }
}

private actor RetryEventRecorder {
    private var recordedEvents: [ControlPlaneRetryEvent] = []

    func record(_ event: ControlPlaneRetryEvent) {
        self.recordedEvents.append(event)
    }

    func events() -> [ControlPlaneRetryEvent] {
        self.recordedEvents
    }
}

private actor FailableOperation<Value: Sendable> {
    enum ResultCase: Sendable {
        case success(Value)
        case failure(any Error)
    }

    private var results: [ResultCase]
    private var attempts = 0

    init(results: [ResultCase]) {
        self.results = results
    }

    func run() async throws -> Value {
        self.attempts += 1
        let current = self.results.removeFirst()
        switch current {
        case .success(let value):
            return value
        case .failure(let error):
            throw error
        }
    }

    func attemptCount() -> Int {
        self.attempts
    }
}

private actor MockAuthorizationHeaderProvider: AuthorizationHeaderProvider {
    private var headers: [String]
    private var requestedHeaders = 0
    private var invalidations = 0

    init(headers: [String]) {
        self.headers = headers
    }

    func authorizationHeader() async throws -> String {
        self.requestedHeaders += 1
        return self.headers.removeFirst()
    }

    func invalidateAuthorization() async {
        self.invalidations += 1
    }

    func authorizationRequests() -> Int {
        self.requestedHeaders
    }

    func invalidationCount() -> Int {
        self.invalidations
    }
}

private actor MockGatewayClient: GatewayControlPlaneClientProtocol {
    enum OperationResult: Sendable {
        case create(Result<Archebase_DataGateway_V1_CreateLogicalUploadResponse, RPCError>)
        case getRecovery(Result<Archebase_DataGateway_V1_GetUploadRecoveryResponse, RPCError>)
        case reissue(Result<Archebase_DataGateway_V1_ReissueUploadCredentialsResponse, RPCError>)
        case abort(Result<Archebase_DataGateway_V1_AbortUploadResponse, RPCError>)
        case complete(Result<Archebase_DataGateway_V1_CompleteUploadResponse, RPCError>)
    }

    private var results: [OperationResult]
    private var createHeaders: [String] = []
    private var recoveryHeaders: [String] = []

    init(results: [OperationResult]) {
        self.results = results
    }

    func createLogicalUpload(
        clientHints: [String : String],
        restartFromUploadID: String?,
        authorizationHeader: String
    ) async throws -> Archebase_DataGateway_V1_CreateLogicalUploadResponse {
        self.createHeaders.append(authorizationHeader)
        guard case .create(let result) = self.results.removeFirst() else {
            fatalError("unexpected operation ordering")
        }
        return try result.get()
    }

    func getUploadRecovery(
        logicalUploadID: String,
        authorizationHeader: String
    ) async throws -> Archebase_DataGateway_V1_GetUploadRecoveryResponse {
        self.recoveryHeaders.append(authorizationHeader)
        guard case .getRecovery(let result) = self.results.removeFirst() else {
            fatalError("unexpected operation ordering")
        }
        return try result.get()
    }

    func reissueUploadCredentials(
        uploadID: String,
        authorizationHeader: String
    ) async throws -> Archebase_DataGateway_V1_ReissueUploadCredentialsResponse {
        guard case .reissue(let result) = self.results.removeFirst() else {
            fatalError("unexpected operation ordering")
        }
        return try result.get()
    }

    func abortUpload(
        logicalUploadID: String,
        reason: String,
        authorizationHeader: String
    ) async throws -> Archebase_DataGateway_V1_AbortUploadResponse {
        guard case .abort(let result) = self.results.removeFirst() else {
            fatalError("unexpected operation ordering")
        }
        return try result.get()
    }

    func completeUpload(
        uploadID: String,
        fileSize: Int64,
        rawTags: [String : String],
        completedPartCount: Int32,
        ossObjectEtag: String,
        partSizeBytes: Int64,
        authorizationHeader: String
    ) async throws -> Archebase_DataGateway_V1_CompleteUploadResponse {
        guard case .complete(let result) = self.results.removeFirst() else {
            fatalError("unexpected operation ordering")
        }
        return try result.get()
    }

    func createInvocations() -> [String] {
        self.createHeaders
    }

    func getRecoveryInvocations() -> [String] {
        self.recoveryHeaders
    }
}

private actor MockObjectClient: ObjectControlPlaneClientProtocol {
    private var results: [Result<Archebase_DataGateway_V1_ListObjectsResponse, RPCError>]
    private var records: [String] = []

    init(results: [Result<Archebase_DataGateway_V1_ListObjectsResponse, RPCError>]) {
        self.results = results
    }

    func listObjects(
        pageSize: Int32,
        pageToken: String,
        filter: String,
        authorizationHeader: String
    ) async throws -> Archebase_DataGateway_V1_ListObjectsResponse {
        self.records.append("\(pageSize):\(pageToken):\(filter):\(authorizationHeader)")
        return try self.results.removeFirst().get()
    }

    func invocations() -> [String] {
        self.records
    }
}

private func makeCreateResponse() -> Archebase_DataGateway_V1_CreateLogicalUploadResponse {
    var response = Archebase_DataGateway_V1_CreateLogicalUploadResponse()
    response.logicalUploadID = "logical-1"
    response.uploadID = "upload-1"
    return response
}

private func makeListObjectsResponse() -> Archebase_DataGateway_V1_ListObjectsResponse {
    var object = Archebase_DataGateway_V1_DataObject()
    object.fileID = "file-1"
    object.status = .verified

    var response = Archebase_DataGateway_V1_ListObjectsResponse()
    response.objects = [object]
    return response
}

private func makeDetailedRPCError(
    code: RPCError.Code,
    message: String,
    detailCode: String,
    detailMessage: String
) -> RPCError {
    var detail = Archebase_Common_V1_ErrorDetail()
    detail.code = detailCode
    detail.message = detailMessage

    let bytes: [UInt8]
    do {
        bytes = try detail.serializedBytes()
    } catch {
        Issue.record("failed to encode error detail: \(error)")
        return RPCError(code: code, message: message)
    }

    var metadata = Metadata()
    metadata.addBinary(bytes, forKey: "grpc-status-details-bin")
    return RPCError(code: code, message: message, metadata: metadata)
}
