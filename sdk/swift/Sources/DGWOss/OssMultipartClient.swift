import AlibabaCloudOSS
import DGWControlPlane
import DGWProto
import Foundation

package struct OssTemporaryCredentials: Sendable, Equatable {
    package let accessKeyID: String
    package let accessKeySecret: String
    package let securityToken: String?
    package let expiration: Date?

    package init(
        accessKeyID: String,
        accessKeySecret: String,
        securityToken: String?,
        expiration: Date?
    ) {
        self.accessKeyID = accessKeyID
        self.accessKeySecret = accessKeySecret
        self.securityToken = securityToken
        self.expiration = expiration
    }
}

package struct OssUploadContext: Sendable, Equatable {
    package let uploadID: String
    package let bucket: String
    package let endpoint: String
    package let objectKey: String
    package let partSizeBytes: Int64
    package let credentials: OssTemporaryCredentials
    package let credentialRefreshCount: Int32
    package let sessionExpireAt: Date?

    package init(
        uploadID: String,
        bucket: String,
        endpoint: String,
        objectKey: String,
        partSizeBytes: Int64,
        credentials: OssTemporaryCredentials,
        credentialRefreshCount: Int32 = 0,
        sessionExpireAt: Date? = nil
    ) {
        self.uploadID = uploadID
        self.bucket = bucket
        self.endpoint = endpoint
        self.objectKey = objectKey
        self.partSizeBytes = partSizeBytes
        self.credentials = credentials
        self.credentialRefreshCount = credentialRefreshCount
        self.sessionExpireAt = sessionExpireAt
    }
}

package struct OssMultipartClientConfiguration: Sendable, Equatable {
    package let bucket: String
    package let endpoint: String
    package let region: String?
    package let credentials: OssTemporaryCredentials
    package let requestTimeout: Duration?
    package let retryMaxAttempts: Int?
    package let usePathStyle: Bool
    package let enableTLSVerify: Bool

    package init(
        bucket: String,
        endpoint: String,
        region: String? = nil,
        credentials: OssTemporaryCredentials,
        requestTimeout: Duration? = nil,
        retryMaxAttempts: Int? = nil,
        usePathStyle: Bool = false,
        enableTLSVerify: Bool = true
    ) {
        self.bucket = bucket
        self.endpoint = endpoint
        self.region = region
        self.credentials = credentials
        self.requestTimeout = requestTimeout
        self.retryMaxAttempts = retryMaxAttempts
        self.usePathStyle = usePathStyle
        self.enableTLSVerify = enableTLSVerify
    }
}

package struct STSRefreshPolicy: Sendable, Equatable {
    package let refreshSkew: Duration
    package let requestTimeout: Duration

    package init(refreshSkew: Duration, requestTimeout: Duration) {
        self.refreshSkew = refreshSkew
        self.requestTimeout = requestTimeout
    }

    package func shouldRefreshCredentials(expiresAt: Date, now: Date) -> Bool {
        expiresAt.timeIntervalSince(now) <= self.refreshThreshold
    }

    private var refreshThreshold: TimeInterval {
        self.refreshSkew.timeInterval + self.requestTimeout.timeInterval
    }
}

package struct UploadedPartDescriptor: Sendable, Equatable {
    package let partNumber: Int
    package let etag: String
    package let size: Int64?
    package let lastModified: Date?
    package let hashCRC64: String?

    package init(
        partNumber: Int,
        etag: String,
        size: Int64?,
        lastModified: Date?,
        hashCRC64: String?
    ) {
        self.partNumber = partNumber
        self.etag = etag
        self.size = size
        self.lastModified = lastModified
        self.hashCRC64 = hashCRC64
    }
}

package enum OssOperationError: Error, Sendable, Equatable {
    case invalidConfiguration(String)
    case invalidResponse(String)
    case clientFailure(code: String, message: String)
    case serverFailure(statusCode: Int, code: String, message: String, requestID: String, ec: String?)
    case transportFailure(code: Int, message: String)
    case unexpected(String)
}

package enum DataPlaneRetryAction: Sendable, Equatable {
    case retry
    case refreshCredentials
    case fail
}

package struct DataPlaneRetryClassification: Sendable, Equatable {
    package let action: DataPlaneRetryAction
    package let httpStatus: Int?
    package let ossCode: String?
    package let message: String

    package init(
        action: DataPlaneRetryAction,
        httpStatus: Int?,
        ossCode: String?,
        message: String
    ) {
        self.action = action
        self.httpStatus = httpStatus
        self.ossCode = ossCode
        self.message = message
    }
}

package struct DataPlaneRetryEvent: Sendable, Equatable {
    package enum Action: Sendable, Equatable {
        case retry
        case refreshCredentials
    }

    package let attempt: Int
    package let action: Action
    package let delay: Duration?
    package let httpStatus: Int?
    package let ossCode: String?

    package init(
        attempt: Int,
        action: Action,
        delay: Duration?,
        httpStatus: Int?,
        ossCode: String?
    ) {
        self.attempt = attempt
        self.action = action
        self.delay = delay
        self.httpStatus = httpStatus
        self.ossCode = ossCode
    }
}

package protocol DataPlaneRetrySleeper: Sendable {
    func sleep(for duration: Duration) async throws
}

package struct TaskDataPlaneRetrySleeper: DataPlaneRetrySleeper {
    package init() {}

    package func sleep(for duration: Duration) async throws {
        try await Task.sleep(for: duration)
    }
}

package struct OssInitiateMultipartUploadOutput: Sendable, Equatable {
    package let uploadID: String?

    package init(uploadID: String?) {
        self.uploadID = uploadID
    }
}

