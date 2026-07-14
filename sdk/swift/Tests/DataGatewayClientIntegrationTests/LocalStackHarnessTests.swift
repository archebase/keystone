import DGWAuth
import DGWControlPlane
import DGWProto
import DGWStore
import Foundation
import GRPCNIOTransportHTTP2
import Testing

@testable import DataGatewayClient

private let runtimeIntegrationEnabled = ProcessInfo.processInfo.environment["DGW_LOCAL_RUNTIME_INTEGRATION"] == "1"
private let realRuntimeIntegrationEnabled = ProcessInfo.processInfo.environment["DGW_REAL_RUNTIME_INTEGRATION"] == "1"
private let realDeviceInitIntegrationEnabled = ProcessInfo.processInfo.environment["DGW_REAL_DEVICE_INIT_INTEGRATION"] == "1"
private let publicDNSIntegrationEnabled = ProcessInfo.processInfo.environment["DGW_PUBLIC_DNS_INTEGRATION"] == "1"
private let realObjectListingIntegrationEnabled = realRuntimeIntegrationEnabled && hasRealUserAuthorizationHeader()

@Suite(.serialized)
struct LocalStackHarnessTests {

@Test func localStackEnvironmentReadsConfiguredEndpointsAndCredential() throws {
    let environment: [String: String] = [
        "DGW_LOCAL_AUTH_ENDPOINT": "http://127.0.0.1:15055",
        "DGW_LOCAL_GATEWAY_ENDPOINT": "http://127.0.0.1:15053",
        "DGW_LOCAL_INIT_ENDPOINT": "http://127.0.0.1:15057",
        "DGW_LOCAL_CREDENTIAL_BASE64": "credential-base64",
        "DGW_LOCAL_DEVICE_ID": "260427-000001",
        "DGW_LOCAL_UNBOUND_DEVICE_ID": "260427-000002",
        "DGW_LOCAL_PERSIST_ROOT": "/tmp/swift-dgw-local-tests",
    ]

    let config = try LocalStackTestEnvironment(environment: environment).makeClientConfig()
    let initConfig = try LocalStackTestEnvironment(environment: environment).makeDeviceInitConfig()

    #expect(config.authEndpoint == URL(string: "http://127.0.0.1:15055")!)
    #expect(config.gatewayEndpoint == URL(string: "http://127.0.0.1:15053")!)
    #expect(config.credentialBase64 == "credential-base64")
    #expect(config.persistRootURL == URL(fileURLWithPath: "/tmp/swift-dgw-local-tests", isDirectory: true))
    #expect(config.tls == .plaintext)
    #expect(initConfig.endpoint == URL(string: "http://127.0.0.1:15057")!)
    #expect(initConfig.deviceID == "260427-000001")
    #expect(initConfig.unboundDeviceID == "260427-000002")
}

@Test func localStackEnvironmentNormalizesSingleSlashSimulatorURLs() throws {
    let environment: [String: String] = [
        "DGW_LOCAL_AUTH_ENDPOINT": "http:/127.0.0.1:15055",
        "DGW_LOCAL_GATEWAY_ENDPOINT": "http:/127.0.0.1:15053",
        "DGW_LOCAL_CREDENTIAL_BASE64": "credential-base64",
        "DGW_LOCAL_PERSIST_ROOT": "/tmp/swift-dgw-local-tests",
    ]

    let config = try LocalStackTestEnvironment(environment: environment).makeClientConfig()

    #expect(config.authEndpoint == URL(string: "http://127.0.0.1:15055")!)
    #expect(config.gatewayEndpoint == URL(string: "http://127.0.0.1:15053")!)
}

@Test func realEnvironmentURLHelperNormalizesSingleSlashSimulatorURLs() throws {
    let url = try #require(normalizedURLFromEnvironmentValue("http:/example.com:50057"))

    #expect(url == URL(string: "http://example.com:50057")!)
}

@Test func localStackEnvironmentFailsWhenRequiredVariablesAreMissing() {
    let environment = LocalStackTestEnvironment(environment: [:])

    let error = #expect(throws: LocalStackHarnessError.self) {
        try environment.makeClientConfig()
    }

    #expect(error == .missingEnvironmentVariable("DGW_LOCAL_AUTH_ENDPOINT"))
}

@Test func localStackBootstrapConfigUsesExplicitEnvironmentOverrides() throws {
    let environment = LocalStackTestEnvironment(environment: [
        "DGW_LOCAL_GATEWAY_HTTP_BASE": "http://127.0.0.1:18098",
        "DGW_LOCAL_BOOTSTRAP_ADMIN_PASSWORD": "ci-admin-password",
        "DGW_LOCAL_BOOTSTRAP_ORGANIZATION": "system",
        "DGW_LOCAL_BOOTSTRAP_ADMIN_USER": "admin",
        "DGW_LOCAL_BOOTSTRAP_SITE_NAME": "swift-site",
        "DGW_LOCAL_BOOTSTRAP_SITE_STATUS": "2",
        "DGW_LOCAL_BOOTSTRAP_API_KEY_ID": "swift-key",
        "DGW_LOCAL_BOOTSTRAP_API_KEY_PREFIX": "swift-prefix",
        "DGW_LOCAL_BOOTSTRAP_API_KEY_STATUS": "2",
        "DGW_LOCAL_BOOTSTRAP_CSRF_ORIGIN": "http://127.0.0.1:18098",
    ])

    let config = try environment.makeBootstrapConfig()

    #expect(config.gatewayBaseURL == URL(string: "http://127.0.0.1:18098")!)
    #expect(config.organization == "system")
    #expect(config.adminUserName == "admin")
    #expect(config.adminPassword == "ci-admin-password")
    #expect(config.siteName == "swift-site")
    #expect(config.siteStatus == 2)
    #expect(config.apiKeyID == "swift-key")
    #expect(config.apiKeyPrefix == "swift-prefix")
    #expect(config.apiKeyStatus == 2)
    #expect(config.csrfOrigin == "http://127.0.0.1:18098")
}

@Test func localStackBootstrapConfigSuppliesStableDefaults() throws {
    let config = try LocalStackTestEnvironment(environment: [
        "DGW_LOCAL_GATEWAY_HTTP_BASE": "http://127.0.0.1:18098/",
        "DGW_LOCAL_BOOTSTRAP_ADMIN_PASSWORD": "ci-admin-password",
    ]).makeBootstrapConfig()

    #expect(config.organization == "system")
    #expect(config.adminUserName == "admin")
    #expect(config.siteName == "swift-local-site")
    #expect(config.siteStatus == 1)
    #expect(config.apiKeyID == "swift-local-key")
    #expect(config.apiKeyPrefix == "swift-local")
    #expect(config.apiKeyStatus == 1)
    #expect(config.csrfOrigin == "http://127.0.0.1:18098")
}

