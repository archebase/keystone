import DGWControlPlane
import DGWOss
import DGWProto
import DGWStore
import Foundation
import Testing

@testable import DataGatewayClient

@Test func fromArchebaseConfigRejectsMissingConfig() async throws {
    let root = try temporaryRoot()
    let configURL = root.appendingPathComponent("archebase-config.json")
    let endpointsURL = root.appendingPathComponent(ArchebasePublicEndpoints.endpointsFileName)

    let error = await #expect(throws: DataGatewayClientError.self) {
        _ = try await DataGatewayClient.fromArchebaseConfig(
            configURL: configURL,
            persistRootURL: root,
            endpointsURL: endpointsURL
        )
    }

    #expect(error == .notInitialized(configURL: configURL.standardizedFileURL))
}

@Test func fromArchebaseConfigThrowsMissingEndpointsAfterConfigExists() async throws {
    let root = try temporaryRoot()
    let configURL = root.appendingPathComponent("archebase-config.json")
    let endpointsURL = root.appendingPathComponent(ArchebasePublicEndpoints.endpointsFileName)
    let config = try ArchebaseConfig(apiKey: "credential-base64", tags: ["device": "robot"])
    try await ArchebaseConfigStore(configURL: configURL).initialize(config)

    let error = await #expect(throws: DataGatewayClientError.self) {
        _ = try await DataGatewayClient.fromArchebaseConfig(
            configURL: configURL,
            persistRootURL: root,
            endpointsURL: endpointsURL
        )
    }

    #expect(error == .endpointsNotInitialized(endpointsURL: endpointsURL.standardizedFileURL))
}

@Test func fromArchebaseConfigBuildsPublicEndpointClient() async throws {
    let root = try temporaryRoot()
    let configURL = root.appendingPathComponent("archebase-config.json")
    let endpointsURL = root.appendingPathComponent(ArchebasePublicEndpoints.endpointsFileName)
    let config = try ArchebaseConfig(apiKey: "credential-base64", tags: ["device": "robot"])
    try await ArchebaseConfigStore(configURL: configURL).initialize(config)
    try DataGatewayClient.initialize(endpointsJSON: configClientEndpointsJSON(), endpointsURL: endpointsURL)

    _ = try await DataGatewayClient.fromArchebaseConfig(
        configURL: configURL,
        persistRootURL: root,
        endpointsURL: endpointsURL
    )
}

@Test func rawTagsMergerMergesConfigAndUploadTags() throws {
    let merged = try RawTagsMerger.merge(
        configTags: ["device": "robot", "site": "shanghai"],
        uploadRawTags: ["scene": "pick"]
    )

    #expect(merged == ["device": "robot", "site": "shanghai", "scene": "pick"])
}

@Test func rawTagsMergerAcceptsSameKeySameValue() throws {
    let merged = try RawTagsMerger.merge(
        configTags: ["device": "robot"],
        uploadRawTags: ["device": "robot", "scene": "pick"]
    )

    #expect(merged == ["device": "robot", "scene": "pick"])
}

@Test func rawTagsMergerRejectsSameKeyDifferentValue() {
    let error = #expect(throws: DataGatewayClientError.self) {
        _ = try RawTagsMerger.merge(
            configTags: ["device": "robot"],
            uploadRawTags: ["device": "camera"]
        )
    }

    #expect(error == .rawTagConflict(key: "device"))
}

@Test func rawTagsMergerRejectsTooManyTags() {
    let configTags = Dictionary(uniqueKeysWithValues: (0 ..< 256).map { ("c\($0)", "v") })

    let error = #expect(throws: DataGatewayClientError.self) {
        _ = try RawTagsMerger.merge(configTags: configTags, uploadRawTags: ["extra": "v"])
    }

    #expect(error == .invalidConfiguration("raw_tags exceeds the allowed maximum item count of 256"))
}

@Test func rawTagsMergerAddsSourceFileNameTags() throws {
    let sourceURL = URL(fileURLWithPath: "/captures/raw/demo.mcap")

    let merged = try RawTagsMerger.merge(configTags: [:], uploadRawTags: [:], sourceFileURL: sourceURL)

    #expect(merged[RawTagsMerger.sourceFileNameRawTagKey] == "demo.mcap")
}

@Test func rawTagsMergerRejectsConflictingSourceFileNameTag() {
    let sourceURL = URL(fileURLWithPath: "/captures/raw/demo.mcap")

    let error = #expect(throws: DataGatewayClientError.self) {
        _ = try RawTagsMerger.merge(
            configTags: [:],
            uploadRawTags: [RawTagsMerger.sourceFileNameRawTagKey: "other.mcap"],
            sourceFileURL: sourceURL
        )
    }

    #expect(error == .rawTagConflict(key: RawTagsMerger.sourceFileNameRawTagKey))
}

