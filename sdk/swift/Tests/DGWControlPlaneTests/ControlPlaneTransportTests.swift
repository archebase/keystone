import Foundation
import Testing

import DGWControlPlane
import DGWProto
import GRPCCore

@Test func plaintextConfigurationUsesIPv4Target() throws {
    let configuration = ControlPlaneTransportConfiguration(
        endpoint: try #require(URL(string: "http://127.0.0.1:15053")),
        security: .plaintext,
        requestTimeout: .seconds(5)
    )

    #expect(try configuration.resolvedTarget() == .ipv4(address: "127.0.0.1", port: 15053))
}

@Test func tlsConfigurationUsesDNSAndDefaultPort() throws {
    let configuration = ControlPlaneTransportConfiguration(
        endpoint: try #require(URL(string: "https://gateway.example.com")),
        security: .tls,
        requestTimeout: .seconds(8)
    )

    #expect(try configuration.resolvedTarget() == .dns(host: "gateway.example.com", port: 443))
}

@Test func requestOptionsInjectAuthorizationHeaderAndTimeout() throws {
    let configuration = ControlPlaneTransportConfiguration(
        endpoint: try #require(URL(string: "https://gateway.example.com")),
        security: .tls,
        requestTimeout: .seconds(9)
    )

    let options = configuration.requestOptions(authorizationHeader: "Bearer token-1")

    #expect(Array(options.metadata[stringValues: "authorization"]) == ["Bearer token-1"])
    #expect(options.callOptions.timeout == .seconds(9))
}

@Test func gatewayClientMapsFiveRpcRequestsAndMetadata() async throws {
    let stub = GatewayServiceClientStub()
    let client = GatewayControlPlaneClient(client: stub, requestTimeout: .seconds(7))

    let createResponse = try await client.createLogicalUpload(
        clientHints: ["device": "iphone"],
        restartFromUploadID: "old-upload",
        authorizationHeader: "Bearer token-1"
    )
    #expect(createResponse.logicalUploadID == "logical-1")

    let recoveryResponse = try await client.getUploadRecovery(
        logicalUploadID: "logical-1",
        authorizationHeader: "Bearer token-1"
    )
    #expect(recoveryResponse.logicalUploadID == "logical-1")

    let reissueResponse = try await client.reissueUploadCredentials(
        uploadID: "upload-1",
        authorizationHeader: "Bearer token-1"
    )
    #expect(reissueResponse.uploadID == "upload-2")

    let abortResponse = try await client.abortUpload(
        logicalUploadID: "logical-1",
        reason: "aborted by client",
        authorizationHeader: "Bearer token-1"
    )
    #expect(abortResponse.uploadID == "upload-1")

    _ = try await client.completeUpload(
        uploadID: "upload-1",
        fileSize: 128,
        rawTags: ["scene": "robot"],
        completedPartCount: 3,
        ossObjectEtag: "\"etag-1\"",
        partSizeBytes: 64 * 1024 * 1024,
        authorizationHeader: "Bearer token-1"
    )

    let invocations = await stub.invocations()
    #expect(invocations.count == 5)
    #expect(invocations[0].method == "CreateLogicalUpload")
    #expect(invocations[0].metadata["authorization"] == ["Bearer token-1"])
    #expect(invocations[0].timeout == .seconds(7))
    #expect(invocations[0].requestSummary == "device=iphone,restart=old-upload")
    #expect(invocations[1].method == "GetUploadRecovery")
    #expect(invocations[1].requestSummary == "logical-1")
    #expect(invocations[2].method == "ReissueUploadCredentials")
    #expect(invocations[2].requestSummary == "upload-1")
    #expect(invocations[3].method == "AbortUpload")
    #expect(invocations[3].requestSummary == "logical-1:aborted by client")
    #expect(invocations[4].method == "CompleteUpload")
    #expect(invocations[4].requestSummary == "upload-1:128:3:\"etag-1\":67108864")
}