package struct OssUploadPartOutput: Sendable, Equatable {
    package let etag: String?

    package init(etag: String?) {
        self.etag = etag
    }
}

package struct OssPutObjectOutput: Sendable, Equatable {
    package let etag: String?

    package init(etag: String?) {
        self.etag = etag
    }
}

package struct OssUploadBody: @unchecked Sendable {
    package enum Kind: Sendable, Equatable {
        case data
        case file
        case stream
    }

    fileprivate enum Storage {
        case data(Data)
        case file(URL, sizeBytes: Int64)
        case stream(@Sendable () throws -> InputStream, sizeBytes: Int64)
    }

    fileprivate let storage: Storage
    fileprivate let contentMD5Base64: String?

    package var sizeBytes: Int64 {
        switch self.storage {
        case .data(let data):
            return Int64(data.count)
        case .file(_, let sizeBytes), .stream(_, let sizeBytes):
            return sizeBytes
        }
    }

    package var kind: Kind {
        switch self.storage {
        case .data:
            return .data
        case .file:
            return .file
        case .stream:
            return .stream
        }
    }

    package static func data(_ data: Data) -> OssUploadBody {
        OssUploadBody(storage: .data(data), contentMD5Base64: nil)
    }

    package static func file(
        _ fileURL: URL,
        sizeBytes: Int64,
        contentMD5Base64: String? = nil
    ) -> OssUploadBody {
        OssUploadBody(storage: .file(fileURL, sizeBytes: sizeBytes), contentMD5Base64: contentMD5Base64)
    }

    package static func stream(
        sizeBytes: Int64,
        contentMD5Base64: String? = nil,
        makeStream: @escaping @Sendable () throws -> InputStream
    ) -> OssUploadBody {
        OssUploadBody(storage: .stream(makeStream, sizeBytes: sizeBytes), contentMD5Base64: contentMD5Base64)
    }

    package func byteStream() throws -> ByteStream {
        switch self.storage {
        case .data(let data):
            return .data(data)
        case .file(let fileURL, _):
            return .file(fileURL)
        case .stream(let makeStream, _):
            return try .stream(makeStream())
        }
    }

    fileprivate func addIntegrityHeaders(to request: inout some RequestModel) {
        guard let contentMD5Base64 else {
            return
        }
        request.addHeader("Content-MD5", contentMD5Base64)
    }
}

package struct OssCompleteMultipartUploadOutput: Sendable, Equatable {
    package let etag: String?

    package init(etag: String?) {
        self.etag = etag
    }
}

package struct OssListedPart: Sendable, Equatable {
    package let partNumber: Int?
    package let etag: String?
    package let size: Int64?
    package let lastModified: Date?
    package let hashCRC64: String?

    package init(
        partNumber: Int?,
        etag: String?,
        size: Int64?,
        lastModified: Date?,
        hashCRC64: String?
    ) {
        self.partNumber = partNumber
        self.etag = etag
        self.size = size
        self.lastModified = lastModified
        self.hashCRC64 = hashCRC64
    }
}

package struct OssListPartsPage: Sendable, Equatable {
    package let isTruncated: Bool
    package let nextPartNumberMarker: Int?
    package let parts: [OssListedPart]

    package init(
        isTruncated: Bool,
        nextPartNumberMarker: Int?,
        parts: [OssListedPart]
    ) {
        self.isTruncated = isTruncated
        self.nextPartNumberMarker = nextPartNumberMarker
        self.parts = parts
    }
}

package struct OssHeadObjectOutput: Sendable, Equatable {
    package let etag: String?

    package init(etag: String?) {
        self.etag = etag
    }
}

package protocol AlibabaOSSSDKClientProtocol: Sendable {
    func initiateMultipartUpload(
        _ request: InitiateMultipartUploadRequest
    ) async throws -> OssInitiateMultipartUploadOutput

    func uploadPart(
        _ request: UploadPartRequest
    ) async throws -> OssUploadPartOutput

    func putObject(
        _ request: PutObjectRequest
    ) async throws -> OssPutObjectOutput

    func completeMultipartUpload(
        _ request: CompleteMultipartUploadRequest
    ) async throws -> OssCompleteMultipartUploadOutput

    func abortMultipartUpload(
        _ request: AbortMultipartUploadRequest
    ) async throws

    func listPartsPages(
        _ request: ListPartsRequest
    ) async throws -> [OssListPartsPage]

    func headObject(
        _ request: HeadObjectRequest
    ) async throws -> OssHeadObjectOutput
}

package struct AlibabaOSSSDKClientAdapter: AlibabaOSSSDKClientProtocol {
    private let client: Client

    package init(client: Client) {
        self.client = client
    }

    package func initiateMultipartUpload(
        _ request: InitiateMultipartUploadRequest
    ) async throws -> OssInitiateMultipartUploadOutput {
        let result = try await self.client.initiateMultipartUpload(request)
        return OssInitiateMultipartUploadOutput(uploadID: result.uploadId)
    }

    package func uploadPart(
        _ request: UploadPartRequest
    ) async throws -> OssUploadPartOutput {
        let result = try await self.client.uploadPart(request)
        return OssUploadPartOutput(etag: result.etag)
    }

    package func putObject(
        _ request: PutObjectRequest
    ) async throws -> OssPutObjectOutput {
        let result = try await self.client.putObject(request)
        return OssPutObjectOutput(etag: result.etag)
    }

    package func completeMultipartUpload(
        _ request: CompleteMultipartUploadRequest
    ) async throws -> OssCompleteMultipartUploadOutput {
        let result = try await self.client.completeMultipartUpload(request)
        return OssCompleteMultipartUploadOutput(etag: result.etag)
    }

    package func abortMultipartUpload(
        _ request: AbortMultipartUploadRequest
    ) async throws {
        _ = try await self.client.abortMultipartUpload(request)
    }

    package func listPartsPages(
        _ request: ListPartsRequest
    ) async throws -> [OssListPartsPage] {
        var pages: [OssListPartsPage] = []
        for try await page in self.client.listPartsPaginator(request) {
            pages.append(
                OssListPartsPage(
                    isTruncated: page.isTruncated ?? false,
                    nextPartNumberMarker: page.nextPartNumberMarker,
                    parts: (page.parts ?? []).map {
                        OssListedPart(
                            partNumber: $0.partNumber,
                            etag: $0.etag,
                            size: $0.size.map(Int64.init),
                            lastModified: $0.lastModified,
                            hashCRC64: $0.hashCrc64
                        )
                    }
                )
            )
        }
        return pages
    }

    package func headObject(
        _ request: HeadObjectRequest
    ) async throws -> OssHeadObjectOutput {
        let result = try await self.client.headObject(request)
        return OssHeadObjectOutput(etag: result.etag)
    }
}

