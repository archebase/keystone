import DGWOss
import Foundation

/// Errors raised while preparing the local Rust stack integration harness.
package enum LocalStackHarnessError: Error, Sendable, Equatable {
    case missingEnvironmentVariable(String)
    case invalidEndpoint(String)
}

/// Test-only bootstrap metadata required to create a local `credential_base64`.
package struct LocalStackBootstrapConfig: Sendable, Equatable {
    package var gatewayBaseURL: URL
    package var organization: String
    package var adminUserName: String
    package var adminPassword: String
    package var siteName: String
    package var siteStatus: Int32
    package var apiKeyID: String
    package var apiKeyPrefix: String
    package var apiKeyStatus: Int32
    package var csrfOrigin: String

    package init(
        gatewayBaseURL: URL,
        organization: String,
        adminUserName: String,
        adminPassword: String,
        siteName: String,
        siteStatus: Int32,
        apiKeyID: String,
        apiKeyPrefix: String,
        apiKeyStatus: Int32,
        csrfOrigin: String
    ) {
        self.gatewayBaseURL = gatewayBaseURL
        self.organization = organization
        self.adminUserName = adminUserName
        self.adminPassword = adminPassword
        self.siteName = siteName
        self.siteStatus = siteStatus
        self.apiKeyID = apiKeyID
        self.apiKeyPrefix = apiKeyPrefix
        self.apiKeyStatus = apiKeyStatus
        self.csrfOrigin = csrfOrigin
    }
}

/// Test-only device init metadata exported by the local bootstrap script.
package struct LocalStackDeviceInitConfig: Sendable, Equatable {
    package var endpoint: URL
    package var deviceID: String
    package var unboundDeviceID: String?
    package var configURL: URL

    package init(endpoint: URL, deviceID: String, unboundDeviceID: String?, configURL: URL) {
        self.endpoint = endpoint
        self.deviceID = deviceID
        self.unboundDeviceID = unboundDeviceID
        self.configURL = configURL
    }
}