@Test func localStackBootstrapConfigRequiresGatewayBaseAndAdminPassword() {
    let missingGateway = LocalStackTestEnvironment(environment: [
        "DGW_LOCAL_BOOTSTRAP_ADMIN_PASSWORD": "ci-admin-password",
    ])
    let missingPassword = LocalStackTestEnvironment(environment: [
        "DGW_LOCAL_GATEWAY_HTTP_BASE": "http://127.0.0.1:18098",
    ])

    let gatewayError = #expect(throws: LocalStackHarnessError.self) {
        try missingGateway.makeBootstrapConfig()
    }
    let passwordError = #expect(throws: LocalStackHarnessError.self) {
        try missingPassword.makeBootstrapConfig()
    }

    #expect(gatewayError == .missingEnvironmentVariable("DGW_LOCAL_GATEWAY_HTTP_BASE"))
    #expect(passwordError == .missingEnvironmentVariable("DGW_LOCAL_BOOTSTRAP_ADMIN_PASSWORD"))
}

@Test func localStackBootstrapScriptContainsRequiredPreparationSteps() throws {
    let scriptURL = URL(fileURLWithPath: #filePath)
        .deletingLastPathComponent()
        .deletingLastPathComponent()
        .deletingLastPathComponent()
        .appendingPathComponent("Scripts/local_integration_bootstrap.sh")
    let script = try String(contentsOf: scriptURL, encoding: .utf8)

    #expect(script.contains("scripts/local_run.sh --build debug --deploy --reset-db"))
    #expect(script.contains("DATA_GATEWAY_USE_MOCK_STS=true"))
    #expect(script.contains("DGW_LOCAL_GATEWAY_HTTP_BASE"))
    #expect(script.contains("DGW_LOCAL_BOOTSTRAP_ADMIN_PASSWORD"))
    #expect(script.contains("DGW_LOCAL_BOOTSTRAP_MAX_TIME_SECONDS"))
    #expect(script.contains("DGW_LOCAL_BOOTSTRAP_CONNECT_TIMEOUT_SECONDS"))
    #expect(script.contains("DGW_LOCAL_OPERATION_API_BASE"))
    #expect(script.contains(#"BOOTSTRAP_API_KEY_SUFFIX="${BOOTSTRAP_API_KEY_SUFFIX:0:26}""#))
    #expect(script.contains("swift-key-${BOOTSTRAP_API_KEY_SUFFIX}"))
    #expect(script.contains("DGW_LOCAL_BOOTSTRAP_API_KEY_NAME"))
    #expect(script.contains("DGW_LOCAL_CREDENTIAL_BASE64"))
    #expect(script.contains("DGW_LOCAL_DEVICE_ID"))
    #expect(script.contains("DGW_LOCAL_UNBOUND_DEVICE_ID"))
    #expect(script.contains("DGW_LOCAL_INIT_ENDPOINT"))
    #expect(script.contains("curl -sS -X POST"))
    #expect(script.contains("${OPERATION_API_BASE}/auth/login"))
    #expect(script.contains("${OPERATION_API_BASE}/sites"))
    #expect(script.contains("${OPERATION_API_BASE}/sites/${SITE_ID}/api-keys"))
    #expect(script.contains("${OPERATION_API_BASE}/devices:register"))
    #expect(script.contains("${OPERATION_API_BASE}/deviceSuites"))
    #expect(script.contains(#""keyName":$(json_string "$BOOTSTRAP_API_KEY_NAME")"#))
    #expect(script.contains(#""suite":$(json_string "$SUITE_NAME")"#))
    #expect(script.contains("CreateSiteApiKey"))
    #expect(script.contains("RemoveDeviceFromSuite"))
    #expect(script.contains(":removeDevice"))
}

@Test func simulatorSmokeScriptSkipsPackageUpdatesForCachedDependencies() throws {
    let scriptURL = URL(fileURLWithPath: #filePath)
        .deletingLastPathComponent()
        .deletingLastPathComponent()
        .deletingLastPathComponent()
        .appendingPathComponent("Scripts/simulator_smoke.sh")
    let script = try String(contentsOf: scriptURL, encoding: .utf8)

    #expect(script.contains("xcodebuild build-for-testing"))
    #expect(script.contains("-skipPackageUpdates"))
    #expect(script.contains(#"for key in "$AUTH_ENDPOINT_KEY" "$GATEWAY_ENDPOINT_KEY" "$INIT_ENDPOINT_KEY"; do"#))
}

@Test func aliyunEnvironmentContractValidatesPresenceOfCredentials() {
    let environment = AliyunOSSTestEnvironment(environment: [:])

    let error = #expect(throws: AliyunOSSHarnessError.self) {
        try environment.validate()
    }

    #expect(error == .missingEnvironmentVariable("DGW_OSS_TEST_ENDPOINT"))
}

@Test func aliyunEnvironmentContractAcceptsCompleteConfiguration() throws {
    let environment = AliyunOSSTestEnvironment(environment: [
        "DGW_OSS_TEST_ENDPOINT": "https://oss-cn-shanghai.aliyuncs.com",
        "DGW_OSS_TEST_BUCKET": "bucket-1",
        "DGW_OSS_TEST_ACCESS_KEY_ID": "ak",
        "DGW_OSS_TEST_ACCESS_KEY_SECRET": "sk",
        "DGW_OSS_TEST_SECURITY_TOKEN": "token",
        "DGW_OSS_TEST_OBJECT_PREFIX": "swift-tests/run-1",
    ])

    try environment.validate()
}

@Test func aliyunEnvironmentBuildsRemoteClientConfig() throws {
    let config = try AliyunOSSTestEnvironment(environment: [
        "DGW_REAL_AUTH_ENDPOINT": "http://example-auth:50051",
        "DGW_REAL_GATEWAY_ENDPOINT": "http://example-gateway:50053",
        "DGW_REAL_CREDENTIAL_BASE64": "credential-base64",
        "DGW_REAL_PERSIST_ROOT": "/tmp/swift-dgw-real-tests",
        "DGW_REAL_TLS_MODE": "plaintext",
    ]).makeRemoteClientConfig()

    #expect(config.authEndpoint == URL(string: "http://example-auth:50051")!)
    #expect(config.gatewayEndpoint == URL(string: "http://example-gateway:50053")!)
    #expect(config.credentialBase64 == "credential-base64")
    #expect(config.persistRootURL == URL(fileURLWithPath: "/tmp/swift-dgw-real-tests", isDirectory: true))
    #expect(config.tls == .plaintext)
}

@Test func aliyunEnvironmentAppliesRemoteRequestTimeoutOverride() throws {
    let config = try AliyunOSSTestEnvironment(environment: [
        "DGW_REAL_AUTH_ENDPOINT": "http://example-auth:50051",
        "DGW_REAL_GATEWAY_ENDPOINT": "http://example-gateway:50053",
        "DGW_REAL_CREDENTIAL_BASE64": "credential-base64",
        "DGW_REAL_REQUEST_TIMEOUT_SECONDS": "120",
    ]).makeRemoteClientConfig()

    #expect(config.requestTimeout == .seconds(120))
}

@Test func aliyunEnvironmentRemoteConfigRequiresCredentialBeforeEndpointOverrides() {
    let environment = AliyunOSSTestEnvironment(environment: [:])

    let error = #expect(throws: AliyunOSSHarnessError.self) {
        try environment.makeRemoteClientConfig()
    }

    #expect(error == .missingEnvironmentVariable("DGW_REAL_CREDENTIAL_BASE64"))
}