package protocol OssMultipartClientFactoryProtocol: Sendable {
    func makeMultipartClient(
        configuration: OssMultipartClientConfiguration
    ) throws -> any OssMultipartClientProtocol
}

package struct AlibabaOSSSDKClientFactory: OssMultipartClientFactoryProtocol {
    package init() {}

    package func makeMultipartClient(
        configuration: OssMultipartClientConfiguration
    ) throws -> any OssMultipartClientProtocol {
        try OssMultipartClient(
            configuration: configuration,
            sdkClient: self.makeSDKClient(configuration: configuration)
        )
    }

    package func makeSDKClient(
        configuration: OssMultipartClientConfiguration
    ) throws -> any AlibabaOSSSDKClientProtocol {
        try Self.validate(configuration)

        let credentials = Credentials(
            accessKeyId: configuration.credentials.accessKeyID,
            accessKeySecret: configuration.credentials.accessKeySecret,
            securityToken: configuration.credentials.securityToken,
            expiration: configuration.credentials.expiration
        )
        let provider = StaticCredentialsProvider(credentials)

        let clientConfiguration = Configuration.default()
            .withEndpoint(configuration.endpoint)
            .withCredentialsProvider(provider)

        if let region = configuration.region?.nilIfBlank {
            clientConfiguration.withRegion(region)
        }
        if let requestTimeout = configuration.requestTimeout {
            clientConfiguration.withTimeoutIntervalForRequest(requestTimeout.timeInterval)
        }
        if let retryMaxAttempts = configuration.retryMaxAttempts {
            clientConfiguration.withRetryMaxAttempts(retryMaxAttempts)
        }
        if configuration.usePathStyle {
            clientConfiguration.withUsePathStyle(true)
        }
        clientConfiguration.withTLSVerify(configuration.enableTLSVerify)

        return AlibabaOSSSDKClientAdapter(client: Client(clientConfiguration))
    }

    package static func validate(_ configuration: OssMultipartClientConfiguration) throws {
        guard configuration.bucket.nilIfBlank != nil else {
            throw OssOperationError.invalidConfiguration("OSS bucket must not be empty")
        }
        guard configuration.endpoint.nilIfBlank != nil else {
            throw OssOperationError.invalidConfiguration("OSS endpoint must not be empty")
        }
        guard configuration.credentials.accessKeyID.nilIfBlank != nil else {
            throw OssOperationError.invalidConfiguration("OSS access key id must not be empty")
        }
        guard configuration.credentials.accessKeySecret.nilIfBlank != nil else {
            throw OssOperationError.invalidConfiguration("OSS access key secret must not be empty")
        }
        if let retryMaxAttempts = configuration.retryMaxAttempts, retryMaxAttempts < 1 {
            throw OssOperationError.invalidConfiguration("OSS retry max attempts must be greater than 0")
        }
    }
}

