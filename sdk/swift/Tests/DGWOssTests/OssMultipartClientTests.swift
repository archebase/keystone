@preconcurrency @testable import AlibabaCloudOSS
import DGWControlPlane
import DGWProto
import Foundation
import Testing

@testable import DGWOss

@Test func ossMultipartClientBuildsExpectedSDKRequests() async throws {
    let sdkClient = MockAlibabaOSSSDKClient(
        initiateValue: OssInitiateMultipartUploadOutput(uploadID: "upload-1"),
        uploadPartValue: OssUploadPartOutput(etag: "\"etag-1\""),
        completeValue: OssCompleteMultipartUploadOutput(etag: "\"etag-complete\""),
        listValues: [
            OssListPartsPage(
                isTruncated: false,
                nextPartNumberMarker: nil,
                parts: [
                    OssListedPart(
                        partNumber: 1,
                        etag: "\"etag-1\"",
                        size: 3,
                        lastModified: nil,
                        hashCRC64: nil
                    ),
                ]
            ),
        ],
        headValue: OssHeadObjectOutput(etag: "\"etag-head\"")
    )

    let client = try OssMultipartClient(configuration: makeConfiguration(), sdkClient: sdkClient)

    _ = try await client.initiateMultipartUpload(objectKey: "objects/demo.bin")
    _ = try await client.uploadPart(
        objectKey: "objects/demo.bin",
        multipartUploadID: "upload-1",
        partNumber: 7,
        body: .data(Data("abc".utf8))
    )
    _ = try await client.putObject(
        objectKey: "objects/demo.bin",
        body: .data(Data("put".utf8))
    )
    _ = try await client.completeMultipartUpload(
        objectKey: "objects/demo.bin",
        multipartUploadID: "upload-1",
        parts: [
            UploadedPartDescriptor(partNumber: 2, etag: "\"etag-2\"", size: nil, lastModified: nil, hashCRC64: nil),
            UploadedPartDescriptor(partNumber: 1, etag: "\"etag-1\"", size: nil, lastModified: nil, hashCRC64: nil),
        ]
    )
    try await client.abortMultipartUpload(objectKey: "objects/demo.bin", multipartUploadID: "upload-1")
    _ = try await client.listParts(objectKey: "objects/demo.bin", multipartUploadID: "upload-1")
    _ = try await client.headObjectETag(objectKey: "objects/demo.bin")

    let initiateRequests = await sdkClient.initiateRequests()
    #expect(initiateRequests.count == 1)
    #expect(initiateRequests[0].bucket == "bucket-1")
    #expect(initiateRequests[0].key == "objects/demo.bin")

    let uploadRequests = await sdkClient.uploadPartRequests()
    #expect(uploadRequests.count == 1)
    #expect(uploadRequests[0].bucket == "bucket-1")
    #expect(uploadRequests[0].key == "objects/demo.bin")
    #expect(uploadRequests[0].uploadId == "upload-1")
    #expect(uploadRequests[0].partNumber == 7)
    #expect(try uploadRequests[0].body?.readData() == Data("abc".utf8))

    let putRequests = await sdkClient.putRequests()
    #expect(putRequests.count == 1)
    #expect(putRequests[0].bucket == "bucket-1")
    #expect(putRequests[0].key == "objects/demo.bin")
    #expect(try putRequests[0].body?.readData() == Data("put".utf8))

    let completeRequests = await sdkClient.completeRequests()
    #expect(completeRequests.count == 1)
    #expect(completeRequests[0].bucket == "bucket-1")
    #expect(completeRequests[0].key == "objects/demo.bin")
    #expect(completeRequests[0].uploadId == "upload-1")
    #expect(completeRequests[0].completeMultipartUpload?.parts?.compactMap(\.partNumber) == [1, 2])
    #expect(completeRequests[0].completeMultipartUpload?.parts?.compactMap(\.etag) == ["\"etag-1\"", "\"etag-2\""])

    let abortRequests = await sdkClient.abortRequests()
    #expect(abortRequests.count == 1)
    #expect(abortRequests[0].bucket == "bucket-1")
    #expect(abortRequests[0].key == "objects/demo.bin")
    #expect(abortRequests[0].uploadId == "upload-1")

    let listRequests = await sdkClient.listRequests()
    #expect(listRequests.count == 1)
    #expect(listRequests[0].bucket == "bucket-1")
    #expect(listRequests[0].key == "objects/demo.bin")
    #expect(listRequests[0].uploadId == "upload-1")

    let headRequests = await sdkClient.headRequests()
    #expect(headRequests.count == 1)
    #expect(headRequests[0].bucket == "bucket-1")
    #expect(headRequests[0].key == "objects/demo.bin")
}

