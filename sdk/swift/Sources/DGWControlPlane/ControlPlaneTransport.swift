import Foundation

import DGWAuth
import DGWProto
import GRPCCore
import GRPCNIOTransportHTTP2

private let authorizationMetadataKey = "authorization"
private let statusDetailsMetadataKey = "grpc-status-details-bin"

package enum ControlPlaneTransportSecurity: Sendable, Equatable {
    case plaintext
    case tls
}

package enum ControlPlaneResolvedTarget: Sendable, Equatable {
    case dns(host: String, port: Int)
    case ipv4(address: String, port: Int)
    case ipv6(address: String, port: Int)

    fileprivate func makeResolvableTarget() -> any ResolvableTarget {
        switch self {
        case .dns(let host, let port):
            return ResolvableTargets.DNS(host: host, port: port)
        case .ipv4(let address, let port):
            return ResolvableTargets.IPv4(addresses: [.init(host: address, port: port)])
        case .ipv6(let address, let port):
            return ResolvableTargets.IPv6(addresses: [.init(host: address, port: port)])
        }
    }
}

package enum ControlPlaneTransportError: Error, Sendable, Equatable {
    case invalidEndpoint(String)
}

package struct ControlPlaneRequestOptions: Sendable {
    package let metadata: Metadata
    package let callOptions: CallOptions

    package init(metadata: Metadata, callOptions: CallOptions) {
        self.metadata = metadata
        self.callOptions = callOptions
    }
}

package struct ControlPlaneRequestOptionsBuilder: Sendable {
    private let requestTimeout: Duration

    package init(requestTimeout: Duration) {
        self.requestTimeout = requestTimeout
    }

    package func make(authorizationHeader: String?) -> ControlPlaneRequestOptions {
        var metadata = Metadata()
        if let authorizationHeader {
            let trimmed = authorizationHeader.trimmingCharacters(in: .whitespacesAndNewlines)
            if !trimmed.isEmpty {
                metadata.replaceOrAddString(trimmed, forKey: authorizationMetadataKey)
            }
        }

        var callOptions = CallOptions.defaults
        callOptions.timeout = self.requestTimeout

        return ControlPlaneRequestOptions(metadata: metadata, callOptions: callOptions)
    }
}

package struct ControlPlaneTransportConfiguration: Sendable, Equatable {
    package let endpoint: URL
    package let security: ControlPlaneTransportSecurity
    package let requestTimeout: Duration

    package init(
        endpoint: URL,
        security: ControlPlaneTransportSecurity,
        requestTimeout: Duration
    ) {
        self.endpoint = endpoint
        self.security = security
        self.requestTimeout = requestTimeout
    }

    package func resolvedTarget() throws -> ControlPlaneResolvedTarget {
        guard let components = URLComponents(url: self.endpoint, resolvingAgainstBaseURL: false) else {
            throw ControlPlaneTransportError.invalidEndpoint(
                "endpoint '\(self.endpoint.absoluteString)' is not a valid URL"
            )
        }

        guard let host = components.host?.trimmingCharacters(in: .whitespacesAndNewlines), !host.isEmpty else {
            throw ControlPlaneTransportError.invalidEndpoint(
                "endpoint '\(self.endpoint.absoluteString)' must include a host"
            )
        }

        let port = components.port ?? self.defaultPort
        if Self.isIPv4Address(host) {
            return .ipv4(address: host, port: port)
        }
        if host.contains(":") {
            return .ipv6(address: host, port: port)
        }
        return .dns(host: host, port: port)
    }

    package func requestOptions(authorizationHeader: String?) -> ControlPlaneRequestOptions {
        ControlPlaneRequestOptionsBuilder(requestTimeout: self.requestTimeout)
            .make(authorizationHeader: authorizationHeader)
    }

    fileprivate func makeTransportSecurity() -> HTTP2ClientTransport.TransportServices.TransportSecurity {
        switch self.security {
        case .plaintext:
            return .plaintext
        case .tls:
            return .tls
        }
    }

    private var defaultPort: Int {
        switch self.security {
        case .plaintext:
            return 80
        case .tls:
            return 443
        }
    }

    private static func isIPv4Address(_ value: String) -> Bool {
        let components = value.split(separator: ".", omittingEmptySubsequences: false)
        guard components.count == 4 else {
            return false
        }

        return components.allSatisfy { component in
            guard !component.isEmpty, let octet = Int(component), (0 ... 255).contains(octet) else {
                return false
            }
            return true
        }
    }
}