@Test func aliyunEnvironmentNormalizesSingleSlashRemoteURLs() throws {
    let config = try AliyunOSSTestEnvironment(environment: [
        "DGW_REAL_AUTH_ENDPOINT": "http:/example-auth:50051",
        "DGW_REAL_GATEWAY_ENDPOINT": "http:/example-gateway:50053",
        "DGW_REAL_CREDENTIAL_BASE64": "credential-base64",
        "DGW_REAL_PERSIST_ROOT": "/tmp/swift-dgw-real-tests",
        "DGW_REAL_TLS_MODE": "plaintext",
        "DGW_OSS_TEST_ENDPOINT": "https://oss-cn-shanghai.aliyuncs.com",
        "DGW_OSS_TEST_BUCKET": "archebase",
        "DGW_OSS_TEST_ACCESS_KEY_ID": "ak",
        "DGW_OSS_TEST_ACCESS_KEY_SECRET": "sk",
        "DGW_OSS_TEST_SECURITY_TOKEN": "token",
        "DGW_OSS_TEST_OBJECT_PREFIX": "dev/1",
    ]).makeRemoteClientConfig()

    #expect(config.authEndpoint == URL(string: "http://example-auth:50051")!)
    #expect(config.gatewayEndpoint == URL(string: "http://example-gateway:50053")!)
}

@Test func aliyunEnvironmentProvidesRemoteUploadExpectation() throws {
    let expectation = try AliyunOSSTestEnvironment(environment: [
        "DGW_OSS_TEST_ENDPOINT": "https://oss-cn-shanghai.aliyuncs.com",
        "DGW_OSS_TEST_BUCKET": "archebase",
        "DGW_OSS_TEST_ACCESS_KEY_ID": "ak",
        "DGW_OSS_TEST_ACCESS_KEY_SECRET": "sk",
        "DGW_OSS_TEST_SECURITY_TOKEN": "token",
        "DGW_OSS_TEST_OBJECT_PREFIX": "dev/1",
    ]).remoteUploadExpectation()

    #expect(expectation.bucket == "archebase")
    #expect(expectation.objectPrefix == "dev/1")
}

@Test(
    .enabled(if: runtimeIntegrationEnabled)
) func localStackRuntimeBootstrapAndControlPlaneFlow() async throws {
    let environment = LocalStackTestEnvironment()
    let clientConfig = try environment.makeClientConfig()
    #expect(!clientConfig.credentialBase64.isEmpty)

    let authFactory = ControlPlaneClientFactory(
        configuration: ControlPlaneTransportConfiguration(
            endpoint: clientConfig.authEndpoint,
            security: .plaintext,
            requestTimeout: clientConfig.requestTimeout
        )
    )
    let authTransport = try authFactory.makeAuthTransport()
    let authProvider = CredentialAuthProvider(
        credentialBase64: clientConfig.credentialBase64,
        refreshBefore: clientConfig.authRefreshBefore,
        requestTimeout: clientConfig.requestTimeout,
        transport: authTransport.serviceClient
    )
    let gatewayTransport = try ManagedControlPlaneServiceClient(
        configuration: ControlPlaneTransportConfiguration(
            endpoint: clientConfig.gatewayEndpoint,
            security: .plaintext,
            requestTimeout: clientConfig.requestTimeout
        )
    ) { grpcClient in
        Archebase_DataGateway_V1_DataGatewayService.Client(wrapping: grpcClient)
    }
    let objectTransport = try ManagedControlPlaneServiceClient(
        configuration: ControlPlaneTransportConfiguration(
            endpoint: clientConfig.gatewayEndpoint,
            security: .plaintext,
            requestTimeout: clientConfig.requestTimeout
        )
    ) { grpcClient in
        Archebase_DataGateway_V1_DataGatewayObjectService.Client(wrapping: grpcClient)
    }
    let gatewayClient = AnyUploadCoordinatorGatewayClient(
        authProvider: authProvider,
        gatewayServiceClient: gatewayTransport.serviceClient,
        requestTimeout: clientConfig.requestTimeout,
        retryPolicy: clientConfig.retryPolicy.controlPlane.controlPlaneValue
    )
    let stateStore = UploadStateStore(persistRoot: clientConfig.persistRootURL)
    let fileCoordinator = FileStagingCoordinator(
        stagingRoot: clientConfig.persistRootURL
            .appendingPathComponent("data-gateway-client", isDirectory: true)
            .appendingPathComponent("staging", isDirectory: true)
    )
    let client = DataGatewayClient(
        uploadCoordinator: UploadCoordinator(
            executionPolicy: clientConfig.execution,
            dependencies: UploadCoordinatorDependencies(
                gatewayClient: gatewayClient,
                stateStore: stateStore,
                fileCoordinator: fileCoordinator,
                ossClientFactory: { context in LocalStackMockMultipartSession(uploadID: context.uploadID) }
            )
        ),
        runtimeResources: DataGatewayClientRuntimeResources(
            authTransport: authTransport,
            gatewayTransport: gatewayTransport,
            objectTransport: objectTransport
        )
    )

    let fileURL = FileManager.default.temporaryDirectory
        .appendingPathComponent("swift-local-runtime-\(UUID().uuidString)")
        .appendingPathExtension("bin")
    try Data("swift-local-runtime-payload".utf8).write(to: fileURL)
    defer { try? FileManager.default.removeItem(at: fileURL) }

    let zeroByteURL = FileManager.default.temporaryDirectory
        .appendingPathComponent("swift-local-runtime-zero-\(UUID().uuidString)")
        .appendingPathExtension("bin")
    try Data().write(to: zeroByteURL)
    defer { try? FileManager.default.removeItem(at: zeroByteURL) }

    let zeroByteError: DGWControlPlane.DataGatewayClientError? = await #expect(throws: DGWControlPlane.DataGatewayClientError.self) {
        try await client.upload(
            UploadRequest(fileURL: zeroByteURL, clientHints: ["kind": "zero-byte"], rawTags: [:], displayName: nil)
        )
    }
    #expect(zeroByteError == .zeroByteFile)

    let result = try await client.upload(
        UploadRequest(fileURL: fileURL, clientHints: ["kind": "runtime"], rawTags: ["suite": "local-stack"], displayName: "runtime")
    )
    #expect(!result.logicalUploadID.isEmpty)
    #expect(!result.uploadID.isEmpty)
    #expect(result.fileSize == UInt64(Data("swift-local-runtime-payload".utf8).count))
    #expect(!result.bucket.isEmpty)
    #expect(!result.objectKey.isEmpty)
    #expect(!result.ossObjectETag.isEmpty)

    let pending = try await client.listPendingUploads()
    #expect(!pending.contains(where: { $0.logicalUploadID == result.logicalUploadID }))

    let resumeError: DGWControlPlane.DataGatewayClientError? = await #expect(throws: DGWControlPlane.DataGatewayClientError.self) {
        try await client.resumeUpload(logicalUploadID: "missing-logical-upload-id")
    }
    switch resumeError {
    case .resumeNotPossible(let message):
        #expect(message == "local snapshot not found: missing-logical-upload-id")
    default:
        Issue.record("unexpected resume error: \(String(describing: resumeError))")
    }
}