@Test func ossMultipartClientMapsResponses() async throws {
    let sdkClient = MockAlibabaOSSSDKClient(
        initiateValue: OssInitiateMultipartUploadOutput(uploadID: "upload-1"),
        uploadPartValue: OssUploadPartOutput(etag: "\"etag-1\""),
        completeValue: OssCompleteMultipartUploadOutput(etag: "\"etag-complete\""),
        listValues: [
            OssListPartsPage(
                isTruncated: false,
                nextPartNumberMarker: nil,
                parts: [
                    OssListedPart(
                        partNumber: 2,
                        etag: "\"etag-2\"",
                        size: 4,
                        lastModified: Date(timeIntervalSince1970: 20),
                        hashCRC64: "crc-2"
                    ),
                ]
            ),
        ],
        headValue: OssHeadObjectOutput(etag: "\"etag-head\"")
    )

    let client = try OssMultipartClient(configuration: makeConfiguration(), sdkClient: sdkClient)

    let uploadID = try await client.initiateMultipartUpload(objectKey: "objects/demo.bin")
    #expect(uploadID == "upload-1")

    let uploadedPart = try await client.uploadPart(
        objectKey: "objects/demo.bin",
        multipartUploadID: "upload-1",
        partNumber: 2,
        body: .data(Data("abcd".utf8))
    )
    #expect(uploadedPart == UploadedPartDescriptor(
        partNumber: 2,
        etag: "\"etag-1\"",
        size: 4,
        lastModified: nil,
        hashCRC64: nil
    ))

    let putObject = try await client.putObject(
        objectKey: "objects/demo.bin",
        body: .data(Data("put-body".utf8))
    )
    #expect(putObject == UploadedPartDescriptor(
        partNumber: 1,
        etag: "\"etag-put\"",
        size: 8,
        lastModified: nil,
        hashCRC64: nil
    ))

    let etag = try await client.completeMultipartUpload(
        objectKey: "objects/demo.bin",
        multipartUploadID: "upload-1",
        parts: [uploadedPart]
    )
    #expect(etag == "\"etag-complete\"")

    let parts = try await client.listParts(objectKey: "objects/demo.bin", multipartUploadID: "upload-1")
    #expect(parts == [
        UploadedPartDescriptor(
            partNumber: 2,
            etag: "\"etag-2\"",
            size: 4,
            lastModified: Date(timeIntervalSince1970: 20),
            hashCRC64: "crc-2"
        ),
    ])

    let headETag = try await client.headObjectETag(objectKey: "objects/demo.bin")
    #expect(headETag == "\"etag-head\"")
}

@Test func ossMultipartClientAddsContentMD5ForExplicitIntegrityBodies() async throws {
    let root = FileManager.default.temporaryDirectory.appendingPathComponent("oss-md5-\(UUID().uuidString)", isDirectory: true)
    try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
    let fileURL = root.appendingPathComponent("body.bin")
    try Data("put-body".utf8).write(to: fileURL)
    let sdkClient = MockAlibabaOSSSDKClient(
        initiateValue: OssInitiateMultipartUploadOutput(uploadID: "upload-1"),
        uploadPartValue: OssUploadPartOutput(etag: "\"etag-1\""),
        completeValue: OssCompleteMultipartUploadOutput(etag: "\"etag-complete\""),
        listValues: [],
        headValue: OssHeadObjectOutput(etag: "\"etag-head\"")
    )
    let client = try OssMultipartClient(configuration: makeConfiguration(), sdkClient: sdkClient)

    _ = try await client.uploadPart(
        objectKey: "objects/demo.bin",
        multipartUploadID: "upload-1",
        partNumber: 1,
        body: .stream(sizeBytes: 3, contentMD5Base64: "part-md5-base64") {
            InputStream(data: Data("abc".utf8))
        }
    )
    _ = try await client.putObject(
        objectKey: "objects/demo.bin",
        body: .file(fileURL, sizeBytes: 8, contentMD5Base64: "put-md5-base64")
    )

    let uploadRequests = await sdkClient.uploadPartRequests()
    #expect(uploadRequests[0].commonProp.headers?["content-md5"] == "part-md5-base64")
    #expect(uploadRequests[0].commonProp.headers?["content-length"] == "3")

    let putRequests = await sdkClient.putRequests()
    #expect(putRequests[0].commonProp.headers?["content-md5"] == "put-md5-base64")
    #expect(putRequests[0].commonProp.headers?["content-length"] == "8")
}