@Test func uploadPersistsMergedConfigTags() async throws {
    let root = try temporaryRoot()
    let sourceURL = root.appendingPathComponent("demo.bin")
    try Data("robot-data".utf8).write(to: sourceURL)
    let stateStore = UploadStateStore(persistRoot: root)
    let gateway = RecordingGatewayClient()
    let coordinator = UploadCoordinator(
        executionPolicy: makeTestExecutionPolicy(),
        dependencies: UploadCoordinatorDependencies(
            gatewayClient: gateway,
            stateStore: stateStore,
            fileCoordinator: FileStagingCoordinator(stagingRoot: root.appendingPathComponent("staging", isDirectory: true)),
            ossClientFactory: { _ in FakeMultipartSession() }
        )
    )
    let client = DataGatewayClient(uploadCoordinator: coordinator, configTags: ["device": "robot"])

    _ = try await client.upload(
        UploadRequest(fileURL: sourceURL, clientHints: [:], rawTags: ["scene": "pick"], displayName: nil)
    )

    #expect(await gateway.completedRawTags == [
        "device": "robot",
        "scene": "pick",
        RawTagsMerger.sourceFileNameRawTagKey: "demo.bin",
    ])
    let pending = try await stateStore.listPendingUploads()
    #expect(pending.isEmpty)
}

@Test func listObjectsUsesUserAuthorizationAndMapsPage() async throws {
    let root = try temporaryRoot()
    let objectClient = RecordingObjectClient()
    let coordinator = UploadCoordinator(
        executionPolicy: makeTestExecutionPolicy(),
        dependencies: UploadCoordinatorDependencies(
            gatewayClient: RecordingGatewayClient(),
            stateStore: UploadStateStore(persistRoot: root),
            fileCoordinator: FileStagingCoordinator(stagingRoot: root.appendingPathComponent("staging", isDirectory: true)),
            ossClientFactory: { _ in FakeMultipartSession() }
        )
    )
    let client = DataGatewayClient(uploadCoordinator: coordinator, objectClient: objectClient)

    let page = try await client.listObjects(
        ListObjectsOptions(pageSize: 50, pageToken: "page-1", filter: "status:verified"),
        authorizationHeader: "Bearer user-token"
    )

    #expect(page.nextPageToken == "page-2")
    #expect(page.objects == [
        DataObject(
            objectID: "object-1",
            fileID: "file-1",
            status: .verified,
            sizeBytes: 1024,
            createdAtUnix: 1_700_000_001,
            uploadedAtUnix: 1_700_000_002,
            verifiedAtUnix: 1_700_000_003,
            etag: "\"etag-1\""
        ),
    ])
    #expect(await objectClient.lastCall == "50:page-1:status:verified:Bearer user-token")
}

@Test func listObjectsRejectsBlankAuthorizationHeaderLocally() async throws {
    let root = try temporaryRoot()
    let objectClient = RecordingObjectClient()
    let coordinator = UploadCoordinator(
        executionPolicy: makeTestExecutionPolicy(),
        dependencies: UploadCoordinatorDependencies(
            gatewayClient: RecordingGatewayClient(),
            stateStore: UploadStateStore(persistRoot: root),
            fileCoordinator: FileStagingCoordinator(stagingRoot: root.appendingPathComponent("staging", isDirectory: true)),
            ossClientFactory: { _ in FakeMultipartSession() }
        )
    )
    let client = DataGatewayClient(uploadCoordinator: coordinator, objectClient: objectClient)

    let error = await #expect(throws: DataGatewayClientError.self) {
        _ = try await client.listObjects(authorizationHeader: " \n\t ")
    }

    #expect(error == .invalidConfiguration("authorization header is required"))
    #expect(await objectClient.lastCall == nil)
}

private func temporaryRoot() throws -> URL {
    let root = FileManager.default.temporaryDirectory
        .appendingPathComponent("archebase-client-config-tests", isDirectory: true)
        .appendingPathComponent(UUID().uuidString, isDirectory: true)
    try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
    return root
}

private func configClientEndpointsJSON() -> String {
    """
    {
      "auth": { "scheme": "http", "host": "auth.example.com", "port": 50051 },
      "gateway": { "scheme": "http", "host": "gateway.example.com", "port": 50053 },
      "deviceInit": { "scheme": "https", "host": "init.example.com", "port": 443 }
    }
    """
}