@Test func objectClientListsObjectsWithUserAuthorization() async throws {
    let stub = ObjectServiceClientStub()
    let client = ObjectControlPlaneClient(client: stub, requestTimeout: .seconds(4))

    let response = try await client.listObjects(
        pageSize: 25,
        pageToken: "page-1",
        filter: "status:verified",
        authorizationHeader: "Bearer user-token"
    )

    #expect(response.nextPageToken == "page-2")
    #expect(response.objects.map(\.fileID) == ["file-1"])

    let invocations = await stub.invocations()
    #expect(invocations == [
        InvocationRecord(
            method: "ListObjects",
            metadata: ["authorization": ["Bearer user-token"]],
            timeout: .seconds(4),
            requestSummary: "25:page-1:status:verified"
        ),
    ])
}

@Test func deviceInitTransportBuildsRequestWithoutAuthorization() async throws {
    let stub = DeviceInitServiceClientStub()
    let transport = DeviceInitServiceClientTransport(client: stub, requestTimeout: .seconds(6))

    let response = try await transport.initDevice(
        deviceID: "260427-000001",
        sdkVersion: "1.2.3",
        platform: "ios"
    )

    #expect(response.apiKey == "credential-v1")
    #expect(response.tags == ["device": "robot"])

    let invocations = await stub.invocations()
    #expect(invocations == [
        InvocationRecord(
            method: "InitDevice",
            metadata: ["authorization": []],
            timeout: .seconds(6),
            requestSummary: "260427-000001:1.2.3:ios"
        ),
    ])
}

@Test func deviceInitTransportBuildsReinitRequestWithoutAuthorization() async throws {
    let stub = DeviceInitServiceClientStub()
    let transport = DeviceInitServiceClientTransport(client: stub, requestTimeout: .seconds(6))

    let response = try await transport.reinitDevice(
        deviceID: "260427-000001",
        sdkVersion: "1.2.3",
        platform: "ios"
    )

    #expect(response.apiKey == "credential-v2")
    #expect(response.tags == ["device": "robot-reinit"])

    let invocations = await stub.invocations()
    #expect(invocations == [
        InvocationRecord(
            method: "ReinitDevice",
            metadata: ["authorization": []],
            timeout: .seconds(6),
            requestSummary: "260427-000001:1.2.3:ios"
        ),
    ])
}

@Test func deviceInitErrorMapperDecodesGatewayDetail() {
    let error = makeRPCError(
        code: .failedPrecondition,
        message: "device not ready",
        detailCode: "DATA_GATEWAY_DEVICE_NOT_READY",
        detailMessage: "device has not been bound to a suite"
    )

    #expect(
        ControlPlaneErrorMapper.map(error) == .gatewayFailed(
            statusCode: RPCError.Code.failedPrecondition.rawValue,
            detailCode: "DATA_GATEWAY_DEVICE_NOT_READY",
            message: "device has not been bound to a suite"
        )
    )
}

@Test func errorMapperPrefersStructuredDetail() {
    let error = makeRPCError(
        code: .failedPrecondition,
        message: "upload not refreshable",
        detailCode: "DATA_GATEWAY_UPLOAD_NOT_REFRESHABLE",
        detailMessage: "upload not refreshable"
    )

    #expect(
        ControlPlaneErrorMapper.map(error) == .gatewayFailed(
            statusCode: RPCError.Code.failedPrecondition.rawValue,
            detailCode: "DATA_GATEWAY_UPLOAD_NOT_REFRESHABLE",
            message: "upload not refreshable"
        )
    )
}

@Test func errorMapperHandlesAuthAndFallbackCodes() {
    let authError = makeRPCError(
        code: .unauthenticated,
        message: "invalid credential",
        detailCode: "AUTH_INVALID_CREDENTIAL",
        detailMessage: "invalid credential"
    )
    #expect(
        ControlPlaneErrorMapper.map(authError) == .authenticationFailed(
            code: "AUTH_INVALID_CREDENTIAL",
            message: "invalid credential"
        )
    )

    let notFoundError = RPCError(code: .notFound, message: "not found")
    #expect(
        ControlPlaneErrorMapper.map(notFoundError) == .gatewayFailed(
            statusCode: RPCError.Code.notFound.rawValue,
            detailCode: nil,
            message: "not found"
        )
    )
}