package final class ManagedControlPlaneServiceClient<ServiceClient: Sendable>: @unchecked Sendable {
    package let serviceClient: ServiceClient

    private let grpcClient: GRPCClient<HTTP2ClientTransport.TransportServices>
    private let runTask: Task<Void, Never>

    package init(
        configuration: ControlPlaneTransportConfiguration,
        makeServiceClient: @escaping @Sendable (GRPCClient<HTTP2ClientTransport.TransportServices>) -> ServiceClient
    ) throws {
        let transport = try HTTP2ClientTransport.TransportServices(
            target: configuration.resolvedTarget().makeResolvableTarget(),
            transportSecurity: configuration.makeTransportSecurity()
        )
        let grpcClient = GRPCClient(transport: transport)

        self.grpcClient = grpcClient
        self.serviceClient = makeServiceClient(grpcClient)
        self.runTask = Task {
            try? await grpcClient.runConnections()
        }
    }

    deinit {
        self.shutdown()
    }

    package func shutdown() {
        self.grpcClient.beginGracefulShutdown()
    }
}

package struct ControlPlaneClientFactory: Sendable {
    private let configuration: ControlPlaneTransportConfiguration

    package init(configuration: ControlPlaneTransportConfiguration) {
        self.configuration = configuration
    }

    package func resolvedTarget() throws -> ControlPlaneResolvedTarget {
        try self.configuration.resolvedTarget()
    }

    package func requestOptions(authorizationHeader: String?) -> ControlPlaneRequestOptions {
        self.configuration.requestOptions(authorizationHeader: authorizationHeader)
    }

    package func makeAuthTransport() throws -> ManagedControlPlaneServiceClient<any CredentialExchangeTransport> {
        try ManagedControlPlaneServiceClient(configuration: self.configuration) { grpcClient in
            let authClient = Archebase_Auth_V1_AuthService.Client(wrapping: grpcClient)
            return AuthServiceClientTransport(client: authClient) as any CredentialExchangeTransport
        }
    }

    package func makeGatewayClient() throws -> ManagedControlPlaneServiceClient<Archebase_DataGateway_V1_DataGatewayService.Client<HTTP2ClientTransport.TransportServices>> {
        try ManagedControlPlaneServiceClient(configuration: self.configuration) { grpcClient in
            Archebase_DataGateway_V1_DataGatewayService.Client(wrapping: grpcClient)
        }
    }

    package func makeObjectClient() throws -> ManagedControlPlaneServiceClient<Archebase_DataGateway_V1_DataGatewayObjectService.Client<HTTP2ClientTransport.TransportServices>> {
        try ManagedControlPlaneServiceClient(configuration: self.configuration) { grpcClient in
            Archebase_DataGateway_V1_DataGatewayObjectService.Client(wrapping: grpcClient)
        }
    }

    package func makeDeviceInitTransport() throws -> ManagedControlPlaneServiceClient<any DeviceInitTransport> {
        try ManagedControlPlaneServiceClient(configuration: self.configuration) { grpcClient in
            let client = Archebase_DataGateway_V1_DeviceInitService.Client(wrapping: grpcClient)
            return DeviceInitServiceClientTransport(
                client: client,
                requestTimeout: self.configuration.requestTimeout
            ) as any DeviceInitTransport
        }
    }
}