package enum OSSDataPlaneErrorMapper {
    private static let stsRefreshableServerCodes: Set<String> = [
        "SecurityTokenExpired",
        "InvalidAccessKeyId",
        "SignatureDoesNotMatch",
        "RequestTimeTooSkewed",
    ]

    private static let stsRefreshableClientCodes: Set<String> = [
        "CredentialsFetchError",
        "RemoteSignatureError",
    ]

    package static func mapToClientError(_ error: any Error) -> DataGatewayClientError {
        if let clientError = error as? DataGatewayClientError {
            return clientError
        }
        if let operationError = error as? OssOperationError {
            return self.map(operationError)
        }
        if let serverError = error as? ServerError {
            return self.map(
                OssOperationError.serverFailure(
                    statusCode: serverError.statusCode,
                    code: serverError.code,
                    message: serverError.message,
                    requestID: serverError.requestId,
                    ec: serverError.ec.nilIfBlank
                )
            )
        }
        if let clientError = error as? ClientError {
            return self.map(OssOperationError.clientFailure(code: clientError.code, message: clientError.message))
        }
        if let urlError = error as? URLError {
            return DataGatewayClientError.ossFailed(
                httpStatus: nil,
                ossCode: urlError.code.rawValue.description,
                message: urlError.localizedDescription
            )
        }
        return DataGatewayClientError.ossFailed(
            httpStatus: nil,
            ossCode: nil,
            message: String(describing: error)
        )
    }

    package static func classify(_ error: any Error) -> DataPlaneRetryClassification {
        if let operationError = error as? OssOperationError {
            return self.classify(operationError)
        }
        if let serverError = error as? ServerError {
            return self.classify(
                OssOperationError.serverFailure(
                    statusCode: serverError.statusCode,
                    code: serverError.code,
                    message: serverError.message,
                    requestID: serverError.requestId,
                    ec: serverError.ec.nilIfBlank
                )
            )
        }
        if let clientError = error as? ClientError {
            return self.classify(.clientFailure(code: clientError.code, message: clientError.message))
        }
        if let urlError = error as? URLError {
            return self.classifyURL(urlError)
        }

        return DataPlaneRetryClassification(
            action: .fail,
            httpStatus: nil,
            ossCode: nil,
            message: String(describing: error)
        )
    }

    private static func map(_ error: OssOperationError) -> DataGatewayClientError {
        switch error {
        case .invalidConfiguration(let message):
            return .invalidConfiguration(message)
        case .invalidResponse(let message), .unexpected(let message):
            return DataGatewayClientError.ossFailed(httpStatus: nil, ossCode: nil, message: message)
        case .clientFailure(let code, let message):
            return DataGatewayClientError.ossFailed(httpStatus: nil, ossCode: code, message: message)
        case .serverFailure(let statusCode, let code, let message, _, _):
            return DataGatewayClientError.ossFailed(httpStatus: statusCode, ossCode: code, message: message)
        case .transportFailure(let code, let message):
            return DataGatewayClientError.ossFailed(httpStatus: nil, ossCode: code.description, message: message)
        }
    }

    private static func classify(_ error: OssOperationError) -> DataPlaneRetryClassification {
        switch error {
        case .invalidConfiguration(let message):
            return DataPlaneRetryClassification(action: .fail, httpStatus: nil, ossCode: nil, message: message)
        case .invalidResponse(let message), .unexpected(let message):
            return DataPlaneRetryClassification(action: .fail, httpStatus: nil, ossCode: nil, message: message)
        case .clientFailure(let code, let message):
            if self.stsRefreshableClientCodes.contains(code) {
                return DataPlaneRetryClassification(action: .refreshCredentials, httpStatus: nil, ossCode: code, message: message)
            }
            if Self.isRetriableClientCode(code) {
                return DataPlaneRetryClassification(action: .retry, httpStatus: nil, ossCode: code, message: message)
            }
            return DataPlaneRetryClassification(action: .fail, httpStatus: nil, ossCode: code, message: message)
        case .serverFailure(let statusCode, let code, let message, _, _):
            if self.isRefreshableServerFailure(statusCode: statusCode, code: code, message: message) {
                return DataPlaneRetryClassification(action: .refreshCredentials, httpStatus: statusCode, ossCode: code, message: message)
            }
            if statusCode == 429 || statusCode >= 500 {
                return DataPlaneRetryClassification(action: .retry, httpStatus: statusCode, ossCode: code, message: message)
            }
            if statusCode == 408 {
                return DataPlaneRetryClassification(action: .retry, httpStatus: statusCode, ossCode: code, message: message)
            }
            return DataPlaneRetryClassification(action: .fail, httpStatus: statusCode, ossCode: code, message: message)
        case .transportFailure(let code, let message):
            return DataPlaneRetryClassification(
                action: Self.isRetriableTransportCode(code) ? .retry : .fail,
                httpStatus: nil,
                ossCode: code.description,
                message: message
            )
        }
    }

    private static func classifyURL(_ error: URLError) -> DataPlaneRetryClassification {
        if self.isRetriableURLFailure(error) {
            return DataPlaneRetryClassification(
                action: .retry,
                httpStatus: nil,
                ossCode: error.code.rawValue.description,
                message: error.localizedDescription
            )
        }

        return DataPlaneRetryClassification(
            action: .fail,
            httpStatus: nil,
            ossCode: error.code.rawValue.description,
            message: error.localizedDescription
        )
    }

    private static func isRefreshableServerFailure(statusCode: Int, code: String, message: String) -> Bool {
        if self.stsRefreshableServerCodes.contains(code) {
            return true
        }
        if statusCode == 401 {
            return true
        }
        return message == "Invalid signing date in Authorization header."
    }

    private static func isRetriableClientCode(_ code: String) -> Bool {
        ["CredentialsFetchError", "InconsistentError", "SerdeError", "ResponseError"].contains(code)
    }

    private static func isRetriableURLFailure(_ error: URLError) -> Bool {
        self.isRetriableTransportCode(error.code.rawValue)
    }

    private static func isRetriableTransportCode(_ code: Int) -> Bool {
        let urlErrorCode = URLError.Code(rawValue: code)
        switch urlErrorCode {
        case .timedOut,
            .networkConnectionLost,
            .notConnectedToInternet,
            .cannotConnectToHost,
            .cannotFindHost,
            .dnsLookupFailed:
            return true
        default:
            return false
        }
    }
}

package struct RetryPolicySet: Sendable, Equatable {
    package let controlPlane: RetryPolicy
    package let dataPlane: RetryPolicy

    package init(controlPlane: RetryPolicy, dataPlane: RetryPolicy) {
        self.controlPlane = controlPlane
        self.dataPlane = dataPlane
    }

    package static let `default` = RetryPolicySet(
        controlPlane: .controlPlane,
        dataPlane: .dataPlane
    )
}

package extension RetryPolicy {
    static let dataPlane = RetryPolicy(
        maxAttempts: 8,
        initialBackoff: .seconds(1),
        maxBackoff: .seconds(30)
    )
}