@Test func errorMapperMapsGatewayUploadNotFoundDetail() {
    let error = makeRPCError(
        code: .notFound,
        message: "missing upload",
        detailCode: "DATA_GATEWAY_UPLOAD_NOT_FOUND",
        detailMessage: "missing upload"
    )

    #expect(
        ControlPlaneErrorMapper.map(error) == .gatewayFailed(
            statusCode: RPCError.Code.notFound.rawValue,
            detailCode: "DATA_GATEWAY_UPLOAD_NOT_FOUND",
            message: "missing upload"
        )
    )
}

private struct InvocationRecord: Equatable, Sendable {
    let method: String
    let metadata: [String: [String]]
    let timeout: Duration?
    let requestSummary: String
}

private actor GatewayServiceClientStub: Archebase_DataGateway_V1_DataGatewayService.ClientProtocol {
    private var records: [InvocationRecord] = []

    func createLogicalUpload<Result>(
        request: ClientRequest<Archebase_DataGateway_V1_CreateLogicalUploadRequest>,
        serializer: some MessageSerializer<Archebase_DataGateway_V1_CreateLogicalUploadRequest>,
        deserializer: some MessageDeserializer<Archebase_DataGateway_V1_CreateLogicalUploadResponse>,
        options: CallOptions,
        onResponse handleResponse: @Sendable @escaping (ClientResponse<Archebase_DataGateway_V1_CreateLogicalUploadResponse>) async throws -> Result
    ) async throws -> Result where Result : Sendable {
        let summary = "device=\(request.message.clientHints["device", default: ""]),restart=\(request.message.restartFromUploadID)"
        self.record(method: "CreateLogicalUpload", metadata: request.metadata, timeout: options.timeout, requestSummary: summary)

        var response = Archebase_DataGateway_V1_CreateLogicalUploadResponse()
        response.logicalUploadID = "logical-1"
        response.uploadID = "upload-1"
        return try await handleResponse(ClientResponse(message: response))
    }

    func getUploadRecovery<Result>(
        request: ClientRequest<Archebase_DataGateway_V1_GetUploadRecoveryRequest>,
        serializer: some MessageSerializer<Archebase_DataGateway_V1_GetUploadRecoveryRequest>,
        deserializer: some MessageDeserializer<Archebase_DataGateway_V1_GetUploadRecoveryResponse>,
        options: CallOptions,
        onResponse handleResponse: @Sendable @escaping (ClientResponse<Archebase_DataGateway_V1_GetUploadRecoveryResponse>) async throws -> Result
    ) async throws -> Result where Result : Sendable {
        self.record(method: "GetUploadRecovery", metadata: request.metadata, timeout: options.timeout, requestSummary: request.message.logicalUploadID)

        var response = Archebase_DataGateway_V1_GetUploadRecoveryResponse()
        response.logicalUploadID = request.message.logicalUploadID
        return try await handleResponse(ClientResponse(message: response))
    }

    func reissueUploadCredentials<Result>(
        request: ClientRequest<Archebase_DataGateway_V1_ReissueUploadCredentialsRequest>,
        serializer: some MessageSerializer<Archebase_DataGateway_V1_ReissueUploadCredentialsRequest>,
        deserializer: some MessageDeserializer<Archebase_DataGateway_V1_ReissueUploadCredentialsResponse>,
        options: CallOptions,
        onResponse handleResponse: @Sendable @escaping (ClientResponse<Archebase_DataGateway_V1_ReissueUploadCredentialsResponse>) async throws -> Result
    ) async throws -> Result where Result : Sendable {
        self.record(method: "ReissueUploadCredentials", metadata: request.metadata, timeout: options.timeout, requestSummary: request.message.uploadID)

        var response = Archebase_DataGateway_V1_ReissueUploadCredentialsResponse()
        response.logicalUploadID = "logical-1"
        response.uploadID = "upload-2"
        return try await handleResponse(ClientResponse(message: response))
    }

    func abortUpload<Result>(
        request: ClientRequest<Archebase_DataGateway_V1_AbortUploadRequest>,
        serializer: some MessageSerializer<Archebase_DataGateway_V1_AbortUploadRequest>,
        deserializer: some MessageDeserializer<Archebase_DataGateway_V1_AbortUploadResponse>,
        options: CallOptions,
        onResponse handleResponse: @Sendable @escaping (ClientResponse<Archebase_DataGateway_V1_AbortUploadResponse>) async throws -> Result
    ) async throws -> Result where Result : Sendable {
        let summary = "\(request.message.logicalUploadID):\(request.message.reason)"
        self.record(method: "AbortUpload", metadata: request.metadata, timeout: options.timeout, requestSummary: summary)

        var response = Archebase_DataGateway_V1_AbortUploadResponse()
        response.logicalUploadID = request.message.logicalUploadID
        response.uploadID = "upload-1"
        return try await handleResponse(ClientResponse(message: response))
    }

    func completeUpload<Result>(
        request: ClientRequest<Archebase_DataGateway_V1_CompleteUploadRequest>,
        serializer: some MessageSerializer<Archebase_DataGateway_V1_CompleteUploadRequest>,
        deserializer: some MessageDeserializer<Archebase_DataGateway_V1_CompleteUploadResponse>,
        options: CallOptions,
        onResponse handleResponse: @Sendable @escaping (ClientResponse<Archebase_DataGateway_V1_CompleteUploadResponse>) async throws -> Result
    ) async throws -> Result where Result : Sendable {
        let summary = "\(request.message.uploadID):\(request.message.fileSize):\(request.message.completedPartCount):\(request.message.ossObjectEtag):\(request.message.partSizeBytes)"
        self.record(method: "CompleteUpload", metadata: request.metadata, timeout: options.timeout, requestSummary: summary)

        return try await handleResponse(ClientResponse(message: Archebase_DataGateway_V1_CompleteUploadResponse()))
    }

    func invocations() -> [InvocationRecord] {
        self.records
    }

    private func record(method: String, metadata: Metadata, timeout: Duration?, requestSummary: String) {
        self.records.append(
            InvocationRecord(
                method: method,
                metadata: Dictionary(uniqueKeysWithValues: [
                    ("authorization", Array(metadata[stringValues: "authorization"])),
                ]),
                timeout: timeout,
                requestSummary: requestSummary
            )
        )
    }
}