@Test func listPartsMergesPaginatorPagesInOrder() async throws {
    let sdkClient = MockAlibabaOSSSDKClient(
        initiateValue: OssInitiateMultipartUploadOutput(uploadID: nil),
        uploadPartValue: OssUploadPartOutput(etag: nil),
        completeValue: OssCompleteMultipartUploadOutput(etag: nil),
        listValues: [
            OssListPartsPage(
                isTruncated: true,
                nextPartNumberMarker: 2,
                parts: [OssListedPart(partNumber: 2, etag: "\"etag-2\"", size: 2, lastModified: nil, hashCRC64: nil)]
            ),
            OssListPartsPage(
                isTruncated: false,
                nextPartNumberMarker: nil,
                parts: [OssListedPart(partNumber: 1, etag: "\"etag-1\"", size: 1, lastModified: nil, hashCRC64: nil)]
            ),
        ],
        headValue: OssHeadObjectOutput(etag: nil)
    )

    let client = try OssMultipartClient(configuration: makeConfiguration(), sdkClient: sdkClient)
    let parts = try await client.listParts(objectKey: "objects/demo.bin", multipartUploadID: "upload-1")

    #expect(parts.map(\.partNumber) == [1, 2])
    #expect(parts.map(\.etag) == ["\"etag-1\"", "\"etag-2\""])
}

@Test func putObjectMissingETagFailsInvalidResponse() async throws {
    let sdkClient = MockAlibabaOSSSDKClient(
        initiateValue: OssInitiateMultipartUploadOutput(uploadID: nil),
        uploadPartValue: OssUploadPartOutput(etag: nil),
        putValue: OssPutObjectOutput(etag: nil),
        completeValue: OssCompleteMultipartUploadOutput(etag: nil),
        listValues: [],
        headValue: OssHeadObjectOutput(etag: nil)
    )
    let client = try OssMultipartClient(configuration: makeConfiguration(), sdkClient: sdkClient)

    let error = await #expect(throws: OssOperationError.self) {
        try await client.putObject(objectKey: "objects/demo.bin", body: .data(Data("body".utf8)))
    }

    #expect(error == .invalidResponse("PutObject response missing ETag"))
}

@Test func putObjectURLErrorClassifiesAsRetriableTransportFailure() async throws {
    let sdkClient = MockAlibabaOSSSDKClient(
        initiateValue: OssInitiateMultipartUploadOutput(uploadID: nil),
        uploadPartValue: OssUploadPartOutput(etag: nil),
        putError: URLError(.timedOut),
        completeValue: OssCompleteMultipartUploadOutput(etag: nil),
        listValues: [],
        headValue: OssHeadObjectOutput(etag: nil)
    )
    let client = try OssMultipartClient(configuration: makeConfiguration(), sdkClient: sdkClient)

    let error = await #expect(throws: OssOperationError.self) {
        try await client.putObject(objectKey: "objects/demo.bin", body: .data(Data("body".utf8)))
    }

    #expect(error == .transportFailure(code: URLError.Code.timedOut.rawValue, message: URLError(.timedOut).localizedDescription))
    if let error {
        #expect(OSSDataPlaneErrorMapper.classify(error).action == .retry)
    }
}