private func makeTestExecutionPolicy() -> UploadExecutionPolicy {
    UploadExecutionPolicy(
        maxRestartCount: 1,
        autoResumeByFileURL: true,
        reconcileRemotePartsOnResume: true,
        cleanupOnTerminalFailure: true,
        credentialRefreshSkew: .seconds(30),
        persistence: LocalPersistencePolicy(
            keepTerminalSnapshot: true,
            keepCompletedSnapshot: false,
            completedSnapshotTTL: .seconds(0),
            terminalSnapshotTTL: .seconds(3600),
            copyExternalFileIntoManagedStaging: true
        )
    )
}

private actor RecordingObjectClient: ObjectControlPlaneClientProtocol {
    private(set) var lastCall: String?

    func listObjects(
        pageSize: Int32,
        pageToken: String,
        filter: String,
        authorizationHeader: String
    ) async throws -> Archebase_DataGateway_V1_ListObjectsResponse {
        self.lastCall = "\(pageSize):\(pageToken):\(filter):\(authorizationHeader)"

        var object = Archebase_DataGateway_V1_DataObject()
        object.objectID = "object-1"
        object.fileID = "file-1"
        object.status = .verified
        object.sizeBytes = 1024
        object.createdAtUnix = 1_700_000_001
        object.uploadedAtUnix = 1_700_000_002
        object.verifiedAtUnix = 1_700_000_003
        object.etag = "\"etag-1\""

        var response = Archebase_DataGateway_V1_ListObjectsResponse()
        response.objects = [object]
        response.nextPageToken = "page-2"
        return response
    }
}

private actor RecordingGatewayClient: UploadCoordinatorGatewayClient {
    private(set) var completedRawTags: [String: String] = [:]

    func createLogicalUpload(
        clientHints: [String : String],
        restartFromUploadID: String?
    ) async throws -> Archebase_DataGateway_V1_CreateLogicalUploadResponse {
        var credentials = Archebase_DataGateway_V1_UploadCredentials()
        credentials.bucket = "bucket"
        credentials.endpoint = "https://oss.example.com"
        credentials.objectKey = "objects/upload-1"
        credentials.stsAccessKeyID = "ak"
        credentials.stsAccessKeySecret = "sk"
        credentials.stsSecurityToken = "token"
        credentials.stsExpireAtUnix = Int64(Date().addingTimeInterval(3600).timeIntervalSince1970)
        credentials.partSizeBytes = 1024

        var response = Archebase_DataGateway_V1_CreateLogicalUploadResponse()
        response.logicalUploadID = "logical-1"
        response.uploadID = "upload-1"
        response.credentials = credentials
        return response
    }

    func getUploadRecovery(logicalUploadID: String) async throws -> Archebase_DataGateway_V1_GetUploadRecoveryResponse {
        throw DataGatewayClientError.resumeNotPossible("not used")
    }

    func reissueUploadCredentials(uploadID: String) async throws -> Archebase_DataGateway_V1_ReissueUploadCredentialsResponse {
        throw DataGatewayClientError.resumeNotPossible("not used")
    }

    func abortUpload(logicalUploadID: String, reason: String) async throws -> Archebase_DataGateway_V1_AbortUploadResponse {
        Archebase_DataGateway_V1_AbortUploadResponse()
    }

    func completeUpload(
        uploadID: String,
        fileSize: Int64,
        rawTags: [String : String],
        completedPartCount: Int32,
        ossObjectEtag: String,
        partSizeBytes: Int64
    ) async throws -> Archebase_DataGateway_V1_CompleteUploadResponse {
        self.completedRawTags = rawTags
        return Archebase_DataGateway_V1_CompleteUploadResponse()
    }
}

private actor FakeMultipartSession: UploadCoordinatorMultipartSessionProtocol {
    func ensureFreshCredentialsIfNeeded() async throws -> Bool { false }

    func lastKnownCredentialExpiration() async -> Date? { nil }

    func initiateMultipartUpload() async throws -> String { "multipart-1" }

    func uploadPart(multipartUploadID: String, partNumber: Int, body: OssUploadBody) async throws -> UploadedPartDescriptor {
        UploadedPartDescriptor(partNumber: partNumber, etag: "\"etag-\(partNumber)\"", size: body.sizeBytes, lastModified: nil, hashCRC64: nil)
    }

    func putObject(body: OssUploadBody) async throws -> UploadedPartDescriptor {
        UploadedPartDescriptor(partNumber: 1, etag: "\"etag-object\"", size: body.sizeBytes, lastModified: nil, hashCRC64: nil)
    }

    func listParts(multipartUploadID: String) async throws -> [UploadedPartDescriptor] { [] }

    func headObjectETag() async throws -> String { "\"etag-object\"" }

    func completeMultipartUpload(multipartUploadID: String, parts: [UploadedPartDescriptor]) async throws -> String {
        "\"etag-object\""
    }
}