/// Environment reader used by local-stack integration tests.
package struct LocalStackTestEnvironment: Sendable {
    package var environment: [String: String]

    package init(environment: [String: String] = ProcessInfo.processInfo.environment) {
        self.environment = environment
    }

    /// Builds a client config for the local Rust stack integration environment.
    package func makeClientConfig(credentialBase64Override: String? = nil) throws -> DataGatewayClientConfig {
        let authEndpoint = try self.requiredURL(for: "DGW_LOCAL_AUTH_ENDPOINT")
        let gatewayEndpoint = try self.requiredURL(for: "DGW_LOCAL_GATEWAY_ENDPOINT")
        let credentialBase64: String
        if let override = credentialBase64Override?.trimmedNonEmpty {
            credentialBase64 = override
        } else {
            credentialBase64 = try self.requiredValue(for: "DGW_LOCAL_CREDENTIAL_BASE64")
        }
        let persistRoot = URL(
            fileURLWithPath: self.environment["DGW_LOCAL_PERSIST_ROOT"] ?? NSTemporaryDirectory(),
            isDirectory: true
        )

        return DataGatewayClientConfig.testRecommended(
            authEndpoint: authEndpoint,
            gatewayEndpoint: gatewayEndpoint,
            credentialBase64: credentialBase64,
            persistRootURL: persistRoot,
            tls: .plaintext
        )
    }

    /// Returns the HTTP bootstrap configuration used to create one local test credential.
    package func makeBootstrapConfig() throws -> LocalStackBootstrapConfig {
        let gatewayBaseURL = try self.requiredURL(for: "DGW_LOCAL_GATEWAY_HTTP_BASE")
        return LocalStackBootstrapConfig(
            gatewayBaseURL: gatewayBaseURL,
            organization: self.environment["DGW_LOCAL_BOOTSTRAP_ORGANIZATION"]?.trimmedNonEmpty ?? "system",
            adminUserName: self.environment["DGW_LOCAL_BOOTSTRAP_ADMIN_USER"]?.trimmedNonEmpty ?? "admin",
            adminPassword: try self.requiredValue(for: "DGW_LOCAL_BOOTSTRAP_ADMIN_PASSWORD"),
            siteName: self.environment["DGW_LOCAL_BOOTSTRAP_SITE_NAME"]?.trimmedNonEmpty ?? "swift-local-site",
            siteStatus: Self.parseInt32(self.environment["DGW_LOCAL_BOOTSTRAP_SITE_STATUS"], defaultValue: 1),
            apiKeyID: self.environment["DGW_LOCAL_BOOTSTRAP_API_KEY_ID"]?.trimmedNonEmpty ?? "swift-local-key",
            apiKeyPrefix: self.environment["DGW_LOCAL_BOOTSTRAP_API_KEY_PREFIX"]?.trimmedNonEmpty ?? "swift-local",
            apiKeyStatus: Self.parseInt32(self.environment["DGW_LOCAL_BOOTSTRAP_API_KEY_STATUS"], defaultValue: 1),
            csrfOrigin: self.environment["DGW_LOCAL_BOOTSTRAP_CSRF_ORIGIN"]?.trimmedNonEmpty ?? gatewayBaseURL.absoluteString.trimmingTrailingSlash
        )
    }

    /// Builds the local device initialization test configuration.
    package func makeDeviceInitConfig() throws -> LocalStackDeviceInitConfig {
        let persistRoot = URL(
            fileURLWithPath: self.environment["DGW_LOCAL_PERSIST_ROOT"] ?? NSTemporaryDirectory(),
            isDirectory: true
        )
        return LocalStackDeviceInitConfig(
            endpoint: try self.requiredURL(for: "DGW_LOCAL_INIT_ENDPOINT"),
            deviceID: try self.requiredValue(for: "DGW_LOCAL_DEVICE_ID"),
            unboundDeviceID: self.environment["DGW_LOCAL_UNBOUND_DEVICE_ID"]?.trimmedNonEmpty,
            configURL: persistRoot.appendingPathComponent("archebase-config.json")
        )
    }

    private func requiredValue(for key: String) throws -> String {
        guard let value = self.environment[key]?.trimmingCharacters(in: .whitespacesAndNewlines), !value.isEmpty else {
            throw LocalStackHarnessError.missingEnvironmentVariable(key)
        }
        return value
    }

    private func requiredURL(for key: String) throws -> URL {
        let value = try self.requiredValue(for: key)
        guard let url = Self.normalizedURL(from: value) else {
            throw LocalStackHarnessError.invalidEndpoint(key)
        }
        return url
    }

    private static func parseInt32(_ value: String?, defaultValue: Int32) -> Int32 {
        guard let value = value?.trimmedNonEmpty, let parsed = Int32(value) else {
            return defaultValue
        }
        return parsed
    }

    private static func normalizedURL(from value: String) -> URL? {
        if let url = URL(string: value), url.host?.isEmpty == false {
            return url
        }

        guard
            let schemeRange = value.range(of: ":/"),
            !value[schemeRange.upperBound...].hasPrefix("/")
        else {
            return nil
        }

        let normalized = value.replacingCharacters(in: schemeRange, with: "://")
        guard let url = URL(string: normalized), url.host?.isEmpty == false else {
            return nil
        }
        return url
    }
}

/// Errors raised while validating the Aliyun OSS test harness environment.
package enum AliyunOSSHarnessError: Error, Sendable, Equatable {
    case missingEnvironmentVariable(String)
    case invalidEnvironmentVariable(String)
}

/// Expected object metadata for remote upload assertions.
package struct RemoteUploadExpectation: Sendable, Equatable {
    package var bucket: String
    package var objectPrefix: String

    package init(bucket: String, objectPrefix: String) {
        self.bucket = bucket
        self.objectPrefix = objectPrefix
    }
}