@Test func ttlLowTriggersClientRebuild() async throws {
    let initialClient = RecordingMultipartClient(identifier: "initial")
    let refreshedClient = RecordingMultipartClient(
        identifier: "refreshed",
        uploadPartResult: UploadedPartDescriptor(
            partNumber: 1,
            etag: "\"etag-refreshed\"",
            size: 3,
            lastModified: nil,
            hashCRC64: nil
        )
    )
    let factory = RecordingMultipartClientFactory(clients: [initialClient, refreshedClient])
    let provider = MockGatewayUploadCredentialsProvider(
        responses: [
            makeReissueResponse(
                uploadID: "upload-1",
                credentials: makeUploadCredentials(expireAtUnix: 3_600, tokenSuffix: "refreshed")
            ),
        ]
    )
    let clock = TestOssSessionClock(now: Date(timeIntervalSince1970: 1_000))
    let context = try OssUploadSession.makeUploadContext(
        uploadID: "upload-1",
        credentials: makeUploadCredentials(expireAtUnix: 1_010, tokenSuffix: "initial")
    )
    let session = try OssUploadSession(
        context: context,
        refreshPolicy: STSRefreshPolicy(refreshSkew: .seconds(8), requestTimeout: .seconds(5)),
        requestTimeout: .seconds(5),
        retryMaxAttempts: 3,
        clientFactory: factory,
        credentialsProvider: provider,
        clock: clock
    )

    let uploadedPart = try await session.uploadPart(
        multipartUploadID: "multipart-1",
        partNumber: 1,
        body: .data(Data("abc".utf8))
    )

    #expect(uploadedPart.etag == "\"etag-refreshed\"")
    #expect(await provider.requestedUploadIDs() == ["upload-1"])
    #expect(await initialClient.uploadPartCalls().isEmpty)
    #expect(await refreshedClient.uploadPartCalls() == ["multipart-1:1"])
    #expect(factory.configurations().count == 2)

    let refreshedContext = await session.uploadContext()
    #expect(refreshedContext.bucket == "bucket-1")
    #expect(refreshedContext.endpoint == "https://oss-cn-shanghai.aliyuncs.com")
    #expect(refreshedContext.objectKey == "objects/demo.bin")
    #expect(refreshedContext.partSizeBytes == 64 * 1024 * 1024)
    #expect(refreshedContext.credentialRefreshCount == 1)
    #expect(await session.lastKnownCredentialExpiration() == Date(timeIntervalSince1970: 3_600))
}

@Test func refreshThenContinueUploadSucceeds() async throws {
    let initialClient = RecordingMultipartClient(identifier: "initial")
    let refreshedClient = RecordingMultipartClient(
        identifier: "refreshed",
        uploadPartResult: UploadedPartDescriptor(
            partNumber: 2,
            etag: "\"etag-2\"",
            size: 4,
            lastModified: nil,
            hashCRC64: nil
        ),
        putObjectResult: UploadedPartDescriptor(
            partNumber: 1,
            etag: "\"etag-put\"",
            size: 4,
            lastModified: nil,
            hashCRC64: nil
        ),
        completeResult: "\"etag-complete\"",
        headObjectETagResult: "\"etag-head\""
    )
    let factory = RecordingMultipartClientFactory(clients: [initialClient, refreshedClient])
    let provider = MockGatewayUploadCredentialsProvider(
        responses: [
            makeReissueResponse(
                uploadID: "upload-1",
                credentials: makeUploadCredentials(expireAtUnix: 5_000, tokenSuffix: "second")
            ),
        ]
    )
    let clock = TestOssSessionClock(now: Date(timeIntervalSince1970: 2_000))
    let context = try OssUploadSession.makeUploadContext(
        uploadID: "upload-1",
        credentials: makeUploadCredentials(expireAtUnix: 2_006, tokenSuffix: "first")
    )
    let session = try OssUploadSession(
        context: context,
        refreshPolicy: STSRefreshPolicy(refreshSkew: .seconds(5), requestTimeout: .seconds(2)),
        requestTimeout: .seconds(2),
        retryMaxAttempts: 3,
        clientFactory: factory,
        credentialsProvider: provider,
        clock: clock
    )

    let uploadedPart = try await session.uploadPart(
        multipartUploadID: "multipart-1",
        partNumber: 2,
        body: .data(Data("data".utf8))
    )
    let putObject = try await session.putObject(body: .data(Data("blob".utf8)))
    let completeETag = try await session.completeMultipartUpload(
        multipartUploadID: "multipart-1",
        parts: [uploadedPart]
    )
    let headETag = try await session.headObjectETag()

    #expect(uploadedPart.etag == "\"etag-2\"")
    #expect(putObject.etag == "\"etag-put\"")
    #expect(completeETag == "\"etag-complete\"")
    #expect(headETag == "\"etag-head\"")
    #expect(await provider.requestedUploadIDs() == ["upload-1"])
    #expect(await initialClient.uploadPartCalls().isEmpty)
    #expect(await refreshedClient.uploadPartCalls() == ["multipart-1:2"])
    #expect(await refreshedClient.putObjectCalls() == ["objects/demo.bin:4"])
    #expect(await refreshedClient.completeCalls() == [[2]])
    #expect(await refreshedClient.headObjectCalls() == ["objects/demo.bin"])
}

