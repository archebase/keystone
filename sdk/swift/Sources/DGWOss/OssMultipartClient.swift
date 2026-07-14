// SPDX-FileCopyrightText: 2026 ArcheBase
// SPDX-License-Identifier: MulanPSL-2.0

import Crypto
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
    package let backend: String
    package let region: String
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
        sessionExpireAt: Date? = nil,
        backend: String = "volcengine_tos",
        region: String = "cn-beijing"
    ) {
        self.uploadID = uploadID
        self.bucket = bucket
        self.endpoint = endpoint
        self.objectKey = objectKey
        self.backend = backend
        self.region = region
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

package protocol RequestModel {
    var headers: [String: String] { get set }
    mutating func addHeader(_ name: String, _ value: String)
}

package extension RequestModel {
    mutating func addHeader(_ name: String, _ value: String) {
        self.headers[name] = value
    }
}

package enum ByteStream: @unchecked Sendable {
    case data(Data)
    case file(URL)
    case stream(InputStream)

    fileprivate func materialize() throws -> Data {
        switch self {
        case .data(let data):
            return data
        case .file(let url):
            return try Data(contentsOf: url, options: .mappedIfSafe)
        case .stream(let stream):
            stream.open()
            defer { stream.close() }
            var result = Data()
            var buffer = [UInt8](repeating: 0, count: 64 * 1024)
            while stream.hasBytesAvailable {
                let count = stream.read(&buffer, maxLength: buffer.count)
                if count < 0 {
                    throw stream.streamError ?? OssOperationError.unexpected("failed to read upload stream")
                }
                if count == 0 {
                    break
                }
                result.append(contentsOf: buffer.prefix(count))
            }
            return result
        }
    }
}

package struct InitiateMultipartUploadRequest: Sendable, RequestModel {
    package let bucket: String
    package let key: String
    package var headers: [String: String] = [:]
}

package struct UploadPartRequest: @unchecked Sendable, RequestModel {
    package let bucket: String
    package let key: String
    package let partNumber: Int
    package let uploadId: String
    package let body: ByteStream
    package var headers: [String: String] = [:]
}

package struct PutObjectRequest: @unchecked Sendable, RequestModel {
    package let bucket: String
    package let key: String
    package let body: ByteStream
    package var headers: [String: String] = [:]
}

package struct UploadPart: Sendable, Equatable {
    package let etag: String
    package let partNumber: Int
}

package struct CompleteMultipartUpload: Sendable, Equatable {
    package let parts: [UploadPart]
}

package struct CompleteMultipartUploadRequest: Sendable, RequestModel {
    package let bucket: String
    package let key: String
    package let uploadId: String
    package let completeMultipartUpload: CompleteMultipartUpload
    package var headers: [String: String] = [:]
}

package struct AbortMultipartUploadRequest: Sendable, RequestModel {
    package let bucket: String
    package let key: String
    package let uploadId: String
    package var headers: [String: String] = [:]
}

package struct ListPartsRequest: Sendable, RequestModel {
    package let bucket: String
    package let key: String
    package let uploadId: String
    package var headers: [String: String] = [:]
}

package struct HeadObjectRequest: Sendable, RequestModel {
    package let bucket: String
    package let key: String
    package var headers: [String: String] = [:]
}

package struct ClientError: Error, Sendable {
    package let code: String
    package let message: String
}

package struct ServerError: Error, Sendable {
    package let statusCode: Int
    package let code: String
    package let message: String
    package let requestId: String
    package let ec: String
}

package protocol TOSHTTPClientProtocol: Sendable {
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

package struct TOSHTTPClientAdapter: TOSHTTPClientProtocol {
    private let configuration: OssMultipartClientConfiguration
    private let session: URLSession

    package init(configuration: OssMultipartClientConfiguration, session: URLSession = .shared) {
        self.configuration = configuration
        self.session = session
    }

    package func initiateMultipartUpload(
        _ request: InitiateMultipartUploadRequest
    ) async throws -> OssInitiateMultipartUploadOutput {
        let (data, _) = try await self.send(method: "POST", key: request.key, query: ["uploads": ""], headers: request.headers)
        let values = Self.responseValues(data)
        return OssInitiateMultipartUploadOutput(uploadID: values["UploadId"] ?? values["UploadID"])
    }

    package func uploadPart(
        _ request: UploadPartRequest
    ) async throws -> OssUploadPartOutput {
        let (_, response) = try await self.send(
            method: "PUT",
            key: request.key,
            query: ["partNumber": String(request.partNumber), "uploadId": request.uploadId],
            headers: request.headers,
            body: try request.body.materialize()
        )
        return OssUploadPartOutput(etag: response.value(forHTTPHeaderField: "ETag"))
    }

    package func putObject(
        _ request: PutObjectRequest
    ) async throws -> OssPutObjectOutput {
        let (_, response) = try await self.send(
            method: "PUT",
            key: request.key,
            query: [:],
            headers: request.headers,
            body: try request.body.materialize()
        )
        return OssPutObjectOutput(etag: response.value(forHTTPHeaderField: "ETag"))
    }

    package func completeMultipartUpload(
        _ request: CompleteMultipartUploadRequest
    ) async throws -> OssCompleteMultipartUploadOutput {
        let body = Self.completeBody(request.completeMultipartUpload.parts)
        let (data, response) = try await self.send(
            method: "POST",
            key: request.key,
            query: ["uploadId": request.uploadId],
            headers: ["Content-Type": "application/json"],
            body: body
        )
        return OssCompleteMultipartUploadOutput(
            etag: response.value(forHTTPHeaderField: "ETag") ?? Self.responseValues(data)["ETag"]
        )
    }

    package func abortMultipartUpload(
        _ request: AbortMultipartUploadRequest
    ) async throws {
        _ = try await self.send(method: "DELETE", key: request.key, query: ["uploadId": request.uploadId], headers: request.headers)
    }

    package func listPartsPages(
        _ request: ListPartsRequest
    ) async throws -> [OssListPartsPage] {
        var pages: [OssListPartsPage] = []
        var marker: String?
        repeat {
            var query = ["uploadId": request.uploadId]
            if let marker { query["part-number-marker"] = marker }
            let (data, _) = try await self.send(method: "GET", key: request.key, query: query, headers: request.headers)
            let parsed = TOSListPartsXMLParser.parse(data)
            pages.append(parsed.page)
            marker = parsed.page.isTruncated ? parsed.nextMarker : nil
        } while marker != nil
        return pages
    }

    package func headObject(
        _ request: HeadObjectRequest
    ) async throws -> OssHeadObjectOutput {
        let (_, response) = try await self.send(method: "HEAD", key: request.key, query: [:], headers: request.headers)
        return OssHeadObjectOutput(etag: response.value(forHTTPHeaderField: "ETag"))
    }

    private func send(
        method: String,
        key: String,
        query: [String: String],
        headers: [String: String],
        body: Data = Data()
    ) async throws -> (Data, HTTPURLResponse) {
        let url = try self.objectURL(key: key, query: query)
        var request = URLRequest(url: url)
        request.httpMethod = method
        request.httpBody = body.isEmpty ? nil : body
        if let timeout = self.configuration.requestTimeout?.timeInterval {
            request.timeoutInterval = timeout
        }
        headers.forEach { request.setValue($1, forHTTPHeaderField: $0) }
        try TOSV4Signer.sign(
            request: &request,
            region: self.configuration.region ?? "",
            credentials: self.configuration.credentials,
            payload: body
        )

        let (data, rawResponse) = try await self.session.data(for: request)
        guard let response = rawResponse as? HTTPURLResponse else {
            throw OssOperationError.invalidResponse("TOS response was not HTTP")
        }
        guard (200 ..< 300).contains(response.statusCode) else {
            let values = Self.responseValues(data)
            throw ServerError(
                statusCode: response.statusCode,
                code: values["Code"] ?? "TOSHTTPError",
                message: values["Message"] ?? HTTPURLResponse.localizedString(forStatusCode: response.statusCode),
                requestId: values["RequestId"] ?? response.value(forHTTPHeaderField: "x-tos-request-id") ?? "",
                ec: values["EC"] ?? ""
            )
        }
        return (data, response)
    }

    private func objectURL(key: String, query: [String: String]) throws -> URL {
        guard var components = URLComponents(string: self.configuration.endpoint), let host = components.host else {
            throw OssOperationError.invalidConfiguration("TOS endpoint is invalid")
        }
        let encodedKey = key.split(separator: "/", omittingEmptySubsequences: false)
            .map { String($0).addingPercentEncoding(withAllowedCharacters: .tosPathSegment) ?? String($0) }
            .joined(separator: "/")
        if self.configuration.usePathStyle {
            components.percentEncodedPath = "/\(self.configuration.bucket)/\(encodedKey)"
        } else {
            components.host = "\(self.configuration.bucket).\(host)"
            components.percentEncodedPath = "/\(encodedKey)"
        }
        components.percentEncodedQuery = query.sorted(by: { $0.key < $1.key }).map {
            "\($0.key.tosQueryEncoded)=\($0.value.tosQueryEncoded)"
        }.joined(separator: "&")
        guard let url = components.url else {
            throw OssOperationError.invalidConfiguration("TOS object URL is invalid")
        }
        return url
    }

    package static func completeBody(_ parts: [UploadPart]) -> Data {
        let payload = TOSCompleteMultipartUploadPayload(
            Parts: parts
                .sorted(by: { $0.partNumber < $1.partNumber })
                .map { TOSCompleteMultipartUploadPart(PartNumber: $0.partNumber, ETag: $0.etag) }
        )
        return (try? JSONEncoder().encode(payload)) ?? Data(#"{"Parts":[]}"#.utf8)
    }

    package static func responseValues(_ data: Data) -> [String: String] {
        let jsonValues = (try? JSONSerialization.jsonObject(with: data)) as? [String: Any]
        if let jsonValues {
            return jsonValues.reduce(into: [:]) { result, entry in
                if let value = entry.value as? String {
                    result[entry.key] = value
                } else if let value = entry.value as? CustomStringConvertible {
                    result[entry.key] = value.description
                }
            }
        }
        return TOSFlatXMLParser.parse(data)
    }
}

package protocol OssMultipartClientFactoryProtocol: Sendable {
    func makeMultipartClient(
        configuration: OssMultipartClientConfiguration
    ) throws -> any OssMultipartClientProtocol
}

package struct TOSHTTPClientFactory: OssMultipartClientFactoryProtocol {
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
    ) throws -> any TOSHTTPClientProtocol {
        try Self.validate(configuration)

        return TOSHTTPClientAdapter(configuration: configuration)
    }

    package static func validate(_ configuration: OssMultipartClientConfiguration) throws {
        guard configuration.bucket.nilIfBlank != nil else {
            throw OssOperationError.invalidConfiguration("TOS bucket must not be empty")
        }
        guard configuration.endpoint.nilIfBlank != nil else {
            throw OssOperationError.invalidConfiguration("TOS endpoint must not be empty")
        }
        guard URL(string: configuration.endpoint)?.scheme?.lowercased() == "https" else {
            throw OssOperationError.invalidConfiguration("TOS endpoint must use https")
        }
        guard configuration.region?.nilIfBlank != nil else {
            throw OssOperationError.invalidConfiguration("TOS region must not be empty")
        }
        guard configuration.credentials.accessKeyID.nilIfBlank != nil else {
            throw OssOperationError.invalidConfiguration("TOS access key id must not be empty")
        }
        guard configuration.credentials.accessKeySecret.nilIfBlank != nil else {
            throw OssOperationError.invalidConfiguration("TOS access key secret must not be empty")
        }
        guard configuration.credentials.securityToken?.nilIfBlank != nil else {
            throw OssOperationError.invalidConfiguration("TOS security token must not be empty")
        }
        guard configuration.enableTLSVerify else {
            throw OssOperationError.invalidConfiguration("TOS TLS verification cannot be disabled")
        }
        if let retryMaxAttempts = configuration.retryMaxAttempts, retryMaxAttempts < 1 {
            throw OssOperationError.invalidConfiguration("TOS retry max attempts must be greater than 0")
        }
    }
}

package enum TOSV4Signer {
    private static let algorithm = "TOS4-HMAC-SHA256"
    private static let service = "tos"

    package static func sign(
        request: inout URLRequest,
        region: String,
        credentials: OssTemporaryCredentials,
        payload: Data,
        now: Date = Date()
    ) throws {
        guard let host = request.url?.host, !region.isEmpty else {
            throw OssOperationError.invalidConfiguration("TOS signing requires host and region")
        }
        let date = self.timestamp(now)
        let shortDate = String(date.prefix(8))
        let payloadHash = self.sha256Hex(payload)
        request.setValue(host, forHTTPHeaderField: "Host")
        request.setValue(date, forHTTPHeaderField: "x-tos-date")
        request.setValue(payloadHash, forHTTPHeaderField: "x-tos-content-sha256")
        if let token = credentials.securityToken?.nilIfBlank {
            request.setValue(token, forHTTPHeaderField: "x-tos-security-token")
        }

        let signedHeaderNames = ["host", "x-tos-content-sha256", "x-tos-date", "x-tos-security-token"]
            .filter { request.value(forHTTPHeaderField: $0) != nil }
            .sorted()
        let canonicalHeaders = signedHeaderNames.map {
            "\($0):\((request.value(forHTTPHeaderField: $0) ?? "").trimmingCharacters(in: .whitespacesAndNewlines))\n"
        }.joined()
        let signedHeaders = signedHeaderNames.joined(separator: ";")
        let canonicalRequest = [
            request.httpMethod ?? "GET",
            request.url.flatMap { URLComponents(url: $0, resolvingAgainstBaseURL: false)?.percentEncodedPath }?.nonEmpty ?? "/",
            request.url.flatMap { URLComponents(url: $0, resolvingAgainstBaseURL: false)?.percentEncodedQuery } ?? "",
            canonicalHeaders,
            signedHeaders,
            payloadHash,
        ].joined(separator: "\n")
        let scope = "\(shortDate)/\(region)/\(self.service)/request"
        let stringToSign = [
            self.algorithm,
            date,
            scope,
            self.sha256Hex(Data(canonicalRequest.utf8)),
        ].joined(separator: "\n")

        let dateKey = self.hmac(Data(shortDate.utf8), key: Data(credentials.accessKeySecret.utf8))
        let regionKey = self.hmac(Data(region.utf8), key: dateKey)
        let serviceKey = self.hmac(Data(self.service.utf8), key: regionKey)
        let signingKey = self.hmac(Data("request".utf8), key: serviceKey)
        let signature = self.hmac(Data(stringToSign.utf8), key: signingKey).hexString
        request.setValue(
            "\(self.algorithm) Credential=\(credentials.accessKeyID)/\(scope), SignedHeaders=\(signedHeaders), Signature=\(signature)",
            forHTTPHeaderField: "Authorization"
        )
    }

    private static func timestamp(_ date: Date) -> String {
        let formatter = DateFormatter()
        formatter.calendar = Calendar(identifier: .gregorian)
        formatter.locale = Locale(identifier: "en_US_POSIX")
        formatter.timeZone = TimeZone(secondsFromGMT: 0)
        formatter.dateFormat = "yyyyMMdd'T'HHmmss'Z'"
        return formatter.string(from: date)
    }

    private static func sha256Hex(_ data: Data) -> String {
        Data(SHA256.hash(data: data)).hexString
    }

    private static func hmac(_ data: Data, key: Data) -> Data {
        Data(HMAC<SHA256>.authenticationCode(for: data, using: SymmetricKey(data: key)))
    }
}

private final class TOSFlatXMLParser: NSObject, XMLParserDelegate {
    private var currentElement = ""
    private var buffer = ""
    private var values: [String: String] = [:]

    static func parse(_ data: Data) -> [String: String] {
        let delegate = TOSFlatXMLParser()
        let parser = XMLParser(data: data)
        parser.delegate = delegate
        _ = parser.parse()
        return delegate.values
    }

    func parser(_ parser: XMLParser, didStartElement elementName: String, namespaceURI: String?, qualifiedName: String?, attributes attributeDict: [String: String] = [:]) {
        self.currentElement = elementName
        self.buffer = ""
    }

    func parser(_ parser: XMLParser, foundCharacters string: String) {
        self.buffer += string
    }

    func parser(_ parser: XMLParser, didEndElement elementName: String, namespaceURI: String?, qualifiedName: String?) {
        let value = self.buffer.trimmingCharacters(in: .whitespacesAndNewlines)
        if !value.isEmpty {
            self.values[elementName] = value
        }
        self.currentElement = ""
        self.buffer = ""
    }
}

private final class TOSListPartsXMLParser: NSObject, XMLParserDelegate {
    private var currentElement = ""
    private var buffer = ""
    private var inPart = false
    private var currentPart: [String: String] = [:]
    private var parts: [OssListedPart] = []
    private var values: [String: String] = [:]

    static func parse(_ data: Data) -> (page: OssListPartsPage, nextMarker: String?) {
        let delegate = TOSListPartsXMLParser()
        let parser = XMLParser(data: data)
        parser.delegate = delegate
        _ = parser.parse()
        let truncated = delegate.values["IsTruncated"]?.lowercased() == "true"
        return (
            OssListPartsPage(
                isTruncated: truncated,
                nextPartNumberMarker: delegate.values["NextPartNumberMarker"].flatMap(Int.init),
                parts: delegate.parts
            ),
            delegate.values["NextPartNumberMarker"]
        )
    }

    func parser(_ parser: XMLParser, didStartElement elementName: String, namespaceURI: String?, qualifiedName: String?, attributes attributeDict: [String: String] = [:]) {
        self.currentElement = elementName
        self.buffer = ""
        if elementName == "Part" {
            self.inPart = true
            self.currentPart = [:]
        }
    }

    func parser(_ parser: XMLParser, foundCharacters string: String) {
        self.buffer += string
    }

    func parser(_ parser: XMLParser, didEndElement elementName: String, namespaceURI: String?, qualifiedName: String?) {
        let value = self.buffer.trimmingCharacters(in: .whitespacesAndNewlines)
        if elementName == "Part" {
            self.parts.append(
                OssListedPart(
                    partNumber: self.currentPart["PartNumber"].flatMap(Int.init),
                    etag: self.currentPart["ETag"],
                    size: self.currentPart["Size"].flatMap(Int64.init),
                    lastModified: self.currentPart["LastModified"].flatMap { ISO8601DateFormatter().date(from: $0) },
                    hashCRC64: self.currentPart["HashCrc64ecma"]
                )
            )
            self.inPart = false
        } else if !value.isEmpty {
            if self.inPart {
                self.currentPart[elementName] = value
            } else {
                self.values[elementName] = value
            }
        }
        self.currentElement = ""
        self.buffer = ""
    }
}

private struct TOSCompleteMultipartUploadPayload: Encodable {
    let Parts: [TOSCompleteMultipartUploadPart]
}

private struct TOSCompleteMultipartUploadPart: Encodable {
    let PartNumber: Int
    let ETag: String
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
    private let sdkClient: any TOSHTTPClientProtocol

    package init(
        configuration: OssMultipartClientConfiguration,
        sdkClient: any TOSHTTPClientProtocol
    ) throws {
        try TOSHTTPClientFactory.validate(configuration)
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
        guard credentials.objectStoreBackend == context.backend else {
            throw OssOperationError.invalidResponse("ReissueUploadCredentials returned a different objectStoreBackend")
        }
        guard credentials.objectStoreRegion == context.region else {
            throw OssOperationError.invalidResponse("ReissueUploadCredentials returned a different objectStoreRegion")
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
            sessionExpireAt: context.sessionExpireAt,
            backend: context.backend,
            region: context.region
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
            region: context.region,
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
        guard credentials.objectStoreBackend == "volcengine_tos" else {
            throw OssOperationError.invalidResponse("UploadCredentials object_store_backend must be volcengine_tos")
        }
        guard let region = credentials.objectStoreRegion.nilIfBlank else {
            throw OssOperationError.invalidResponse("UploadCredentials missing object_store_region")
        }
        let temporaryCredentials = try Self.makeTemporaryCredentials(from: credentials)
        return OssUploadContext(
            uploadID: uploadID,
            bucket: credentials.bucket,
            endpoint: credentials.endpoint,
            objectKey: credentials.objectKey,
            partSizeBytes: credentials.partSizeBytes,
            credentials: temporaryCredentials,
            credentialRefreshCount: credentialRefreshCount,
            sessionExpireAt: sessionExpireAtUnix.flatMap { Self.makeDate(fromUnix: $0) },
            backend: credentials.objectStoreBackend,
            region: region
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
    nil
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

    var nonEmpty: String? {
        self.isEmpty ? nil : self
    }

    var tosQueryEncoded: String {
        self.addingPercentEncoding(withAllowedCharacters: .tosQuery) ?? self
    }

    var xmlEscaped: String {
        self.replacingOccurrences(of: "&", with: "&amp;")
            .replacingOccurrences(of: "<", with: "&lt;")
            .replacingOccurrences(of: ">", with: "&gt;")
            .replacingOccurrences(of: "\"", with: "&quot;")
            .replacingOccurrences(of: "'", with: "&apos;")
    }
}

private extension CharacterSet {
    static let tosPathSegment = CharacterSet(charactersIn: "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~")
    static let tosQuery = CharacterSet(charactersIn: "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~")
}

private extension Data {
    var hexString: String {
        self.map { String(format: "%02x", $0) }.joined()
    }
}