@Test(
    .enabled(if: runtimeIntegrationEnabled)
) func localCredentialCanExchangeForBearerToken() async throws {
    let environment = LocalStackTestEnvironment()
    let clientConfig = try environment.makeClientConfig()

    let authFactory = ControlPlaneClientFactory(
        configuration: ControlPlaneTransportConfiguration(
            endpoint: clientConfig.authEndpoint,
            security: .plaintext,
            requestTimeout: clientConfig.requestTimeout
        )
    )
    let authTransport = try authFactory.makeAuthTransport()
    let authProvider = CredentialAuthProvider(
        credentialBase64: clientConfig.credentialBase64,
        refreshBefore: clientConfig.authRefreshBefore,
        requestTimeout: clientConfig.requestTimeout,
        transport: authTransport.serviceClient
    )

    let header = try await authProvider.authorizationHeader()
    #expect(header.starts(with: "Bearer "))
}

@Test(
    .enabled(if: publicDNSIntegrationEnabled && realRuntimeIntegrationEnabled)
) func publicPathCanExchangeForBearerToken() async throws {
    let environment = AliyunOSSTestEnvironment()
    let clientConfig = try uniqueRealClientConfig(from: environment.makeRemoteClientConfig(), label: "public-auth")
    let harness = try makeRealGatewayHarness(clientConfig: clientConfig)
    let response = try await harness.gatewayClient.createLogicalUpload(clientHints: ["suite": "public-dns"], restartFromUploadID: nil)

    #expect(!response.logicalUploadID.isEmpty)
}

@Test(
    .enabled(if: publicDNSIntegrationEnabled && realRuntimeIntegrationEnabled)
) func publicPathRuntimeBootstrapAndControlPlaneFlow() async throws {
    try await realAliyunRuntimeUploadFlow()
}

@Test(
    .enabled(if: publicDNSIntegrationEnabled && realDeviceInitIntegrationEnabled)
) func publicPathDeviceInitThenUploadFlow() async throws {
    try await realAliyunDeviceInitReinitAndFromConfigUploadFlow()
}

@Test(
    .enabled(if: runtimeIntegrationEnabled)
) func localGatewayDeviceInitFlow() async throws {
    let environment = LocalStackTestEnvironment()
    let initConfig = try environment.makeDeviceInitConfig()
    let configURL = uniqueConfigURL(from: initConfig.configURL)
    let initializer = try ArchebaseDeviceInitializer(
        config: DeviceInitClientConfig(configURL: configURL, tls: .plaintext),
        initEndpoint: initConfig.endpoint,
        sdkVersion: "local-integration",
        platform: "ios-simulator"
    )

    let config = try await initializer.initDevice(deviceID: initConfig.deviceID)

    #expect(!config.apiKey.isEmpty)
    #expect(try await ArchebaseConfigStore(configURL: configURL).load() == config)
}

@Test(
    .enabled(if: runtimeIntegrationEnabled)
) func localGatewayDeviceInitFailsWhenUnbound() async throws {
    let environment = LocalStackTestEnvironment()
    let initConfig = try environment.makeDeviceInitConfig()
    let unboundDeviceID = try #require(initConfig.unboundDeviceID)
    let initializer = try ArchebaseDeviceInitializer(
        config: DeviceInitClientConfig(configURL: uniqueConfigURL(from: initConfig.configURL), tls: .plaintext),
        initEndpoint: initConfig.endpoint,
        sdkVersion: "local-integration",
        platform: "ios-simulator"
    )

    let error = await #expect(throws: DataGatewayClientError.self) {
        _ = try await initializer.initDevice(deviceID: unboundDeviceID)
    }

    guard case .gatewayFailed(_, let detailCode, _) = error else {
        Issue.record("unexpected init error: \(String(describing: error))")
        return
    }
    #expect(detailCode == "DATA_GATEWAY_DEVICE_NOT_READY")
}

@Test(
    .enabled(if: runtimeIntegrationEnabled)
) func localGatewayDeviceInitRejectsLegacyUuidDeviceId() async throws {
    let environment = LocalStackTestEnvironment()
    let initConfig = try environment.makeDeviceInitConfig()
    let initializer = try ArchebaseDeviceInitializer(
        config: DeviceInitClientConfig(configURL: uniqueConfigURL(from: initConfig.configURL), tls: .plaintext),
        initEndpoint: initConfig.endpoint,
        sdkVersion: "local-integration",
        platform: "ios-simulator"
    )

    let error = await #expect(throws: DataGatewayClientError.self) {
        _ = try await initializer.initDevice(deviceID: "00000000-0000-0000-0000-000000000000")
    }

    guard case .gatewayFailed(_, let detailCode, _) = error else {
        Issue.record("unexpected init error: \(String(describing: error))")
        return
    }
    #expect(detailCode == "DATA_GATEWAY_DEVICE_ID_INVALID")
}

@Test(
    .enabled(if: runtimeIntegrationEnabled)
) func localGatewayInitThenUploadFlow() async throws {
    let environment = LocalStackTestEnvironment()
    let clientConfig = try environment.makeClientConfig()
    let initConfig = try environment.makeDeviceInitConfig()
    let configURL = uniqueConfigURL(from: initConfig.configURL)
    let initializer = try ArchebaseDeviceInitializer(
        config: DeviceInitClientConfig(configURL: configURL, tls: .plaintext),
        initEndpoint: initConfig.endpoint,
        sdkVersion: "local-integration",
        platform: "ios-simulator"
    )
    _ = try await initializer.initDevice(deviceID: initConfig.deviceID)

    let client = try await DataGatewayClient.testFromArchebaseConfig(
        authEndpoint: clientConfig.authEndpoint,
        gatewayEndpoint: clientConfig.gatewayEndpoint,
        configURL: configURL,
        persistRootURL: clientConfig.persistRootURL,
        tls: .plaintext
    )
    let fileURL = clientConfig.persistRootURL
        .appendingPathComponent("swift-local-init-upload-\(UUID().uuidString)")
        .appendingPathExtension("bin")
    let payload = Data("swift-local-init-upload-payload".utf8)
    try FileManager.default.createDirectory(at: clientConfig.persistRootURL, withIntermediateDirectories: true)
    try payload.write(to: fileURL)
    defer { try? FileManager.default.removeItem(at: fileURL) }

    let result = try await client.upload(
        UploadRequest(fileURL: fileURL, clientHints: ["kind": "device-init"], rawTags: [:], displayName: "device-init")
    )

    #expect(!result.logicalUploadID.isEmpty)
    #expect(result.fileSize == UInt64(payload.count))
}

@Test(
    .enabled(if: realRuntimeIntegrationEnabled)
) func realAliyunRuntimeUploadFlow() async throws {
    let environment = AliyunOSSTestEnvironment()
    try environment.validate()
    let expectation = try environment.remoteUploadExpectation()
    let clientConfig = try uniqueRealClientConfig(from: environment.makeRemoteClientConfig(), label: "runtime")
    defer { try? FileManager.default.removeItem(at: clientConfig.persistRootURL) }
    let client = try DataGatewayClient(config: clientConfig)

    let fileURL = clientConfig.persistRootURL
        .appendingPathComponent("aliyun-real-runtime-\(UUID().uuidString)")
        .appendingPathExtension("bin")
    let payload = Data("aliyun-real-runtime-payload-\(UUID().uuidString)".utf8)
    try FileManager.default.createDirectory(at: clientConfig.persistRootURL, withIntermediateDirectories: true)
    try payload.write(to: fileURL)
    defer { try? FileManager.default.removeItem(at: fileURL) }

    let result = try await client.upload(
        UploadRequest(
            fileURL: fileURL,
            clientHints: ["suite": "aliyun-real", "mode": "swift-e2e"],
            rawTags: ["suite": "aliyun-real", "runtime": "macos"],
            displayName: "aliyun-real-runtime"
        )
    )

    #expect(!result.logicalUploadID.isEmpty)
    #expect(!result.uploadID.isEmpty)
    #expect(result.fileSize == UInt64(payload.count))
    #expect(result.bucket == expectation.bucket)
    #expect(result.objectKey.hasPrefix(expectation.objectPrefix))
    #expect(!result.ossObjectETag.isEmpty)
    try assertNoStagingDataCopies(under: clientConfig.persistRootURL)

    let pending = try await client.listPendingUploads()
    #expect(!pending.contains(where: { $0.logicalUploadID == result.logicalUploadID }))
}