@Test func dataPlaneRetryClassifierRetriesFiveHundredAnd429() {
    let server500 = OSSDataPlaneErrorMapper.classify(
        OssOperationError.serverFailure(
            statusCode: 500,
            code: "InternalError",
            message: "server exploded",
            requestID: "req-1",
            ec: nil
        )
    )
    #expect(server500.action == .retry)
    #expect(server500.httpStatus == 500)
    #expect(server500.ossCode == "InternalError")

    let throttled = OSSDataPlaneErrorMapper.classify(
        OssOperationError.serverFailure(
            statusCode: 429,
            code: "TooManyRequests",
            message: "slow down",
            requestID: "req-2",
            ec: nil
        )
    )
    #expect(throttled.action == .retry)
    #expect(throttled.httpStatus == 429)
    #expect(throttled.ossCode == "TooManyRequests")
}

@Test func dataPlaneRetryClassifierRefreshesOnSignatureFailures() {
    let expiredToken = OSSDataPlaneErrorMapper.classify(
        OssOperationError.serverFailure(
            statusCode: 403,
            code: "SecurityTokenExpired",
            message: "token expired",
            requestID: "req-3",
            ec: nil
        )
    )
    #expect(expiredToken.action == .refreshCredentials)
    #expect(expiredToken.ossCode == "SecurityTokenExpired")

    let signingDate = OSSDataPlaneErrorMapper.classify(
        OssOperationError.serverFailure(
            statusCode: 403,
            code: "RequestTimeTooSkewed",
            message: "Invalid signing date in Authorization header.",
            requestID: "req-4",
            ec: nil
        )
    )
    #expect(signingDate.action == .refreshCredentials)
    #expect(signingDate.ossCode == "RequestTimeTooSkewed")
}

@Test func dataPlaneRetryClassifierFailsTerminalForFourHundred() {
    let badRequest = OSSDataPlaneErrorMapper.classify(
        OssOperationError.serverFailure(
            statusCode: 400,
            code: "InvalidArgument",
            message: "object key invalid",
            requestID: "req-5",
            ec: nil
        )
    )

    #expect(badRequest.action == .fail)
    #expect(
        OSSDataPlaneErrorMapper.mapToClientError(
            OssOperationError.serverFailure(
                statusCode: 400,
                code: "InvalidArgument",
                message: "object key invalid",
                requestID: "req-5",
                ec: nil
            )
        ) == .ossFailed(httpStatus: 400, ossCode: "InvalidArgument", message: "object key invalid")
    )
}

@Test func dataPlaneRetryExecutorRefreshesThenSucceeds() async throws {
    let sleeper = RecordingDataPlaneSleeper()
    let events = DataPlaneRetryEventRecorder()
    let executor = DataPlaneRetryExecutor(sleeper: sleeper) { event in
        await events.record(event)
    }
    let operation = FailableDataPlaneOperation<String>(results: [
        .failure(OssOperationError.serverFailure(
            statusCode: 403,
            code: "SecurityTokenExpired",
            message: "token expired",
            requestID: "req-6",
            ec: nil
        )),
        .success("ok"),
    ])
    let refreshRecorder = RefreshRecorder()

    let value = try await executor.execute(
        policy: .dataPlane,
        refreshCredentials: {
            await refreshRecorder.record()
        }
    ) {
        try await operation.run()
    }

    #expect(value == "ok")
    #expect(await refreshRecorder.count() == 1)
    #expect(await sleeper.durations().isEmpty)
    #expect(await events.events().map(\.action) == [.refreshCredentials])
}

@Test func dataPlaneRetryExecutorFailsImmediatelyForNonRetriableFourHundred() async {
    let client = ThrowingMultipartClient(error: OssOperationError.serverFailure(
        statusCode: 404,
        code: "NoSuchKey",
        message: "missing object",
        requestID: "req-7",
        ec: nil
    ))
    let factory = RecordingMultipartClientFactory(clients: [client])
    let provider = MockGatewayUploadCredentialsProvider(responses: [])
    let session = try! OssUploadSession(
        context: try! OssUploadSession.makeUploadContext(
            uploadID: "upload-1",
            credentials: makeUploadCredentials(expireAtUnix: 9_999, tokenSuffix: "stable")
        ),
        refreshPolicy: STSRefreshPolicy(refreshSkew: .seconds(5), requestTimeout: .seconds(2)),
        dataPlaneRetryExecutor: DataPlaneRetryExecutor(sleeper: RecordingDataPlaneSleeper()),
        dataPlaneRetryPolicy: .dataPlane,
        requestTimeout: .seconds(2),
        retryMaxAttempts: 3,
        clientFactory: factory,
        credentialsProvider: provider,
        clock: TestOssSessionClock(now: Date(timeIntervalSince1970: 1_000))
    )

    let error = await #expect(throws: DataGatewayClientError.self) {
        try await session.headObjectETag()
    }

    #expect(error == .ossFailed(httpStatus: 404, ossCode: "NoSuchKey", message: "missing object"))
    #expect(await provider.requestedUploadIDs().isEmpty)
}

