import Foundation
import Testing

import DGWControlPlane
import DGWProto
import DGWStore
@testable import DataGatewayClient

@Test func initDeviceRejectsWhenConfigExistsWithoutRemoteCall() async throws {
    let configURL = temporaryConfigURL()
    let store = ArchebaseConfigStore(configURL: configURL)
    try await store.initialize(ArchebaseConfig(apiKey: "credential-existing", tags: [:]))
    let transport = RecordingDeviceInitTransport(response: .success(response(apiKey: "credential-new")))
    let initializer = try ArchebaseDeviceInitializer(configStore: store, initTransport: transport)

    let error = await #expect(throws: DataGatewayClientError.self) {
        _ = try await initializer.initDevice(deviceID: "260427-000001")
    }

    #expect(error == .alreadyInitialized(configURL: configURL.standardizedFileURL))
    #expect(await transport.requests().isEmpty)
}

@Test func initDeviceWritesConfigAfterRemoteSuccess() async throws {
    let configURL = temporaryConfigURL()
    let store = ArchebaseConfigStore(configURL: configURL)
    let transport = RecordingDeviceInitTransport(response: .success(response(apiKey: "credential-v1", tags: ["device": "robot"])))
    let initializer = try ArchebaseDeviceInitializer(
        configStore: store,
        initTransport: transport,
        sdkVersion: "1.2.3",
        platform: "ios-simulator"
    )

    let config = try await initializer.initDevice(deviceID: "260427-000001")

    let expected = try ArchebaseConfig(apiKey: "credential-v1", tags: ["device": "robot"])
    #expect(config == expected)
    #expect(try await store.load() == config)
    #expect(await transport.requests() == [
        DeviceInitRequestRecord(
            method: .initDevice,
            deviceID: "260427-000001",
            sdkVersion: "1.2.3",
            platform: "ios-simulator"
        ),
    ])
}

@Test func initDeviceDoesNotFallbackWhenRemoteAlreadyInitialized() async throws {
    let store = ArchebaseConfigStore(configURL: temporaryConfigURL())
    let transport = RecordingDeviceInitTransport(
        response: .failure(
            DataGatewayClientError.gatewayFailed(
                statusCode: 9,
                detailCode: DeviceInitGatewayDetailCode.alreadyInitialized,
                message: "device has already been initialized"
            )
        )
    )
    let initializer = try ArchebaseDeviceInitializer(configStore: store, initTransport: transport)

    let error = await #expect(throws: DataGatewayClientError.self) {
        _ = try await initializer.initDevice(deviceID: "260427-000001")
    }

    #expect(error == .gatewayFailed(
        statusCode: 9,
        detailCode: DeviceInitGatewayDetailCode.alreadyInitialized,
        message: "device has already been initialized"
    ))
    #expect(await transport.requests().map(\.method) == [.initDevice])
}

@Test func reinitDeviceCanRecoverWhenConfigMissing() async throws {
    let store = ArchebaseConfigStore(configURL: temporaryConfigURL())
    let transport = RecordingDeviceInitTransport(response: .success(response(apiKey: "credential-new")))
    let initializer = try ArchebaseDeviceInitializer(configStore: store, initTransport: transport)

    let config = try await initializer.reinitDevice(deviceID: "260427-000001")

    #expect(config == (try ArchebaseConfig(apiKey: "credential-new", tags: [:])))
    #expect(try await store.load() == config)
    #expect(await transport.requests().map(\.method) == [.reinitDevice])
}

@Test func reinitDeviceReplacesExistingConfig() async throws {
    let configURL = temporaryConfigURL()
    let store = ArchebaseConfigStore(configURL: configURL)
    try await store.initialize(ArchebaseConfig(apiKey: "credential-v1", tags: ["device": "old"]))
    let transport = RecordingDeviceInitTransport(response: .success(response(apiKey: "credential-v2", tags: ["device": "new"])))
    let initializer = try ArchebaseDeviceInitializer(configStore: store, initTransport: transport)

    let config = try await initializer.reinitDevice(deviceID: "260427-000001")

    let expected = try ArchebaseConfig(apiKey: "credential-v2", tags: ["device": "new"])
    #expect(config == expected)
    #expect(try await store.load() == config)
    #expect(await transport.requests().map(\.method) == [.reinitDevice])
}

@Test func reinitDeviceDoesNotFallbackWhenRemoteNotInitialized() async throws {
    let store = ArchebaseConfigStore(configURL: temporaryConfigURL())
    try await store.initialize(ArchebaseConfig(apiKey: "credential-v1", tags: [:]))
    let transport = RecordingDeviceInitTransport(
        response: .failure(
            DataGatewayClientError.gatewayFailed(
                statusCode: 9,
                detailCode: DeviceInitGatewayDetailCode.notInitialized,
                message: "device has not been initialized"
            )
        )
    )
    let initializer = try ArchebaseDeviceInitializer(configStore: store, initTransport: transport)

    let error = await #expect(throws: DataGatewayClientError.self) {
        _ = try await initializer.reinitDevice(deviceID: "260427-000001")
    }

    #expect(error == .gatewayFailed(
        statusCode: 9,
        detailCode: DeviceInitGatewayDetailCode.notInitialized,
        message: "device has not been initialized"
    ))
    #expect(await transport.requests().map(\.method) == [.reinitDevice])
}

private enum DeviceInitRequestMethod: Sendable, Equatable {
    case initDevice
    case reinitDevice
}

private struct DeviceInitRequestRecord: Sendable, Equatable {
    let method: DeviceInitRequestMethod
    let deviceID: String
    let sdkVersion: String
    let platform: String
}

private actor RecordingDeviceInitTransport: DeviceInitTransport {
    private let response: Result<Archebase_DataGateway_V1_InitDeviceResponse, Error>
    private var records: [DeviceInitRequestRecord] = []

    init(response: Result<Archebase_DataGateway_V1_InitDeviceResponse, Error>) {
        self.response = response
    }

    func initDevice(
        deviceID: String,
        sdkVersion: String,
        platform: String
    ) async throws -> Archebase_DataGateway_V1_InitDeviceResponse {
        self.records.append(DeviceInitRequestRecord(method: .initDevice, deviceID: deviceID, sdkVersion: sdkVersion, platform: platform))
        return try self.response.get()
    }

    func reinitDevice(
        deviceID: String,
        sdkVersion: String,
        platform: String
    ) async throws -> Archebase_DataGateway_V1_InitDeviceResponse {
        self.records.append(DeviceInitRequestRecord(method: .reinitDevice, deviceID: deviceID, sdkVersion: sdkVersion, platform: platform))
        return try self.response.get()
    }

    func requests() -> [DeviceInitRequestRecord] {
        self.records
    }
}

private func response(apiKey: String, tags: [String: String] = [:]) -> Archebase_DataGateway_V1_InitDeviceResponse {
    var response = Archebase_DataGateway_V1_InitDeviceResponse()
    response.apiKey = apiKey
    response.tags = tags
    return response
}

private func temporaryConfigURL() -> URL {
    FileManager.default.temporaryDirectory
        .appendingPathComponent("swift-dgw-device-init-\(UUID().uuidString)", isDirectory: true)
        .appendingPathComponent("archebase-config.json")
}