package protocol DeviceInitTransport: Sendable {
    func initDevice(
        deviceID: String,
        sdkVersion: String,
        platform: String
    ) async throws -> Archebase_DataGateway_V1_InitDeviceResponse

    func reinitDevice(
        deviceID: String,
        sdkVersion: String,
        platform: String
    ) async throws -> Archebase_DataGateway_V1_InitDeviceResponse
}

package final class DeviceInitServiceClientTransport<Client: Archebase_DataGateway_V1_DeviceInitService.ClientProtocol>: DeviceInitTransport, @unchecked Sendable {
    private let client: Client
    private let optionsBuilder: ControlPlaneRequestOptionsBuilder

    package init(client: Client, requestTimeout: Duration) {
        self.client = client
        self.optionsBuilder = ControlPlaneRequestOptionsBuilder(requestTimeout: requestTimeout)
    }

    package func initDevice(
        deviceID: String,
        sdkVersion: String,
        platform: String
    ) async throws -> Archebase_DataGateway_V1_InitDeviceResponse {
        var request = Archebase_DataGateway_V1_InitDeviceRequest()
        request.deviceID = deviceID
        request.sdkVersion = sdkVersion
        request.platform = platform

        let options = self.optionsBuilder.make(authorizationHeader: nil)
        let response: ClientResponse<Archebase_DataGateway_V1_InitDeviceResponse> = try await self.client.initDevice(
            request,
            metadata: options.metadata,
            options: options.callOptions,
            onResponse: { response in response }
        )

        return try response.message
    }

    package func reinitDevice(
        deviceID: String,
        sdkVersion: String,
        platform: String
    ) async throws -> Archebase_DataGateway_V1_InitDeviceResponse {
        var request = Archebase_DataGateway_V1_ReinitDeviceRequest()
        request.deviceID = deviceID
        request.sdkVersion = sdkVersion
        request.platform = platform

        let options = self.optionsBuilder.make(authorizationHeader: nil)
        let response: ClientResponse<Archebase_DataGateway_V1_InitDeviceResponse> = try await self.client.reinitDevice(
            request,
            metadata: options.metadata,
            options: options.callOptions,
            onResponse: { response in response }
        )

        return try response.message
    }
}

package protocol GatewayControlPlaneClientProtocol: Sendable {
    func createLogicalUpload(
        clientHints: [String: String],
        restartFromUploadID: String?,
        authorizationHeader: String
    ) async throws -> Archebase_DataGateway_V1_CreateLogicalUploadResponse

    func getUploadRecovery(
        logicalUploadID: String,
        authorizationHeader: String
    ) async throws -> Archebase_DataGateway_V1_GetUploadRecoveryResponse

    func reissueUploadCredentials(
        uploadID: String,
        authorizationHeader: String
    ) async throws -> Archebase_DataGateway_V1_ReissueUploadCredentialsResponse

    func abortUpload(
        logicalUploadID: String,
        reason: String,
        authorizationHeader: String
    ) async throws -> Archebase_DataGateway_V1_AbortUploadResponse

    func completeUpload(
        uploadID: String,
        fileSize: Int64,
        rawTags: [String: String],
        completedPartCount: Int32,
        ossObjectEtag: String,
        partSizeBytes: Int64,
        authorizationHeader: String
    ) async throws -> Archebase_DataGateway_V1_CompleteUploadResponse
}