package struct DataPlaneRetryExecutor: Sendable {
    private let sleeper: any DataPlaneRetrySleeper
    private let onEvent: (@Sendable (DataPlaneRetryEvent) async -> Void)?

    package init(
        sleeper: any DataPlaneRetrySleeper = TaskDataPlaneRetrySleeper(),
        onEvent: (@Sendable (DataPlaneRetryEvent) async -> Void)? = nil
    ) {
        self.sleeper = sleeper
        self.onEvent = onEvent
    }

    package func execute<T: Sendable>(
        policy: RetryPolicy = .dataPlane,
        refreshCredentials: @Sendable () async throws -> Void = {},
        operation: @Sendable () async throws -> T
    ) async throws -> T {
        var attempt = 1
        var didRefreshCredentials = false

        while true {
            do {
                return try await operation()
            } catch {
                let classification = OSSDataPlaneErrorMapper.classify(error)

                switch classification.action {
                case .retry where attempt < policy.maxAttempts:
                    let delay = policy.backoff(forAttempt: attempt)
                    if let onEvent {
                        await onEvent(
                            DataPlaneRetryEvent(
                                attempt: attempt,
                                action: .retry,
                                delay: delay,
                                httpStatus: classification.httpStatus,
                                ossCode: classification.ossCode
                            )
                        )
                    }
                    try await self.sleeper.sleep(for: delay)
                    attempt += 1
                case .refreshCredentials where !didRefreshCredentials && attempt < policy.maxAttempts:
                    if let onEvent {
                        await onEvent(
                            DataPlaneRetryEvent(
                                attempt: attempt,
                                action: .refreshCredentials,
                                delay: nil,
                                httpStatus: classification.httpStatus,
                                ossCode: classification.ossCode
                            )
                        )
                    }
                    didRefreshCredentials = true
                    try await refreshCredentials()
                    attempt += 1
                default:
                    throw OSSDataPlaneErrorMapper.mapToClientError(error)
                }
            }
        }
    }
}

package protocol OssMultipartClientProtocol: Sendable {
    func initiateMultipartUpload(objectKey: String) async throws -> String

    func uploadPart(
        objectKey: String,
        multipartUploadID: String,
        partNumber: Int,
        body: OssUploadBody
    ) async throws -> UploadedPartDescriptor

    func putObject(
        objectKey: String,
        body: OssUploadBody
    ) async throws -> UploadedPartDescriptor

    func completeMultipartUpload(
        objectKey: String,
        multipartUploadID: String,
        parts: [UploadedPartDescriptor]
    ) async throws -> String

    func abortMultipartUpload(
        objectKey: String,
        multipartUploadID: String
    ) async throws

    func listParts(
        objectKey: String,
        multipartUploadID: String
    ) async throws -> [UploadedPartDescriptor]

    func headObjectETag(objectKey: String) async throws -> String
}