private actor MockAlibabaOSSSDKClient: AlibabaOSSSDKClientProtocol {
    private let initiateValue: OssInitiateMultipartUploadOutput
    private let uploadPartValue: OssUploadPartOutput
    private let putValue: OssPutObjectOutput
    private let putError: (any Error)?
    private let completeValue: OssCompleteMultipartUploadOutput
    private let listValues: [OssListPartsPage]
    private let headValue: OssHeadObjectOutput

    private var recordedInitiateRequests: [InitiateMultipartUploadRequest] = []
    private var recordedUploadRequests: [UploadPartRequest] = []
    private var recordedPutRequests: [PutObjectRequest] = []
    private var recordedCompleteRequests: [CompleteMultipartUploadRequest] = []
    private var recordedAbortRequests: [AbortMultipartUploadRequest] = []
    private var recordedListRequests: [ListPartsRequest] = []
    private var recordedHeadRequests: [HeadObjectRequest] = []

    init(
        initiateValue: OssInitiateMultipartUploadOutput,
        uploadPartValue: OssUploadPartOutput,
        putValue: OssPutObjectOutput = OssPutObjectOutput(etag: "\"etag-put\""),
        putError: (any Error)? = nil,
        completeValue: OssCompleteMultipartUploadOutput,
        listValues: [OssListPartsPage],
        headValue: OssHeadObjectOutput
    ) {
        self.initiateValue = initiateValue
        self.uploadPartValue = uploadPartValue
        self.putValue = putValue
        self.putError = putError
        self.completeValue = completeValue
        self.listValues = listValues
        self.headValue = headValue
    }

    func initiateMultipartUpload(
        _ request: InitiateMultipartUploadRequest
    ) async throws -> OssInitiateMultipartUploadOutput {
        self.recordedInitiateRequests.append(request)
        return self.initiateValue
    }

    func uploadPart(
        _ request: UploadPartRequest
    ) async throws -> OssUploadPartOutput {
        self.recordedUploadRequests.append(request)
        return self.uploadPartValue
    }

    func putObject(
        _ request: PutObjectRequest
    ) async throws -> OssPutObjectOutput {
        self.recordedPutRequests.append(request)
        if let putError {
            throw putError
        }
        return self.putValue
    }

    func completeMultipartUpload(
        _ request: CompleteMultipartUploadRequest
    ) async throws -> OssCompleteMultipartUploadOutput {
        self.recordedCompleteRequests.append(request)
        return self.completeValue
    }

    func abortMultipartUpload(
        _ request: AbortMultipartUploadRequest
    ) async throws {
        self.recordedAbortRequests.append(request)
    }

    func listPartsPages(
        _ request: ListPartsRequest
    ) async throws -> [OssListPartsPage] {
        self.recordedListRequests.append(request)
        return self.listValues
    }

    func headObject(
        _ request: HeadObjectRequest
    ) async throws -> OssHeadObjectOutput {
        self.recordedHeadRequests.append(request)
        return self.headValue
    }

    func initiateRequests() -> [InitiateMultipartUploadRequest] {
        self.recordedInitiateRequests
    }

    func uploadPartRequests() -> [UploadPartRequest] {
        self.recordedUploadRequests
    }

    func putRequests() -> [PutObjectRequest] {
        self.recordedPutRequests
    }

    func completeRequests() -> [CompleteMultipartUploadRequest] {
        self.recordedCompleteRequests
    }

    func abortRequests() -> [AbortMultipartUploadRequest] {
        self.recordedAbortRequests
    }

    func listRequests() -> [ListPartsRequest] {
        self.recordedListRequests
    }

    func headRequests() -> [HeadObjectRequest] {
        self.recordedHeadRequests
    }
}