@Test(
    .enabled(if: realObjectListingIntegrationEnabled)
) func realAliyunListObjectsFlow() async throws {
    let environment = AliyunOSSTestEnvironment()
    try environment.validate()
    let clientConfig = try uniqueRealClientConfig(from: environment.makeRemoteClientConfig(), label: "list-objects")
    defer { try? FileManager.default.removeItem(at: clientConfig.persistRootURL) }
    let client = try DataGatewayClient(config: clientConfig)
    let runID = "swift-list-\(UUID().uuidString)"
    let payload = Data("aliyun-real-list-objects-payload-\(runID)".utf8)
    let fileURL = try writeRealPayload(payload, under: clientConfig.persistRootURL, name: "aliyun-real-list-objects")

    let upload = try await client.upload(
        UploadRequest(
            fileURL: fileURL,
            clientHints: ["suite": "aliyun-real", "mode": "list-objects"],
            rawTags: ["suite": "aliyun-real", "runtime": "list-objects", "object_list_run": runID],
            displayName: "aliyun-real-list-objects"
        )
    )

    let page = try await client.listObjects(
        ListObjectsOptions(pageSize: 10, pageToken: nil, filter: "raw_tags.object_list_run=\(runID)"),
        authorizationHeader: try realUserAuthorizationHeader()
    )

    let object = try #require(page.objects.first)
    #expect(page.objects.count == 1)
    #expect(page.nextPageToken.isEmpty)
    #expect(!object.fileID.isEmpty)
    #expect(object.status == .verified)
    #expect(object.sizeBytes == Int64(payload.count))
    #expect(canonicalObjectETag(object.etag) == canonicalObjectETag(upload.ossObjectETag))
}

@Test(
    .enabled(if: realRuntimeIntegrationEnabled)
) func realAliyunUploadEventsFlow() async throws {
    let environment = AliyunOSSTestEnvironment()
    try environment.validate()
    let expectation = try environment.remoteUploadExpectation()
    let clientConfig = try uniqueRealClientConfig(from: environment.makeRemoteClientConfig(), label: "events")
    defer { try? FileManager.default.removeItem(at: clientConfig.persistRootURL) }
    let client = try DataGatewayClient(config: clientConfig)
    let payload = Data("aliyun-real-events-payload-\(UUID().uuidString)".utf8)
    let fileURL = try writeRealPayload(payload, under: clientConfig.persistRootURL, name: "aliyun-real-events")

    var events: [UploadEvent] = []
    for try await event in await client.uploadEvents(
        UploadRequest(
            fileURL: fileURL,
            clientHints: ["suite": "aliyun-real", "mode": "events"],
            rawTags: ["suite": "aliyun-real", "runtime": "events"],
            displayName: "aliyun-real-events"
        )
    ) {
        events.append(event)
    }

    assertPutObjectUploadEvents(events)

    guard let result = completedUploadResult(from: events) else {
        Issue.record("real Aliyun uploadEvents did not end with completed: \(events)")
        return
    }
    #expect(result.fileSize == UInt64(payload.count))
    #expect(result.bucket == expectation.bucket)
    #expect(result.objectKey.hasPrefix(expectation.objectPrefix))
    #expect(!result.ossObjectETag.isEmpty)
    try assertNoStagingDataCopies(under: clientConfig.persistRootURL)
}

@Test(
    .enabled(if: realRuntimeIntegrationEnabled)
) func realAliyunExactPartSizeUploadEventsFlow() async throws {
    let environment = AliyunOSSTestEnvironment()
    try environment.validate()
    let expectation = try environment.remoteUploadExpectation()
    let clientConfig = try uniqueRealClientConfig(from: environment.makeRemoteClientConfig(), label: "exact-part-size")
    defer { try? FileManager.default.removeItem(at: clientConfig.persistRootURL) }
    let client = try DataGatewayClient(config: clientConfig)
    let size = realPartSizePayloadSizeBytes()
    let fileURL = try writeRealPayload(Data(repeating: 0x45, count: size), under: clientConfig.persistRootURL, name: "aliyun-real-exact-part-size")

    var events: [UploadEvent] = []
    for try await event in await client.uploadEvents(
        UploadRequest(
            fileURL: fileURL,
            clientHints: ["suite": "aliyun-real", "mode": "exact-part-size"],
            rawTags: ["suite": "aliyun-real", "runtime": "exact-part-size"],
            displayName: "aliyun-real-exact-part-size"
        )
    ) {
        events.append(event)
    }

    let result = try #require(completedUploadResult(from: events))
    assertPutObjectUploadEvents(events)
    #expect(result.fileSize == UInt64(size))
    #expect(result.bucket == expectation.bucket)
    #expect(result.objectKey.hasPrefix(expectation.objectPrefix))
    #expect(!result.ossObjectETag.isEmpty)
    try assertNoStagingDataCopies(under: clientConfig.persistRootURL)
}

@Test(
    .enabled(if: realRuntimeIntegrationEnabled)
) func realAliyunMultipartUploadEventsFlow() async throws {
    let environment = AliyunOSSTestEnvironment()
    try environment.validate()
    let expectation = try environment.remoteUploadExpectation()
    let clientConfig = try uniqueRealClientConfig(from: environment.makeRemoteClientConfig(), label: "multipart")
    defer { try? FileManager.default.removeItem(at: clientConfig.persistRootURL) }
    let client = try DataGatewayClient(config: clientConfig)
    let size = realMultipartPayloadSizeBytes()
    let fileURL = try writeRealPayload(Data(repeating: 0x5A, count: size), under: clientConfig.persistRootURL, name: "aliyun-real-multipart")

    var events: [UploadEvent] = []
    for try await event in await client.uploadEvents(
        UploadRequest(
            fileURL: fileURL,
            clientHints: ["suite": "aliyun-real", "mode": "multipart"],
            rawTags: ["suite": "aliyun-real", "runtime": "multipart"],
            displayName: "aliyun-real-multipart"
        )
    ) {
        events.append(event)
    }

    let result = try #require(completedUploadResult(from: events))
    let uploadedPartCount = uploadEventCount(events, matching: isUploadingPartEvent)
    #expect(events.contains(where: isInitiatingMultipartEvent))
    #expect(events.contains(where: isCompletingMultipartEvent))
    #expect(uploadedPartCount >= 2)
    #expect(result.fileSize == UInt64(size))
    #expect(result.bucket == expectation.bucket)
    #expect(result.objectKey.hasPrefix(expectation.objectPrefix))
    #expect(!result.ossObjectETag.isEmpty)
    try assertNoStagingDataCopies(under: clientConfig.persistRootURL)
}