package struct OssMultipartClient: OssMultipartClientProtocol {
    private let configuration: OssMultipartClientConfiguration
    private let sdkClient: any AlibabaOSSSDKClientProtocol

    package init(
        configuration: OssMultipartClientConfiguration,
        sdkClient: any AlibabaOSSSDKClientProtocol
    ) throws {
        try AlibabaOSSSDKClientFactory.validate(configuration)
        self.configuration = configuration
        self.sdkClient = sdkClient
    }

    package func initiateMultipartUpload(objectKey: String) async throws -> String {
        do {
            let request = InitiateMultipartUploadRequest(
                bucket: self.configuration.bucket,
                key: objectKey
            )
            let result = try await self.sdkClient.initiateMultipartUpload(request)
            guard let uploadID = result.uploadID?.nilIfBlank else {
                throw OssOperationError.invalidResponse("InitiateMultipartUpload response missing uploadId")
            }
            return uploadID
        } catch {
            throw Self.mapError(error)
        }
    }

    package func uploadPart(
        objectKey: String,
        multipartUploadID: String,
        partNumber: Int,
        body: OssUploadBody
    ) async throws -> UploadedPartDescriptor {
        do {
            var request = UploadPartRequest(
                bucket: self.configuration.bucket,
                key: objectKey,
                partNumber: partNumber,
                uploadId: multipartUploadID,
                body: try body.byteStream()
            )
            request.addHeader("Content-Length", body.sizeBytes.description)
            body.addIntegrityHeaders(to: &request)
            let result = try await self.sdkClient.uploadPart(request)
            guard let etag = result.etag?.nilIfBlank else {
                throw OssOperationError.invalidResponse("UploadPart response missing ETag")
            }
            return UploadedPartDescriptor(
                partNumber: partNumber,
                etag: etag,
                size: body.sizeBytes,
                lastModified: nil,
                hashCRC64: nil
            )
        } catch {
            throw Self.mapError(error)
        }
    }

    package func putObject(
        objectKey: String,
        body: OssUploadBody
    ) async throws -> UploadedPartDescriptor {
        do {
            var request = PutObjectRequest(
                bucket: self.configuration.bucket,
                key: objectKey,
                body: try body.byteStream()
            )
            request.addHeader("Content-Length", body.sizeBytes.description)
            body.addIntegrityHeaders(to: &request)
            let result = try await self.sdkClient.putObject(request)
            guard let etag = result.etag?.nilIfBlank else {
                throw OssOperationError.invalidResponse("PutObject response missing ETag")
            }
            return UploadedPartDescriptor(
                partNumber: 1,
                etag: etag,
                size: body.sizeBytes,
                lastModified: nil,
                hashCRC64: nil
            )
        } catch {
            throw Self.mapError(error)
        }
    }

    package func completeMultipartUpload(
        objectKey: String,
        multipartUploadID: String,
        parts: [UploadedPartDescriptor]
    ) async throws -> String {
        do {
            let uploadParts = parts
                .sorted(by: { $0.partNumber < $1.partNumber })
                .map { UploadPart(etag: $0.etag, partNumber: $0.partNumber) }

            let request = CompleteMultipartUploadRequest(
                bucket: self.configuration.bucket,
                key: objectKey,
                uploadId: multipartUploadID,
                completeMultipartUpload: CompleteMultipartUpload(parts: uploadParts)
            )
            let result = try await self.sdkClient.completeMultipartUpload(request)
            guard let etag = result.etag?.nilIfBlank else {
                throw OssOperationError.invalidResponse("CompleteMultipartUpload response missing ETag")
            }
            return etag
        } catch {
            throw Self.mapError(error)
        }
    }

    package func abortMultipartUpload(
        objectKey: String,
        multipartUploadID: String
    ) async throws {
        do {
            let request = AbortMultipartUploadRequest(
                bucket: self.configuration.bucket,
                key: objectKey,
                uploadId: multipartUploadID
            )
            try await self.sdkClient.abortMultipartUpload(request)
        } catch {
            throw Self.mapError(error)
        }
    }

    package func listParts(
        objectKey: String,
        multipartUploadID: String
    ) async throws -> [UploadedPartDescriptor] {
        do {
            let request = ListPartsRequest(
                bucket: self.configuration.bucket,
                key: objectKey,
                uploadId: multipartUploadID
            )
            let pages = try await self.sdkClient.listPartsPages(request)

            var descriptors: [UploadedPartDescriptor] = []
            for page in pages {
                for part in page.parts {
                    descriptors.append(try Self.mapListedPart(part))
                }
            }
            return descriptors.sorted(by: { $0.partNumber < $1.partNumber })
        } catch {
            throw Self.mapError(error)
        }
    }

    package func headObjectETag(objectKey: String) async throws -> String {
        do {
            let request = HeadObjectRequest(
                bucket: self.configuration.bucket,
                key: objectKey
            )
            let result = try await self.sdkClient.headObject(request)
            guard let etag = result.etag?.nilIfBlank else {
                throw OssOperationError.invalidResponse("HeadObject response missing ETag")
            }
            return etag
        } catch {
            throw Self.mapError(error)
        }
    }

    private static func mapListedPart(_ part: OssListedPart) throws -> UploadedPartDescriptor {
        guard let partNumber = part.partNumber else {
            throw OssOperationError.invalidResponse("ListParts response missing part number")
        }
        guard let etag = part.etag?.nilIfBlank else {
            throw OssOperationError.invalidResponse("ListParts response missing part ETag")
        }

        return UploadedPartDescriptor(
            partNumber: partNumber,
            etag: etag,
            size: part.size,
            lastModified: part.lastModified,
            hashCRC64: part.hashCRC64?.nilIfBlank
        )
    }

    private static func mapError(_ error: any Error) -> OssOperationError {
        if let operationError = error as? OssOperationError {
            return operationError
        }
        if let clientError = error as? ClientError {
            return .clientFailure(code: clientError.code, message: clientError.message)
        }
        if let serverError = error as? ServerError {
            return .serverFailure(
                statusCode: serverError.statusCode,
                code: serverError.code,
                message: serverError.message,
                requestID: serverError.requestId,
                ec: serverError.ec.nilIfBlank
            )
        }
        if let urlError = error as? URLError {
            return .transportFailure(code: urlError.code.rawValue, message: urlError.localizedDescription)
        }
        return .unexpected(String(describing: error))
    }
}

package protocol GatewayUploadCredentialsProvider: Sendable {
    func reissueUploadCredentials(
        uploadID: String
    ) async throws -> Archebase_DataGateway_V1_ReissueUploadCredentialsResponse
}

package protocol OssSessionClock: Sendable {
    func now() async -> Date
}

package struct SystemOssSessionClock: OssSessionClock {
    package init() {}

    package func now() async -> Date {
        Date()
    }
}