private actor RecordingMultipartClient: OssMultipartClientProtocol {
    private let identifier: String
    private let initiateResult: String
    private let uploadPartResult: UploadedPartDescriptor
    private let putObjectResult: UploadedPartDescriptor
    private let completeResult: String
    private let listPartsResult: [UploadedPartDescriptor]
    private let headObjectETagResult: String

    private var recordedUploadPartCalls: [String] = []
    private var recordedPutObjectCalls: [String] = []
    private var recordedCompleteCalls: [[Int]] = []
    private var recordedHeadObjectCalls: [String] = []

    init(
        identifier: String,
        initiateResult: String = "multipart-1",
        uploadPartResult: UploadedPartDescriptor = UploadedPartDescriptor(
            partNumber: 1,
            etag: "\"etag-default\"",
            size: 1,
            lastModified: nil,
            hashCRC64: nil
        ),
        putObjectResult: UploadedPartDescriptor = UploadedPartDescriptor(
            partNumber: 1,
            etag: "\"etag-default\"",
            size: 1,
            lastModified: nil,
            hashCRC64: nil
        ),
        completeResult: String = "\"etag-default\"",
        listPartsResult: [UploadedPartDescriptor] = [],
        headObjectETagResult: String = "\"etag-default\""
    ) {
        self.identifier = identifier
        self.initiateResult = initiateResult
        self.uploadPartResult = uploadPartResult
        self.putObjectResult = putObjectResult
        self.completeResult = completeResult
        self.listPartsResult = listPartsResult
        self.headObjectETagResult = headObjectETagResult
    }

    func initiateMultipartUpload(objectKey: String) async throws -> String {
        _ = self.identifier
        return self.initiateResult
    }

    func uploadPart(
        objectKey: String,
        multipartUploadID: String,
        partNumber: Int,
        body: OssUploadBody
    ) async throws -> UploadedPartDescriptor {
        self.recordedUploadPartCalls.append("\(multipartUploadID):\(partNumber)")
        return UploadedPartDescriptor(
            partNumber: partNumber,
            etag: self.uploadPartResult.etag,
            size: body.sizeBytes,
            lastModified: self.uploadPartResult.lastModified,
            hashCRC64: self.uploadPartResult.hashCRC64
        )
    }

    func putObject(
        objectKey: String,
        body: OssUploadBody
    ) async throws -> UploadedPartDescriptor {
        self.recordedPutObjectCalls.append("\(objectKey):\(body.sizeBytes)")
        return UploadedPartDescriptor(
            partNumber: 1,
            etag: self.putObjectResult.etag,
            size: body.sizeBytes,
            lastModified: self.putObjectResult.lastModified,
            hashCRC64: self.putObjectResult.hashCRC64
        )
    }

    func completeMultipartUpload(
        objectKey: String,
        multipartUploadID: String,
        parts: [UploadedPartDescriptor]
    ) async throws -> String {
        self.recordedCompleteCalls.append(parts.map(\.partNumber))
        return self.completeResult
    }

    func abortMultipartUpload(
        objectKey: String,
        multipartUploadID: String
    ) async throws {}

    func listParts(
        objectKey: String,
        multipartUploadID: String
    ) async throws -> [UploadedPartDescriptor] {
        self.listPartsResult
    }

    func headObjectETag(objectKey: String) async throws -> String {
        self.recordedHeadObjectCalls.append(objectKey)
        return self.headObjectETagResult
    }

    func uploadPartCalls() -> [String] {
        self.recordedUploadPartCalls
    }

    func putObjectCalls() -> [String] {
        self.recordedPutObjectCalls
    }

    func completeCalls() -> [[Int]] {
        self.recordedCompleteCalls
    }

    func headObjectCalls() -> [String] {
        self.recordedHeadObjectCalls
    }
}

private actor ThrowingMultipartClient: OssMultipartClientProtocol {
    private let error: any Error

    init(error: any Error) {
        self.error = error
    }

    func initiateMultipartUpload(objectKey: String) async throws -> String {
        throw self.error
    }

    func uploadPart(
        objectKey: String,
        multipartUploadID: String,
        partNumber: Int,
        body: OssUploadBody
    ) async throws -> UploadedPartDescriptor {
        throw self.error
    }

    func putObject(
        objectKey: String,
        body: OssUploadBody
    ) async throws -> UploadedPartDescriptor {
        throw self.error
    }

    func completeMultipartUpload(
        objectKey: String,
        multipartUploadID: String,
        parts: [UploadedPartDescriptor]
    ) async throws -> String {
        throw self.error
    }

    func abortMultipartUpload(
        objectKey: String,
        multipartUploadID: String
    ) async throws {
        throw self.error
    }

    func listParts(
        objectKey: String,
        multipartUploadID: String
    ) async throws -> [UploadedPartDescriptor] {
        throw self.error
    }

    func headObjectETag(objectKey: String) async throws -> String {
        throw self.error
    }
}