@Test(
    .enabled(if: realRuntimeIntegrationEnabled)
) func realAliyunConfigStoreFromConfigAndUploadFlow() async throws {
    let environment = AliyunOSSTestEnvironment()
    try environment.validate()
    let expectation = try environment.remoteUploadExpectation()
    let clientConfig = try uniqueRealClientConfig(from: environment.makeRemoteClientConfig(), label: "from-config")
    defer { try? FileManager.default.removeItem(at: clientConfig.persistRootURL) }
    let configURL = clientConfig.persistRootURL.appendingPathComponent("archebase-config.json")
    let store = ArchebaseConfigStore(configURL: configURL)

    let initialConfig = try ArchebaseConfig(apiKey: clientConfig.credentialBase64, tags: ["device": "aliyun-config"])
    try initialConfig.validate()
    let decodedInitialConfig = try ArchebaseConfig.decodeValidated(from: initialConfig.prettyJSONData())
    #expect(!(await store.exists()))
    try await store.initialize(decodedInitialConfig)
    #expect(await store.exists())
    #expect(await store.resolvedConfigURL() == configURL.standardizedFileURL)
    #expect(try await store.load() == decodedInitialConfig)

    let replacedConfig = try ArchebaseConfig(
        apiKey: clientConfig.credentialBase64,
        tags: ["device": "aliyun-config", "flow": "from-config"]
    )
    try await store.replaceForReinit(replacedConfig)
    #expect(try await store.load() == replacedConfig)

    let client = try await DataGatewayClient.testFromArchebaseConfig(
        authEndpoint: clientConfig.authEndpoint,
        gatewayEndpoint: clientConfig.gatewayEndpoint,
        configURL: configURL,
        persistRootURL: clientConfig.persistRootURL,
        tls: clientConfig.tls
    )
    let fileURL = try writeRealPayload(
        Data("aliyun-real-from-config-payload-\(UUID().uuidString)".utf8),
        under: clientConfig.persistRootURL,
        name: "aliyun-real-from-config"
    )

    let result = try await client.upload(
        UploadRequest(fileURL: fileURL, clientHints: ["suite": "aliyun-real"], rawTags: ["runtime": "from-config"], displayName: "from-config")
    )

    #expect(result.bucket == expectation.bucket)
    #expect(result.objectKey.hasPrefix(expectation.objectPrefix))
    #expect(!result.ossObjectETag.isEmpty)
    try assertNoStagingDataCopies(under: clientConfig.persistRootURL)
}

@Test(
    .enabled(if: realRuntimeIntegrationEnabled)
) func realAliyunResumeUploadFromPendingSnapshotFlow() async throws {
    let environment = AliyunOSSTestEnvironment()
    try environment.validate()
    let expectation = try environment.remoteUploadExpectation()
    let clientConfig = try uniqueRealClientConfig(from: environment.makeRemoteClientConfig(), label: "resume")
    defer { try? FileManager.default.removeItem(at: clientConfig.persistRootURL) }
    let harness = try makeRealGatewayHarness(clientConfig: clientConfig)
    let state = try await seedActiveRealUploadSnapshot(
        clientConfig: clientConfig,
        gatewayClient: harness.gatewayClient,
        payload: Data("aliyun-real-resume-payload-\(UUID().uuidString)".utf8),
        label: "resume"
    )
    let client = try DataGatewayClient(config: clientConfig)

    let pendingBeforeResume = try await client.listPendingUploads()
    #expect(pendingBeforeResume.contains(where: { $0.logicalUploadID == state.logicalUploadID && $0.uploadID == state.uploadID }))

    let result = try await client.resumeUpload(logicalUploadID: state.logicalUploadID)

    #expect(result.logicalUploadID == state.logicalUploadID)
    #expect(result.fileSize == state.fileSize)
    #expect(result.bucket == expectation.bucket)
    #expect(result.objectKey.hasPrefix(expectation.objectPrefix))
    #expect(!result.ossObjectETag.isEmpty)
    try assertNoStagingDataCopies(under: clientConfig.persistRootURL)
    let pendingAfterResume = try await client.listPendingUploads()
    #expect(!pendingAfterResume.contains(where: { $0.logicalUploadID == state.logicalUploadID }))
}

@Test(
    .enabled(if: realRuntimeIntegrationEnabled)
) func realAliyunAbortAndDeleteLocalSnapshotFlow() async throws {
    let environment = AliyunOSSTestEnvironment()
    try environment.validate()
    let clientConfig = try uniqueRealClientConfig(from: environment.makeRemoteClientConfig(), label: "abort-delete")
    defer { try? FileManager.default.removeItem(at: clientConfig.persistRootURL) }
    let harness = try makeRealGatewayHarness(clientConfig: clientConfig)
    let client = try DataGatewayClient(config: clientConfig)

    let abortState = try await seedActiveRealUploadSnapshot(
        clientConfig: clientConfig,
        gatewayClient: harness.gatewayClient,
        payload: Data("aliyun-real-abort-payload-\(UUID().uuidString)".utf8),
        label: "abort"
    )
    #expect(try await client.listPendingUploads().contains(where: { $0.logicalUploadID == abortState.logicalUploadID }))
    try await client.abortUpload(logicalUploadID: abortState.logicalUploadID)
    #expect(!(try await client.listPendingUploads().contains(where: { $0.logicalUploadID == abortState.logicalUploadID })))

    let deleteState = try await seedActiveRealUploadSnapshot(
        clientConfig: clientConfig,
        gatewayClient: harness.gatewayClient,
        payload: Data("aliyun-real-delete-local-payload-\(UUID().uuidString)".utf8),
        label: "delete-local"
    )
    #expect(try await client.listPendingUploads().contains(where: { $0.logicalUploadID == deleteState.logicalUploadID }))
    try await client.deleteLocalSnapshot(logicalUploadID: deleteState.logicalUploadID)
    #expect(!(try await client.listPendingUploads().contains(where: { $0.logicalUploadID == deleteState.logicalUploadID })))

    let cleanupResponse = try await harness.gatewayClient.abortUpload(
        logicalUploadID: deleteState.logicalUploadID,
        reason: "cleanup after local snapshot deletion"
    )
    #expect(cleanupResponse.logicalUploadID == deleteState.logicalUploadID)
}