/// Environment reader used by real Aliyun OSS integration tests.
package struct AliyunOSSTestEnvironment: Sendable {
    package var environment: [String: String]

    package init(environment: [String: String] = ProcessInfo.processInfo.environment) {
        self.environment = environment
    }

    /// Validates that all required Aliyun OSS integration variables are present.
    package func validate() throws {
        for key in [
            "DGW_OSS_TEST_ENDPOINT",
            "DGW_OSS_TEST_BUCKET",
            "DGW_OSS_TEST_ACCESS_KEY_ID",
            "DGW_OSS_TEST_ACCESS_KEY_SECRET",
            "DGW_OSS_TEST_SECURITY_TOKEN",
            "DGW_OSS_TEST_OBJECT_PREFIX",
        ] {
            guard let value = self.environment[key]?.trimmingCharacters(in: .whitespacesAndNewlines), !value.isEmpty else {
                throw AliyunOSSHarnessError.missingEnvironmentVariable(key)
            }
        }
    }

    /// Returns the expected bucket/prefix for remote upload assertions.
    package func remoteUploadExpectation() throws -> RemoteUploadExpectation {
        RemoteUploadExpectation(
            bucket: try self.requiredValue(for: "DGW_OSS_TEST_BUCKET"),
            objectPrefix: try self.requiredValue(for: "DGW_OSS_TEST_OBJECT_PREFIX")
        )
    }

    /// Builds a client config for the real remote auth/gateway environment.
    package func makeRemoteClientConfig() throws -> DataGatewayClientConfig {
        let credentialBase64 = try self.requiredValue(for: "DGW_REAL_CREDENTIAL_BASE64")
        let persistRoot = URL(
            fileURLWithPath: self.environment["DGW_REAL_PERSIST_ROOT"] ?? NSTemporaryDirectory(),
            isDirectory: true
        )

        if self.environment["DGW_PUBLIC_DNS_INTEGRATION"] == "1" {
            let endpointsURL = persistRoot.appendingPathComponent(ArchebasePublicEndpoints.endpointsFileName)
            try DataGatewayClient.initialize(
                endpointsJSON: try self.publicEndpointsJSON(),
                endpointsURL: endpointsURL
            )
            let config = try DataGatewayClientConfig.recommended(
                credentialBase64: credentialBase64,
                persistRootURL: persistRoot,
                endpointsURL: endpointsURL
            )
            return try self.applyRemoteRequestTimeoutOverride(to: config)
        }

        let authEndpoint = try self.requiredURL(for: "DGW_REAL_AUTH_ENDPOINT")
        let gatewayEndpoint = try self.requiredURL(for: "DGW_REAL_GATEWAY_ENDPOINT")

        let tls: TLSMode
        if let explicitTLS = self.environment["DGW_REAL_TLS_MODE"]?.trimmedNonEmpty {
            switch explicitTLS.lowercased() {
            case "plaintext", "http":
                tls = .plaintext
            case "tls", "https":
                tls = .tls
            default:
                throw LocalStackHarnessError.invalidEndpoint("DGW_REAL_TLS_MODE")
            }
        } else {
            tls = authEndpoint.scheme?.lowercased() == "https" ? .tls : .plaintext
        }

        let config = DataGatewayClientConfig.testRecommended(
            authEndpoint: authEndpoint,
            gatewayEndpoint: gatewayEndpoint,
            credentialBase64: credentialBase64,
            persistRootURL: persistRoot,
            tls: tls
        )
        return try self.applyRemoteRequestTimeoutOverride(to: config)
    }

    private func applyRemoteRequestTimeoutOverride(to config: DataGatewayClientConfig) throws -> DataGatewayClientConfig {
        guard let seconds = try self.remoteRequestTimeoutSeconds() else {
            return config
        }
        var copy = config
        copy.requestTimeout = .seconds(seconds)
        return copy
    }

    private func remoteRequestTimeoutSeconds() throws -> Int64? {
        guard let value = self.environment["DGW_REAL_REQUEST_TIMEOUT_SECONDS"]?.trimmedNonEmpty else {
            return nil
        }
        guard let seconds = Int64(value), seconds > 0 else {
            throw AliyunOSSHarnessError.invalidEnvironmentVariable("DGW_REAL_REQUEST_TIMEOUT_SECONDS")
        }
        return seconds
    }

    private func requiredValue(for key: String) throws -> String {
        guard let value = self.environment[key]?.trimmingCharacters(in: .whitespacesAndNewlines), !value.isEmpty else {
            throw AliyunOSSHarnessError.missingEnvironmentVariable(key)
        }
        return value
    }

    private func requiredURL(for key: String) throws -> URL {
        let value = try self.requiredValue(for: key)
        if let url = Self.normalizedURL(from: value) {
            return url
        }
        throw LocalStackHarnessError.invalidEndpoint(key)
    }

    private func optionalURL(for key: String) throws -> URL? {
        guard let value = self.environment[key]?.trimmedNonEmpty else {
            return nil
        }
        guard let url = Self.normalizedURL(from: value) else {
            throw LocalStackHarnessError.invalidEndpoint(key)
        }
        return url
    }

    private func publicEndpointsJSON() throws -> String {
        if let json = self.environment["DGW_PUBLIC_ENDPOINTS_JSON"]?.trimmedNonEmpty {
            return json
        }
        if let path = self.environment["DGW_PUBLIC_ENDPOINTS_FILE"]?.trimmedNonEmpty {
            return try String(contentsOf: URL(fileURLWithPath: path), encoding: .utf8)
        }

        let authEndpoint = try self.requiredURL(for: "DGW_REAL_AUTH_ENDPOINT")
        let gatewayEndpoint = try self.requiredURL(for: "DGW_REAL_GATEWAY_ENDPOINT")
        let deviceInitEndpoint = try self.optionalURL(for: "DGW_REAL_INIT_ENDPOINT")
            ?? self.optionalURL(for: "DGW_REAL_DEVICE_INIT_ENDPOINT")
            ?? gatewayEndpoint
        return Self.endpointsJSON(
            authEndpoint: authEndpoint,
            gatewayEndpoint: gatewayEndpoint,
            deviceInitEndpoint: deviceInitEndpoint
        )
    }

    private static func endpointsJSON(authEndpoint: URL, gatewayEndpoint: URL, deviceInitEndpoint: URL) -> String {
        """
        {
          "auth": \(Self.endpointJSON(authEndpoint)),
          "gateway": \(Self.endpointJSON(gatewayEndpoint)),
          "deviceInit": \(Self.endpointJSON(deviceInitEndpoint))
        }
        """
    }

    private static func endpointJSON(_ endpoint: URL) -> String {
        let scheme = endpoint.scheme?.lowercased() ?? "https"
        let host = endpoint.host(percentEncoded: false) ?? endpoint.host ?? ""
        let port = endpoint.port ?? (scheme == "https" ? 443 : 80)
        return #"{"scheme":"\#(scheme)","host":"\#(host)","port":\#(port)}"#
    }

    private static func normalizedURL(from value: String) -> URL? {
        if let url = URL(string: value), url.host?.isEmpty == false {
            return url
        }

        guard
            let schemeRange = value.range(of: ":/"),
            !value[schemeRange.upperBound...].hasPrefix("/")
        else {
            return nil
        }

        let normalized = value.replacingCharacters(in: schemeRange, with: "://")
        guard let url = URL(string: normalized), url.host?.isEmpty == false else {
            return nil
        }
        return url
    }
}

