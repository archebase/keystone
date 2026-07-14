import DGWControlPlane
import Foundation
import Testing

@testable import DGWStore

@Test func loadMissingFileReturnsNotInitialized() async throws {
    let configURL = try temporaryConfigURL()
    let store = ArchebaseConfigStore(configURL: configURL)

    let error = await #expect(throws: DataGatewayClientError.self) {
        _ = try await store.load()
    }

    #expect(error == .notInitialized(configURL: configURL.standardizedFileURL))
}

@Test func initializeWritesConfigWhenMissing() async throws {
    let configURL = try temporaryConfigURL()
    let store = ArchebaseConfigStore(configURL: configURL)
    let config = try ArchebaseConfig(apiKey: "credential-v1", tags: ["device": "robot"])

    try await store.initialize(config)

    #expect(try await store.load() == config)
    #expect(try String(contentsOf: configURL, encoding: .utf8).contains("\"api_key\""))
}

@Test func initializeRejectsWhenFileExists() async throws {
    let configURL = try temporaryConfigURL()
    let store = ArchebaseConfigStore(configURL: configURL)
    let config = try ArchebaseConfig(apiKey: "credential-v1", tags: [:])
    try await store.initialize(config)

    let error = await #expect(throws: DataGatewayClientError.self) {
        try await store.initialize(config)
    }

    #expect(error == .alreadyInitialized(configURL: configURL.standardizedFileURL))
}

@Test func replaceForReinitRejectsWhenMissing() async throws {
    let configURL = try temporaryConfigURL()
    let store = ArchebaseConfigStore(configURL: configURL)
    let config = try ArchebaseConfig(apiKey: "credential-v2", tags: [:])

    let error = await #expect(throws: DataGatewayClientError.self) {
        try await store.replaceForReinit(config)
    }

    #expect(error == .notInitialized(configURL: configURL.standardizedFileURL))
}

@Test func replaceForReinitReplacesExistingConfig() async throws {
    let configURL = try temporaryConfigURL()
    let store = ArchebaseConfigStore(configURL: configURL)
    let old = try ArchebaseConfig(apiKey: "credential-v1", tags: ["device": "old"])
    let new = try ArchebaseConfig(apiKey: "credential-v2", tags: ["device": "new"])
    try await store.initialize(old)

    try await store.replaceForReinit(new)

    #expect(try await store.load() == new)
}

@Test func replaceForReinitKeepsOldFileOnInvalidNewConfig() async throws {
    let configURL = try temporaryConfigURL()
    let store = ArchebaseConfigStore(configURL: configURL)
    let old = try ArchebaseConfig(apiKey: "credential-v1", tags: ["device": "old"])
    try await store.initialize(old)

    var invalid = old
    invalid.apiKey = " "
    _ = await #expect(throws: DataGatewayClientError.self) {
        try await store.replaceForReinit(invalid)
    }

    #expect(try await store.load() == old)
}

@Test func replaceOrInitializeWritesConfigWhenMissing() async throws {
    let configURL = try temporaryConfigURL()
    let store = ArchebaseConfigStore(configURL: configURL)
    let config = try ArchebaseConfig(apiKey: "credential-v1", tags: ["device": "robot"])

    try await store.replaceOrInitialize(config)

    #expect(try await store.load() == config)
}

@Test func replaceOrInitializeOverwritesExistingConfig() async throws {
    let configURL = try temporaryConfigURL()
    let store = ArchebaseConfigStore(configURL: configURL)
    let old = try ArchebaseConfig(apiKey: "credential-v1", tags: ["device": "old"])
    let new = try ArchebaseConfig(apiKey: "credential-v2", tags: ["device": "new"])
    try await store.initialize(old)

    try await store.replaceOrInitialize(new)

    #expect(try await store.load() == new)
}

@Test func replaceOrInitializeKeepsOldFileOnInvalidNewConfig() async throws {
    let configURL = try temporaryConfigURL()
    let store = ArchebaseConfigStore(configURL: configURL)
    let old = try ArchebaseConfig(apiKey: "credential-v1", tags: ["device": "old"])
    try await store.initialize(old)

    var invalid = old
    invalid.apiKey = " "
    _ = await #expect(throws: DataGatewayClientError.self) {
        try await store.replaceOrInitialize(invalid)
    }

    #expect(try await store.load() == old)
}

@Test func loadRejectsCorruptedJSON() async throws {
    let configURL = try temporaryConfigURL()
    try FileManager.default.createDirectory(at: configURL.deletingLastPathComponent(), withIntermediateDirectories: true)
    try Data("not-json".utf8).write(to: configURL)
    let store = ArchebaseConfigStore(configURL: configURL)

    let error = await #expect(throws: DataGatewayClientError.self) {
        _ = try await store.load()
    }

    if case .invalidConfiguration(let message) = error {
        #expect(message.contains("failed to load archebase config"))
    } else {
        Issue.record("expected invalidConfiguration, got \(String(describing: error))")
    }
}

@Test func atomicWriteLeavesParseableFile() async throws {
    let configURL = try temporaryConfigURL()
    let store = ArchebaseConfigStore(configURL: configURL)
    let config = try ArchebaseConfig(apiKey: "credential-v1", tags: ["device": "robot"])

    try await store.initialize(config)

    let data = try Data(contentsOf: configURL)
    #expect(try ArchebaseConfig.decodeValidated(from: data) == config)
}

private func temporaryConfigURL() throws -> URL {
    let root = FileManager.default.temporaryDirectory
        .appendingPathComponent("archebase-config-tests", isDirectory: true)
        .appendingPathComponent(UUID().uuidString, isDirectory: true)
    try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
    return root.appendingPathComponent("archebase-config.json")
}