@Test(
    .enabled(if: realDeviceInitIntegrationEnabled)
) func realAliyunDeviceInitReinitAndFromConfigUploadFlow() async throws {
    let environment = AliyunOSSTestEnvironment()
    try environment.validate()
    let expectation = try environment.remoteUploadExpectation()
    let clientConfig = try uniqueRealClientConfig(from: environment.makeRemoteClientConfig(), label: "device-init")
    defer { try? FileManager.default.removeItem(at: clientConfig.persistRootURL) }
    let deviceID = try requiredValueFromEnvironment("DGW_REAL_DEVICE_ID")
    let paths = try QiongcheSDKPaths(rootURL: clientConfig.persistRootURL)
    let configString = try realQiongcheConfigString(deviceID: deviceID, clientConfig: clientConfig)
    let qiongcheSDK = try QiongcheDataGatewaySDK(
        rootURL: clientConfig.persistRootURL,
        deviceInitTimeout: clientConfig.requestTimeout,
        readinessTimeout: clientConfig.requestTimeout
    )

    try await qiongcheSDK.saveConfigAndInit(configString: configString)
    let initializedConfig = try await ArchebaseConfigStore(configURL: paths.configURL).load()
    #expect(!initializedConfig.apiKey.isEmpty)
    #expect(try QiongcheSDKStateStore(stateURL: paths.stateURL).load().deviceID == deviceID)

    try await qiongcheSDK.saveConfigAndInit(configString: configString)
    let reinitializedConfig = try await ArchebaseConfigStore(configURL: paths.configURL).load()
    #expect(!reinitializedConfig.apiKey.isEmpty)
    #expect(try QiongcheSDKStateStore(stateURL: paths.stateURL).load().deviceID == deviceID)
    #expect(await qiongcheSDK.isReadyToUpload())

    let client = try await DataGatewayClient.fromArchebaseConfig(
        configURL: paths.configURL,
        persistRootURL: paths.persistRootURL,
        endpointsURL: paths.endpointsURL
    )
    let fileURL = try writeRealPayload(
        Data("aliyun-real-device-init-payload-\(UUID().uuidString)".utf8),
        under: paths.persistRootURL,
        name: "aliyun-real-device-init"
    )
    let result = try await client.upload(
        UploadRequest(fileURL: fileURL, clientHints: ["suite": "aliyun-real-device-init"], rawTags: ["runtime": "device-init"], displayName: "device-init")
    )

    #expect(result.bucket == expectation.bucket)
    #expect(result.objectKey.hasPrefix(expectation.objectPrefix))
    #expect(!result.ossObjectETag.isEmpty)
    try assertNoStagingDataCopies(under: paths.persistRootURL)
}

}

private func uniqueConfigURL(from baseURL: URL) -> URL {
    baseURL
        .deletingLastPathComponent()
        .appendingPathComponent("archebase-config-\(UUID().uuidString).json")
}

private struct RealGatewayHarness {
    let authTransport: ManagedControlPlaneServiceClient<any CredentialExchangeTransport>
    let gatewayTransport: ManagedControlPlaneServiceClient<Archebase_DataGateway_V1_DataGatewayService.Client<HTTP2ClientTransport.TransportServices>>
    let gatewayClient: AnyUploadCoordinatorGatewayClient
}

private func uniqueRealClientConfig(from config: DataGatewayClientConfig, label: String) throws -> DataGatewayClientConfig {
    var copy = config
    let originalPersistRoot = config.persistRootURL
    copy.persistRootURL = config.persistRootURL
        .appendingPathComponent("aliyun-real-\(label)-\(UUID().uuidString)", isDirectory: true)
    try FileManager.default.createDirectory(at: copy.persistRootURL, withIntermediateDirectories: true)
    let originalEndpointsURL = originalPersistRoot.appendingPathComponent(ArchebasePublicEndpoints.endpointsFileName)
    if FileManager.default.fileExists(atPath: originalEndpointsURL.path) {
        try FileManager.default.copyItem(
            at: originalEndpointsURL,
            to: copy.persistRootURL.appendingPathComponent(ArchebasePublicEndpoints.endpointsFileName)
        )
    }
    return copy
}

private func writeRealPayload(_ payload: Data, under root: URL, name: String) throws -> URL {
    try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
    let fileURL = root
        .appendingPathComponent("\(name)-\(UUID().uuidString)")
        .appendingPathExtension("bin")
    try payload.write(to: fileURL)
    return fileURL
}

private func assertNoStagingDataCopies(under persistRoot: URL) throws {
    let stagingRoot = persistRoot
        .appendingPathComponent("data-gateway-client", isDirectory: true)
        .appendingPathComponent("staging", isDirectory: true)
    guard FileManager.default.fileExists(atPath: stagingRoot.path) else {
        return
    }

    let stagedItems = try FileManager.default.contentsOfDirectory(
        at: stagingRoot,
        includingPropertiesForKeys: nil
    )
    #expect(stagedItems.isEmpty)
}

private func realQiongcheConfigString(
    deviceID: String,
    clientConfig: DataGatewayClientConfig
) throws -> String {
    let object: [String: Any] = [
        "device_id": deviceID,
        "auth": try endpointJSONObject(clientConfig.authEndpoint),
        "gateway": try endpointJSONObject(clientConfig.gatewayEndpoint),
        "deviceInit": try endpointJSONObject(realDeviceInitEndpoint(clientConfig: clientConfig)),
    ]
    let data = try JSONSerialization.data(withJSONObject: object, options: [.sortedKeys])
    guard let string = String(data: data, encoding: .utf8) else {
        throw DataGatewayClientError.invalidConfiguration("failed to encode real qiongche config")
    }
    return string
}

private func endpointJSONObject(_ endpoint: URL) throws -> [String: Any] {
    guard let scheme = endpoint.scheme?.lowercased(), !scheme.isEmpty else {
        throw LocalStackHarnessError.invalidEndpoint(endpoint.absoluteString)
    }
    guard let host = endpoint.host(percentEncoded: false) ?? endpoint.host, !host.isEmpty else {
        throw LocalStackHarnessError.invalidEndpoint(endpoint.absoluteString)
    }
    let port = endpoint.port ?? (scheme == "https" ? 443 : 80)
    return ["scheme": scheme, "host": host, "port": port]
}

private func realDeviceInitEndpoint(clientConfig: DataGatewayClientConfig) throws -> URL {
    if let endpoint = try optionalURLFromEnvironment("DGW_REAL_INIT_ENDPOINT") {
        return endpoint
    }
    if let endpoint = try optionalURLFromEnvironment("DGW_REAL_DEVICE_INIT_ENDPOINT") {
        return endpoint
    }
    return clientConfig.gatewayEndpoint
}

private func canonicalObjectETag(_ value: String) -> String {
    let trimmed = value.trimmingCharacters(in: .whitespacesAndNewlines)
    if trimmed.count >= 2, trimmed.first == "\"", trimmed.last == "\"" {
        return String(trimmed.dropFirst().dropLast())
    }
    return trimmed
}

private func realMultipartPayloadSizeBytes() -> Int {
    if let value = ProcessInfo.processInfo.environment["DGW_REAL_MULTIPART_SIZE_BYTES"],
        let parsed = Int(value),
        parsed > 0 {
        return parsed
    }
    return 67_108_864 + 1024
}

private func realPartSizePayloadSizeBytes() -> Int {
    if let value = ProcessInfo.processInfo.environment["DGW_REAL_PART_SIZE_BYTES"],
        let parsed = Int(value),
        parsed > 0 {
        return parsed
    }
    return 67_108_864
}

private func assertPutObjectUploadEvents(_ events: [UploadEvent]) {
    #expect(events.contains(.preparing))
    #expect(events.contains(.authenticating))
    #expect(events.contains(.creatingLogicalUpload))
    #expect(uploadEventCount(events, matching: isUploadingPartEvent) == 1)
    #expect(events.contains(where: isCompletingBusinessUploadEvent))
    #expect(events.last.map(isCompletedEvent) ?? false)
    #expect(!events.contains(where: isInitiatingMultipartEvent))
    #expect(!events.contains(where: isCompletingMultipartEvent))
}

private func completedUploadResult(from events: [UploadEvent]) -> UploadResult? {
    guard case .completed(let result) = events.last else {
        return nil
    }
    return result
}