private actor ObjectServiceClientStub: Archebase_DataGateway_V1_DataGatewayObjectService.ClientProtocol {
    private var records: [InvocationRecord] = []

    func listObjects<Result>(
        request: ClientRequest<Archebase_DataGateway_V1_ListObjectsRequest>,
        serializer: some MessageSerializer<Archebase_DataGateway_V1_ListObjectsRequest>,
        deserializer: some MessageDeserializer<Archebase_DataGateway_V1_ListObjectsResponse>,
        options: CallOptions,
        onResponse handleResponse: @Sendable @escaping (ClientResponse<Archebase_DataGateway_V1_ListObjectsResponse>) async throws -> Result
    ) async throws -> Result where Result : Sendable {
        let summary = "\(request.message.pageSize):\(request.message.pageToken):\(request.message.filter)"
        self.records.append(
            InvocationRecord(
                method: "ListObjects",
                metadata: Dictionary(uniqueKeysWithValues: [
                    ("authorization", Array(request.metadata[stringValues: "authorization"])),
                ]),
                timeout: options.timeout,
                requestSummary: summary
            )
        )

        var object = Archebase_DataGateway_V1_DataObject()
        object.objectID = "object-1"
        object.fileID = "file-1"
        object.status = .verified
        object.sizeBytes = 128
        object.createdAtUnix = 1_700_000_001
        object.uploadedAtUnix = 1_700_000_002
        object.verifiedAtUnix = 1_700_000_003
        object.etag = "\"etag-1\""

        var response = Archebase_DataGateway_V1_ListObjectsResponse()
        response.objects = [object]
        response.nextPageToken = "page-2"
        return try await handleResponse(ClientResponse(message: response))
    }

    func requestDownload<Result>(
        request: ClientRequest<Archebase_DataGateway_V1_RequestDownloadRequest>,
        serializer: some MessageSerializer<Archebase_DataGateway_V1_RequestDownloadRequest>,
        deserializer: some MessageDeserializer<Archebase_DataGateway_V1_RequestDownloadResponse>,
        options: CallOptions,
        onResponse handleResponse: @Sendable @escaping (ClientResponse<Archebase_DataGateway_V1_RequestDownloadResponse>) async throws -> Result
    ) async throws -> Result where Result : Sendable {
        return try await handleResponse(ClientResponse(message: Archebase_DataGateway_V1_RequestDownloadResponse()))
    }

    func invocations() -> [InvocationRecord] {
        self.records
    }
}