package final class GatewayControlPlaneClient<Client: Archebase_DataGateway_V1_DataGatewayService.ClientProtocol>: GatewayControlPlaneClientProtocol, @unchecked Sendable {
    private let client: Client
    private let optionsBuilder: ControlPlaneRequestOptionsBuilder

    package init(
        client: Client,
        requestTimeout: Duration
    ) {
        self.client = client
        self.optionsBuilder = ControlPlaneRequestOptionsBuilder(requestTimeout: requestTimeout)
    }

    package func createLogicalUpload(
        clientHints: [String: String],
        restartFromUploadID: String?,
        authorizationHeader: String
    ) async throws -> Archebase_DataGateway_V1_CreateLogicalUploadResponse {
        var request = Archebase_DataGateway_V1_CreateLogicalUploadRequest()
        request.clientHints = clientHints
        request.restartFromUploadID = restartFromUploadID ?? ""

        let options = self.optionsBuilder.make(authorizationHeader: authorizationHeader)
        let response: ClientResponse<Archebase_DataGateway_V1_CreateLogicalUploadResponse> = try await self.client.createLogicalUpload(
            request,
            metadata: options.metadata,
            options: options.callOptions,
            onResponse: { response in response }
        )

        return try response.message
    }

    package func getUploadRecovery(
        logicalUploadID: String,
        authorizationHeader: String
    ) async throws -> Archebase_DataGateway_V1_GetUploadRecoveryResponse {
        var request = Archebase_DataGateway_V1_GetUploadRecoveryRequest()
        request.logicalUploadID = logicalUploadID

        let options = self.optionsBuilder.make(authorizationHeader: authorizationHeader)
        let response: ClientResponse<Archebase_DataGateway_V1_GetUploadRecoveryResponse> = try await self.client.getUploadRecovery(
            request,
            metadata: options.metadata,
            options: options.callOptions,
            onResponse: { response in response }
        )

        return try response.message
    }

    package func reissueUploadCredentials(
        uploadID: String,
        authorizationHeader: String
    ) async throws -> Archebase_DataGateway_V1_ReissueUploadCredentialsResponse {
        var request = Archebase_DataGateway_V1_ReissueUploadCredentialsRequest()
        request.uploadID = uploadID

        let options = self.optionsBuilder.make(authorizationHeader: authorizationHeader)
        let response: ClientResponse<Archebase_DataGateway_V1_ReissueUploadCredentialsResponse> = try await self.client.reissueUploadCredentials(
            request,
            metadata: options.metadata,
            options: options.callOptions,
            onResponse: { response in response }
        )

        return try response.message
    }

    package func abortUpload(
        logicalUploadID: String,
        reason: String,
        authorizationHeader: String
    ) async throws -> Archebase_DataGateway_V1_AbortUploadResponse {
        var request = Archebase_DataGateway_V1_AbortUploadRequest()
        request.logicalUploadID = logicalUploadID
        request.reason = reason

        let options = self.optionsBuilder.make(authorizationHeader: authorizationHeader)
        let response: ClientResponse<Archebase_DataGateway_V1_AbortUploadResponse> = try await self.client.abortUpload(
            request,
            metadata: options.metadata,
            options: options.callOptions,
            onResponse: { response in response }
        )

        return try response.message
    }

    package func completeUpload(
        uploadID: String,
        fileSize: Int64,
        rawTags: [String: String],
        completedPartCount: Int32,
        ossObjectEtag: String,
        partSizeBytes: Int64,
        authorizationHeader: String
    ) async throws -> Archebase_DataGateway_V1_CompleteUploadResponse {
        var request = Archebase_DataGateway_V1_CompleteUploadRequest()
        request.uploadID = uploadID
        request.fileSize = fileSize
        request.rawTags = rawTags
        request.completedPartCount = completedPartCount
        request.ossObjectEtag = ossObjectEtag
        request.partSizeBytes = partSizeBytes

        let options = self.optionsBuilder.make(authorizationHeader: authorizationHeader)
        let response: ClientResponse<Archebase_DataGateway_V1_CompleteUploadResponse> = try await self.client.completeUpload(
            request,
            metadata: options.metadata,
            options: options.callOptions,
            onResponse: { response in response }
        )

        return try response.message
    }
}

package protocol ObjectControlPlaneClientProtocol: Sendable {
    func listObjects(
        pageSize: Int32,
        pageToken: String,
        filter: String,
        authorizationHeader: String
    ) async throws -> Archebase_DataGateway_V1_ListObjectsResponse
}