package struct LocalStackMockMultipartSession: UploadCoordinatorMultipartSessionProtocol {
    package let uploadID: String
    package let completedETag: String

    package init(uploadID: String, completedETag: String = "\"runtime-object-etag\"") {
        self.uploadID = uploadID
        self.completedETag = completedETag
    }

    package func ensureFreshCredentialsIfNeeded() async throws -> Bool {
        false
    }

    package func lastKnownCredentialExpiration() async -> Date? {
        nil
    }

    package func initiateMultipartUpload() async throws -> String {
        "runtime-multipart-\(self.uploadID)"
    }

    package func uploadPart(
        multipartUploadID: String,
        partNumber: Int,
        body: OssUploadBody
    ) async throws -> UploadedPartDescriptor {
        _ = multipartUploadID
        return UploadedPartDescriptor(
            partNumber: partNumber,
            etag: "\"runtime-part-\(partNumber)\"",
            size: body.sizeBytes,
            lastModified: nil,
            hashCRC64: nil
        )
    }

    package func putObject(body: OssUploadBody) async throws -> UploadedPartDescriptor {
        UploadedPartDescriptor(
            partNumber: 1,
            etag: self.completedETag,
            size: body.sizeBytes,
            lastModified: nil,
            hashCRC64: nil
        )
    }

    package func listParts(multipartUploadID: String) async throws -> [UploadedPartDescriptor] {
        _ = multipartUploadID
        return []
    }

    package func headObjectETag() async throws -> String {
        self.completedETag
    }

    package func completeMultipartUpload(
        multipartUploadID: String,
        parts: [UploadedPartDescriptor]
    ) async throws -> String {
        _ = multipartUploadID
        _ = parts
        return self.completedETag
    }
}

private extension String {
    var trimmedNonEmpty: String? {
        let trimmed = self.trimmingCharacters(in: .whitespacesAndNewlines)
        return trimmed.isEmpty ? nil : trimmed
    }

    var trimmingTrailingSlash: String {
        var value = self
        while value.hasSuffix("/") {
            value.removeLast()
        }
        return value
    }
}