package actor OssUploadSession {
    private let refreshPolicy: STSRefreshPolicy
    private let dataPlaneRetryExecutor: DataPlaneRetryExecutor
    private let dataPlaneRetryPolicy: RetryPolicy
    private let requestTimeout: Duration?
    private let retryMaxAttempts: Int?
    private let usePathStyle: Bool
    private let enableTLSVerify: Bool
    private let regionResolver: @Sendable (String) -> String?
    private let clientFactory: any OssMultipartClientFactoryProtocol
    private let credentialsProvider: any GatewayUploadCredentialsProvider
    private let clock: any OssSessionClock

    private var context: OssUploadContext
    private var client: any OssMultipartClientProtocol
    private var lastKnownSTSExpireAt: Date?

    package init(
        context: OssUploadContext,
        refreshPolicy: STSRefreshPolicy,
        dataPlaneRetryExecutor: DataPlaneRetryExecutor = DataPlaneRetryExecutor(),
        dataPlaneRetryPolicy: RetryPolicy = .dataPlane,
        requestTimeout: Duration? = nil,
        retryMaxAttempts: Int? = nil,
        usePathStyle: Bool = false,
        enableTLSVerify: Bool = true,
        regionResolver: @escaping @Sendable (String) -> String? = defaultOssRegionResolver,
        clientFactory: any OssMultipartClientFactoryProtocol,
        credentialsProvider: any GatewayUploadCredentialsProvider,
        clock: any OssSessionClock = SystemOssSessionClock()
    ) throws {
        self.context = context
        self.refreshPolicy = refreshPolicy
        self.dataPlaneRetryExecutor = dataPlaneRetryExecutor
        self.dataPlaneRetryPolicy = dataPlaneRetryPolicy
        self.requestTimeout = requestTimeout
        self.retryMaxAttempts = retryMaxAttempts
        self.usePathStyle = usePathStyle
        self.enableTLSVerify = enableTLSVerify
        self.regionResolver = regionResolver
        self.clientFactory = clientFactory
        self.credentialsProvider = credentialsProvider
        self.clock = clock
        self.lastKnownSTSExpireAt = context.credentials.expiration
        self.client = try clientFactory.makeMultipartClient(
            configuration: Self.makeConfiguration(
                context: context,
                requestTimeout: requestTimeout,
                retryMaxAttempts: retryMaxAttempts,
                usePathStyle: usePathStyle,
                enableTLSVerify: enableTLSVerify,
                regionResolver: regionResolver
            )
        )
    }

    package func ensureFreshClientIfNeeded() async throws {
        guard let expiresAt = self.context.credentials.expiration else {
            return
        }

        let now = await self.clock.now()
        if self.refreshPolicy.shouldRefreshCredentials(expiresAt: expiresAt, now: now) {
            try await self.refreshCredentials()
        }
    }

    package func ensureFreshCredentialsIfNeeded() async throws -> Bool {
        guard let expiresAt = self.context.credentials.expiration else {
            return false
        }

        let now = await self.clock.now()
        guard self.refreshPolicy.shouldRefreshCredentials(expiresAt: expiresAt, now: now) else {
            return false
        }

        try await self.refreshCredentials()
        return true
    }

    package func initiateMultipartUpload() async throws -> String {
        try await self.executeDataPlaneOperation {
            try await self.performInitiateMultipartUpload()
        }
    }

    package func uploadPart(
        multipartUploadID: String,
        partNumber: Int,
        body: OssUploadBody
    ) async throws -> UploadedPartDescriptor {
        try await self.executeDataPlaneOperation {
            try await self.performUploadPart(
                multipartUploadID: multipartUploadID,
                partNumber: partNumber,
                body: body
            )
        }
    }

    package func putObject(body: OssUploadBody) async throws -> UploadedPartDescriptor {
        try await self.executeDataPlaneOperation {
            try await self.performPutObject(body: body)
        }
    }

    package func completeMultipartUpload(
        multipartUploadID: String,
        parts: [UploadedPartDescriptor]
    ) async throws -> String {
        try await self.executeDataPlaneOperation {
            try await self.performCompleteMultipartUpload(
                multipartUploadID: multipartUploadID,
                parts: parts
            )
        }
    }

    package func abortMultipartUpload(multipartUploadID: String) async throws {
        _ = try await self.executeDataPlaneOperation {
            try await self.performAbortMultipartUpload(multipartUploadID: multipartUploadID)
            return true
        }
    }

    package func listParts(multipartUploadID: String) async throws -> [UploadedPartDescriptor] {
        try await self.executeDataPlaneOperation {
            try await self.performListParts(multipartUploadID: multipartUploadID)
        }
    }

    package func headObjectETag() async throws -> String {
        try await self.executeDataPlaneOperation {
            try await self.performHeadObjectETag()
        }
    }

    package func uploadContext() -> OssUploadContext {
        self.context
    }

    package func lastKnownCredentialExpiration() -> Date? {
        self.lastKnownSTSExpireAt
    }

    package func forceRefreshCredentials() async throws {
        try await self.refreshCredentials()
    }

    private func executeDataPlaneOperation<T: Sendable>(
        _ operation: @escaping @Sendable () async throws -> T
    ) async throws -> T {
        do {
            try await self.ensureFreshClientIfNeeded()
            return try await self.dataPlaneRetryExecutor.execute(
                policy: self.dataPlaneRetryPolicy,
                refreshCredentials: {
                    try await self.refreshCredentials()
                },
                operation: operation
            )
        } catch {
            throw OSSDataPlaneErrorMapper.mapToClientError(error)
        }
    }

    private func performInitiateMultipartUpload() async throws -> String {
        try await self.client.initiateMultipartUpload(objectKey: self.context.objectKey)
    }

    private func performUploadPart(
        multipartUploadID: String,
        partNumber: Int,
        body: OssUploadBody
    ) async throws -> UploadedPartDescriptor {
        try await self.client.uploadPart(
            objectKey: self.context.objectKey,
            multipartUploadID: multipartUploadID,
            partNumber: partNumber,
            body: body
        )
    }

    private func performPutObject(body: OssUploadBody) async throws -> UploadedPartDescriptor {
        try await self.client.putObject(
            objectKey: self.context.objectKey,
            body: body
        )
    }

    private func performCompleteMultipartUpload(
        multipartUploadID: String,
        parts: [UploadedPartDescriptor]
    ) async throws -> String {
        try await self.client.completeMultipartUpload(
            objectKey: self.context.objectKey,
            multipartUploadID: multipartUploadID,
            parts: parts
        )
    }

    private func performAbortMultipartUpload(multipartUploadID: String) async throws {
        try await self.client.abortMultipartUpload(
            objectKey: self.context.objectKey,
            multipartUploadID: multipartUploadID
        )
    }

    private func performListParts(multipartUploadID: String) async throws -> [UploadedPartDescriptor] {
        try await self.client.listParts(
            objectKey: self.context.objectKey,
            multipartUploadID: multipartUploadID
        )
    }

    private func performHeadObjectETag() async throws -> String {
        try await self.client.headObjectETag(objectKey: self.context.objectKey)
    }

    private func refreshCredentials() async throws {
        let response = try await self.credentialsProvider.reissueUploadCredentials(uploadID: self.context.uploadID)
        guard response.hasCredentials else {
            throw OssOperationError.invalidResponse("ReissueUploadCredentials response missing credentials")
        }

        let refreshedContext = try Self.merge(context: self.context, response: response)
        self.client = try self.clientFactory.makeMultipartClient(
            configuration: Self.makeConfiguration(
                context: refreshedContext,
                requestTimeout: self.requestTimeout,
                retryMaxAttempts: self.retryMaxAttempts,
                usePathStyle: self.usePathStyle,
                enableTLSVerify: self.enableTLSVerify,
                regionResolver: self.regionResolver
            )
        )
        self.context = refreshedContext
        self.lastKnownSTSExpireAt = refreshedContext.credentials.expiration
    }

    private static func merge(
        context: OssUploadContext,
        response: Archebase_DataGateway_V1_ReissueUploadCredentialsResponse
    ) throws -> OssUploadContext {
        let credentials = response.credentials
        guard response.uploadID == context.uploadID else {
            throw OssOperationError.invalidResponse("ReissueUploadCredentials returned a different uploadID")
        }
        guard credentials.bucket == context.bucket else {
            throw OssOperationError.invalidResponse("ReissueUploadCredentials returned a different bucket")
        }
        guard credentials.endpoint == context.endpoint else {
            throw OssOperationError.invalidResponse("ReissueUploadCredentials returned a different endpoint")
        }
        guard credentials.objectKey == context.objectKey else {
            throw OssOperationError.invalidResponse("ReissueUploadCredentials returned a different objectKey")
        }
        guard credentials.partSizeBytes == context.partSizeBytes else {
            throw OssOperationError.invalidResponse("ReissueUploadCredentials returned a different partSizeBytes")
        }

        return OssUploadContext(
            uploadID: context.uploadID,
            bucket: context.bucket,
            endpoint: context.endpoint,
            objectKey: context.objectKey,
            partSizeBytes: context.partSizeBytes,
            credentials: try Self.makeTemporaryCredentials(from: credentials),
            credentialRefreshCount: context.credentialRefreshCount + 1,
            sessionExpireAt: context.sessionExpireAt
        )
    }

    private static func makeConfiguration(
        context: OssUploadContext,
        requestTimeout: Duration?,
        retryMaxAttempts: Int?,
        usePathStyle: Bool,
        enableTLSVerify: Bool,
        regionResolver: @Sendable (String) -> String?
    ) -> OssMultipartClientConfiguration {
        OssMultipartClientConfiguration(
            bucket: context.bucket,
            endpoint: context.endpoint,
            region: regionResolver(context.endpoint),
            credentials: context.credentials,
            requestTimeout: requestTimeout,
            retryMaxAttempts: retryMaxAttempts,
            usePathStyle: usePathStyle,
            enableTLSVerify: enableTLSVerify
        )
    }

    package static func makeUploadContext(
        uploadID: String,
        credentials: Archebase_DataGateway_V1_UploadCredentials,
        sessionExpireAtUnix: Int64? = nil,
        credentialRefreshCount: Int32 = 0
    ) throws -> OssUploadContext {
        let temporaryCredentials = try Self.makeTemporaryCredentials(from: credentials)
        return OssUploadContext(
            uploadID: uploadID,
            bucket: credentials.bucket,
            endpoint: credentials.endpoint,
            objectKey: credentials.objectKey,
            partSizeBytes: credentials.partSizeBytes,
            credentials: temporaryCredentials,
            credentialRefreshCount: credentialRefreshCount,
            sessionExpireAt: sessionExpireAtUnix.flatMap { Self.makeDate(fromUnix: $0) }
        )
    }

    private static func makeTemporaryCredentials(
        from credentials: Archebase_DataGateway_V1_UploadCredentials
    ) throws -> OssTemporaryCredentials {
        guard let accessKeyID = credentials.stsAccessKeyID.nilIfBlank else {
            throw OssOperationError.invalidResponse("UploadCredentials missing sts_access_key_id")
        }
        guard let accessKeySecret = credentials.stsAccessKeySecret.nilIfBlank else {
            throw OssOperationError.invalidResponse("UploadCredentials missing sts_access_key_secret")
        }
        guard let securityToken = credentials.stsSecurityToken.nilIfBlank else {
            throw OssOperationError.invalidResponse("UploadCredentials missing sts_security_token")
        }
        guard credentials.partSizeBytes > 0 else {
            throw OssOperationError.invalidResponse("UploadCredentials missing part_size_bytes")
        }
        guard let expiration = Self.makeDate(fromUnix: credentials.stsExpireAtUnix) else {
            throw OssOperationError.invalidResponse("UploadCredentials missing sts_expire_at_unix")
        }

        return OssTemporaryCredentials(
            accessKeyID: accessKeyID,
            accessKeySecret: accessKeySecret,
            securityToken: securityToken,
            expiration: expiration
        )
    }

    private static func makeDate(fromUnix unix: Int64) -> Date? {
        guard unix > 0 else {
            return nil
        }
        return Date(timeIntervalSince1970: TimeInterval(unix))
    }
}

private func defaultOssRegionResolver(endpoint: String) -> String? {
    guard let host = URL(string: endpoint)?.host ?? URL(string: "https://\(endpoint)")?.host else {
        return nil
    }

    let segments = host.split(separator: ".")
    guard let first = segments.first else {
        return nil
    }

    if first.hasPrefix("oss-") {
        return String(first.dropFirst(4))
    }
    return nil
}

private extension Duration {
    var timeInterval: TimeInterval {
        let components = self.components
        return Double(components.seconds) + Double(components.attoseconds) / 1_000_000_000_000_000_000
    }
}

private extension String {
    var nilIfBlank: String? {
        let trimmed = self.trimmingCharacters(in: .whitespacesAndNewlines)
        return trimmed.isEmpty ? nil : trimmed
    }
}