private func uploadEventCount(_ events: [UploadEvent], matching matcher: (UploadEvent) -> Bool) -> Int {
    events.reduce(0) { count, event in
        matcher(event) ? count + 1 : count
    }
}

private func isInitiatingMultipartEvent(_ event: UploadEvent) -> Bool {
    if case .initiatingMultipart = event {
        return true
    }
    return false
}

private func isUploadingPartEvent(_ event: UploadEvent) -> Bool {
    if case .uploadingPart = event {
        return true
    }
    return false
}

private func isCompletingMultipartEvent(_ event: UploadEvent) -> Bool {
    if case .completingMultipart = event {
        return true
    }
    return false
}

private func isCompletingBusinessUploadEvent(_ event: UploadEvent) -> Bool {
    if case .completingBusinessUpload = event {
        return true
    }
    return false
}

private func isCompletedEvent(_ event: UploadEvent) -> Bool {
    if case .completed = event {
        return true
    }
    return false
}

private func hasRealUserAuthorizationHeader() -> Bool {
    do {
        _ = try realUserAuthorizationHeader()
        return true
    } catch {
        return false
    }
}

private func realUserAuthorizationHeader() throws -> String {
    if let header = ProcessInfo.processInfo.environment["DGW_REAL_USER_AUTHORIZATION_HEADER"]?.trimmingCharacters(in: .whitespacesAndNewlines),
        !header.isEmpty {
        return header
    }
    if let token = ProcessInfo.processInfo.environment["DGW_REAL_USER_ACCESS_TOKEN"]?.trimmingCharacters(in: .whitespacesAndNewlines),
        !token.isEmpty {
        if token.lowercased().hasPrefix("bearer ") {
            return token
        }
        return "Bearer \(token)"
    }
    throw AliyunOSSHarnessError.missingEnvironmentVariable("DGW_REAL_USER_AUTHORIZATION_HEADER")
}

private func makeRealGatewayHarness(clientConfig: DataGatewayClientConfig) throws -> RealGatewayHarness {
    let security: ControlPlaneTransportSecurity = switch clientConfig.tls {
    case .plaintext: .plaintext
    case .tls: .tls
    }
    let authFactory = ControlPlaneClientFactory(
        configuration: ControlPlaneTransportConfiguration(
            endpoint: clientConfig.authEndpoint,
            security: security,
            requestTimeout: clientConfig.requestTimeout
        )
    )
    let authTransport = try authFactory.makeAuthTransport()
    let authProvider = CredentialAuthProvider(
        credentialBase64: clientConfig.credentialBase64,
        refreshBefore: clientConfig.authRefreshBefore,
        requestTimeout: clientConfig.requestTimeout,
        transport: authTransport.serviceClient
    )
    let gatewayTransport = try ManagedControlPlaneServiceClient(
        configuration: ControlPlaneTransportConfiguration(
            endpoint: clientConfig.gatewayEndpoint,
            security: security,
            requestTimeout: clientConfig.requestTimeout
        )
    ) { grpcClient in
        Archebase_DataGateway_V1_DataGatewayService.Client(wrapping: grpcClient)
    }
    let gatewayClient = AnyUploadCoordinatorGatewayClient(
        authProvider: authProvider,
        gatewayServiceClient: gatewayTransport.serviceClient,
        requestTimeout: clientConfig.requestTimeout,
        retryPolicy: clientConfig.retryPolicy.controlPlane.controlPlaneValue
    )
    return RealGatewayHarness(authTransport: authTransport, gatewayTransport: gatewayTransport, gatewayClient: gatewayClient)
}

private func seedActiveRealUploadSnapshot(
    clientConfig: DataGatewayClientConfig,
    gatewayClient: AnyUploadCoordinatorGatewayClient,
    payload: Data,
    label: String
) async throws -> PersistedUploadState {
    let sourceURL = try writeRealPayload(payload, under: clientConfig.persistRootURL, name: "aliyun-real-\(label)-source")
    let fileCoordinator = FileStagingCoordinator(
        stagingRoot: clientConfig.persistRootURL
            .appendingPathComponent("data-gateway-client", isDirectory: true)
            .appendingPathComponent("staging", isDirectory: true)
    )
    let request = UploadRequest(
        fileURL: sourceURL,
        clientHints: ["suite": "aliyun-real", "mode": label],
        rawTags: ["suite": "aliyun-real", "runtime": label],
        displayName: "aliyun-real-\(label)"
    )
    let prepared = try fileCoordinator.prepare(
        request: request,
        persistence: LocalPersistencePolicy(
            keepTerminalSnapshot: clientConfig.execution.persistence.keepTerminalSnapshot,
            keepCompletedSnapshot: clientConfig.execution.persistence.keepCompletedSnapshot,
            completedSnapshotTTL: clientConfig.execution.persistence.completedSnapshotTTL,
            terminalSnapshotTTL: clientConfig.execution.persistence.terminalSnapshotTTL,
            copyExternalFileIntoManagedStaging: false
        )
    )
    let response = try await gatewayClient.createLogicalUpload(clientHints: request.clientHints, restartFromUploadID: nil)
    let createdAt = Date()
    let state = PersistedUploadState(
        version: 1,
        logicalUploadID: response.logicalUploadID,
        uploadID: response.uploadID,
        restartCount: 0,
        multipartUploadID: nil,
        bucket: response.credentials.bucket,
        endpoint: response.credentials.endpoint,
        objectKey: response.credentials.objectKey,
        fileURLBookmarkData: prepared.bookmarkData,
        managedFileURL: prepared.managedFileURL,
        fileSize: prepared.fileSize,
        fileFingerprint: prepared.fingerprint,
        partSizeBytes: UInt64(response.credentials.partSizeBytes),
        uploadedParts: [],
        clientHints: request.clientHints,
        rawTags: request.rawTags,
        phase: .sessionCreated,
        lastKnownSTSExpireAt: Date(timeIntervalSince1970: TimeInterval(response.credentials.stsExpireAtUnix)),
        createdAt: createdAt,
        updatedAt: createdAt
    )
    try await UploadStateStore(persistRoot: clientConfig.persistRootURL).saveActive(state)
    return state
}

private func requiredValueFromEnvironment(_ key: String) throws -> String {
    guard let value = ProcessInfo.processInfo.environment[key]?.trimmingCharacters(in: .whitespacesAndNewlines), !value.isEmpty else {
        throw AliyunOSSHarnessError.missingEnvironmentVariable(key)
    }
    return value
}

private func requiredURLFromEnvironment(_ key: String) throws -> URL {
    let value = try requiredValueFromEnvironment(key)
    guard let url = normalizedURLFromEnvironmentValue(value) else {
        throw LocalStackHarnessError.invalidEndpoint(key)
    }
    return url
}

private func optionalURLFromEnvironment(_ key: String) throws -> URL? {
    guard let value = ProcessInfo.processInfo.environment[key]?.trimmingCharacters(in: .whitespacesAndNewlines),
          !value.isEmpty else {
        return nil
    }
    guard let url = normalizedURLFromEnvironmentValue(value) else {
        throw LocalStackHarnessError.invalidEndpoint(key)
    }
    return url
}

private func normalizedURLFromEnvironmentValue(_ value: String) -> URL? {
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