private final class RecordingMultipartClientFactory: OssMultipartClientFactoryProtocol, @unchecked Sendable {
    private let lock = NSLock()
    private var clients: [any OssMultipartClientProtocol]
    private var recordedConfigurations: [OssMultipartClientConfiguration] = []

    init(clients: [any OssMultipartClientProtocol]) {
        self.clients = clients
    }

    func makeMultipartClient(
        configuration: OssMultipartClientConfiguration
    ) throws -> any OssMultipartClientProtocol {
        self.lock.lock()
        defer { self.lock.unlock() }
        self.recordedConfigurations.append(configuration)
        guard !self.clients.isEmpty else {
            throw OssOperationError.invalidConfiguration("missing mock OSS client")
        }
        return self.clients.removeFirst()
    }

    func configurations() -> [OssMultipartClientConfiguration] {
        self.lock.lock()
        defer { self.lock.unlock() }
        return self.recordedConfigurations
    }
}

private actor MockGatewayUploadCredentialsProvider: GatewayUploadCredentialsProvider {
    private var responses: [Archebase_DataGateway_V1_ReissueUploadCredentialsResponse]
    private var requestedIDs: [String] = []

    init(responses: [Archebase_DataGateway_V1_ReissueUploadCredentialsResponse]) {
        self.responses = responses
    }

    func reissueUploadCredentials(
        uploadID: String
    ) async throws -> Archebase_DataGateway_V1_ReissueUploadCredentialsResponse {
        self.requestedIDs.append(uploadID)
        return self.responses.removeFirst()
    }

    func requestedUploadIDs() -> [String] {
        self.requestedIDs
    }
}

private actor TestOssSessionClock: OssSessionClock {
    private let current: Date

    init(now: Date) {
        self.current = now
    }

    func now() async -> Date {
        self.current
    }
}

private actor RecordingDataPlaneSleeper: DataPlaneRetrySleeper {
    private var recordedDurations: [Duration] = []

    func sleep(for duration: Duration) async throws {
        self.recordedDurations.append(duration)
    }

    func durations() -> [Duration] {
        self.recordedDurations
    }
}

private actor DataPlaneRetryEventRecorder {
    private var recordedEvents: [DataPlaneRetryEvent] = []

    func record(_ event: DataPlaneRetryEvent) {
        self.recordedEvents.append(event)
    }

    func events() -> [DataPlaneRetryEvent] {
        self.recordedEvents
    }
}

private actor RefreshRecorder {
    private var refreshCount = 0

    func record() {
        self.refreshCount += 1
    }

    func count() -> Int {
        self.refreshCount
    }
}

private actor FailableDataPlaneOperation<Value: Sendable> {
    enum ResultCase: Sendable {
        case success(Value)
        case failure(any Error)
    }

    private var results: [ResultCase]

    init(results: [ResultCase]) {
        self.results = results
    }

    func run() async throws -> Value {
        let current = self.results.removeFirst()
        switch current {
        case .success(let value):
            return value
        case .failure(let error):
            throw error
        }
    }
}

private func makeConfiguration() -> OssMultipartClientConfiguration {
    OssMultipartClientConfiguration(
        bucket: "bucket-1",
        endpoint: "https://oss-cn-shanghai.aliyuncs.com",
        credentials: OssTemporaryCredentials(
            accessKeyID: "ak",
            accessKeySecret: "sk",
            securityToken: "token",
            expiration: Date(timeIntervalSince1970: 1_000)
        ),
        requestTimeout: .seconds(5),
        retryMaxAttempts: 3
    )
}

private func makeUploadCredentials(
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

private func makeReissueResponse(
    uploadID: String,
    credentials: Archebase_DataGateway_V1_UploadCredentials
) -> Archebase_DataGateway_V1_ReissueUploadCredentialsResponse {
    var response = Archebase_DataGateway_V1_ReissueUploadCredentialsResponse()
    response.logicalUploadID = "logical-1"
    response.uploadID = uploadID
    response.credentials = credentials
    return response
}