private actor DeviceInitServiceClientStub: Archebase_DataGateway_V1_DeviceInitService.ClientProtocol {
    private var records: [InvocationRecord] = []

    func initDevice<Result>(
        request: ClientRequest<Archebase_DataGateway_V1_InitDeviceRequest>,
        serializer: some MessageSerializer<Archebase_DataGateway_V1_InitDeviceRequest>,
        deserializer: some MessageDeserializer<Archebase_DataGateway_V1_InitDeviceResponse>,
        options: CallOptions,
        onResponse handleResponse: @Sendable @escaping (ClientResponse<Archebase_DataGateway_V1_InitDeviceResponse>) async throws -> Result
    ) async throws -> Result where Result : Sendable {
        self.records.append(
            InvocationRecord(
                method: "InitDevice",
                metadata: Dictionary(uniqueKeysWithValues: [
                    ("authorization", Array(request.metadata[stringValues: "authorization"])),
                ]),
                timeout: options.timeout,
                requestSummary: "\(request.message.deviceID):\(request.message.sdkVersion):\(request.message.platform)"
            )
        )

        var response = Archebase_DataGateway_V1_InitDeviceResponse()
        response.apiKey = "credential-v1"
        response.tags = ["device": "robot"]
        return try await handleResponse(ClientResponse(message: response))
    }

    func reinitDevice<Result>(
        request: ClientRequest<Archebase_DataGateway_V1_ReinitDeviceRequest>,
        serializer: some MessageSerializer<Archebase_DataGateway_V1_ReinitDeviceRequest>,
        deserializer: some MessageDeserializer<Archebase_DataGateway_V1_InitDeviceResponse>,
        options: CallOptions,
        onResponse handleResponse: @Sendable @escaping (ClientResponse<Archebase_DataGateway_V1_InitDeviceResponse>) async throws -> Result
    ) async throws -> Result where Result : Sendable {
        self.records.append(
            InvocationRecord(
                method: "ReinitDevice",
                metadata: Dictionary(uniqueKeysWithValues: [
                    ("authorization", Array(request.metadata[stringValues: "authorization"])),
                ]),
                timeout: options.timeout,
                requestSummary: "\(request.message.deviceID):\(request.message.sdkVersion):\(request.message.platform)"
            )
        )

        var response = Archebase_DataGateway_V1_InitDeviceResponse()
        response.apiKey = "credential-v2"
        response.tags = ["device": "robot-reinit"]
        return try await handleResponse(ClientResponse(message: response))
    }

    func invocations() -> [InvocationRecord] {
        self.records
    }
}

private func makeRPCError(
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