package final class ObjectControlPlaneClient<Client: Archebase_DataGateway_V1_DataGatewayObjectService.ClientProtocol>: ObjectControlPlaneClientProtocol, @unchecked Sendable {
    private let client: Client
    private let optionsBuilder: ControlPlaneRequestOptionsBuilder

    package init(
        client: Client,
        requestTimeout: Duration
    ) {
        self.client = client
        self.optionsBuilder = ControlPlaneRequestOptionsBuilder(requestTimeout: requestTimeout)
    }

    package func listObjects(
        pageSize: Int32,
        pageToken: String,
        filter: String,
        authorizationHeader: String
    ) async throws -> Archebase_DataGateway_V1_ListObjectsResponse {
        var request = Archebase_DataGateway_V1_ListObjectsRequest()
        request.pageSize = pageSize
        request.pageToken = pageToken
        request.filter = filter

        let options = self.optionsBuilder.make(authorizationHeader: authorizationHeader)
        let response: ClientResponse<Archebase_DataGateway_V1_ListObjectsResponse> = try await self.client.listObjects(
            request,
            metadata: options.metadata,
            options: options.callOptions,
            onResponse: { response in response }
        )

        return try response.message
    }
}

/// Public error model returned by the Swift Data Gateway client.
public enum DataGatewayClientError: Error, Sendable, Equatable {
    case authenticationFailed(code: String?, message: String)
    case gatewayFailed(statusCode: Int, detailCode: String?, message: String)
    case invalidConfiguration(String)
    case alreadyInitialized(configURL: URL)
    case notInitialized(configURL: URL)
    case endpointsAlreadyInitialized(endpointsURL: URL)
    case endpointsNotInitialized(endpointsURL: URL)
    case invalidLocalFile(String)
    case zeroByteFile
    case ossFailed(httpStatus: Int?, ossCode: String?, message: String)
    case persistenceFailed(String)
    case rawTagConflict(key: String)
    case uploadRestartExceeded
    case resumeNotPossible(String)
    case integrityCheckFailed(String)
    case retryExhausted(lastError: String)
    case cancelled
}

package enum DeviceInitGatewayDetailCode {
    package static let alreadyInitialized = "DATA_GATEWAY_DEVICE_ALREADY_INITIALIZED"
    package static let notInitialized = "DATA_GATEWAY_DEVICE_NOT_INITIALIZED"
}

package enum ControlPlaneErrorMapper {
    package static func map(_ error: any Error) -> DataGatewayClientError {
        if let clientError = error as? DataGatewayClientError {
            return clientError
        }
        if let authError = error as? CredentialAuthProviderError {
            switch authError {
            case .authenticationFailed(let code, let message):
                return .authenticationFailed(code: code, message: message)
            case .invalidCredential(let message), .invalidResponse(let message), .transportFailure(_, let message):
                return .authenticationFailed(code: nil, message: message)
            }
        }
        if let rpcError = error as? RPCError {
            let detail = ControlPlaneErrorMapper.decodeErrorDetail(from: rpcError)
            let detailCode = detail?.code.nilIfBlank
            let message = detail?.message.nilIfBlank ?? rpcError.message

            if detailCode?.hasPrefix("AUTH_") == true || rpcError.code == .unauthenticated {
                return .authenticationFailed(code: detailCode, message: message)
            }

            if rpcError.code == .cancelled {
                return .cancelled
            }

            return .gatewayFailed(statusCode: rpcError.code.rawValue, detailCode: detailCode, message: message)
        }

        return .retryExhausted(lastError: String(describing: error))
    }

    package static func decodeErrorDetail(from rpcError: RPCError) -> Archebase_Common_V1_ErrorDetail? {
        guard let bytes = rpcError.metadata[binaryValues: statusDetailsMetadataKey].first(where: { _ in true }) else {
            return nil
        }

        return try? Archebase_Common_V1_ErrorDetail(serializedBytes: bytes)
    }
}

package extension String {
    var nilIfBlank: String? {
        let trimmed = self.trimmingCharacters(in: .whitespacesAndNewlines)
        return trimmed.isEmpty ? nil : trimmed
    }
}
