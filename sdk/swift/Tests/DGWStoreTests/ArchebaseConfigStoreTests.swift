// SPDX-FileCopyrightText: 2026 ArcheBase
// SPDX-License-Identifier: MulanPSL-2.0

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
    let persisted = try String(contentsOf: configURL, encoding: .utf8)
    #expect(persisted.contains("\"credential_store\""))
    #expect(!persisted.contains("credential-v1"))
    #expect(!persisted.contains("api_key"))
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
    let persisted = try ArchebaseConfig.decodePersisted(from: data)
    #expect(persisted.tags == config.tags)
    #expect(persisted.apiKey.isEmpty)
}

@Test func persistedCredentialAccountSurvivesConfigPathChange() async throws {
    let credentialStore = InMemoryDeviceCredentialStore()
    let oldConfigURL = try temporaryConfigURL()
    let oldStore = ArchebaseConfigStore(configURL: oldConfigURL, credentialStore: credentialStore)
    let config = try ArchebaseConfig(apiKey: "credential-v1", tags: ["device": "robot"])
    try await oldStore.initialize(config)

    let newConfigURL = try temporaryConfigURL()
    try FileManager.default.copyItem(at: oldConfigURL, to: newConfigURL)
    let newStore = ArchebaseConfigStore(configURL: newConfigURL, credentialStore: credentialStore)

    #expect(try await newStore.load() == config)
}

@Test func loadMigratesLegacyPathCredentialAccount() async throws {
    let credentialStore = InMemoryDeviceCredentialStore()
    let configURL = try temporaryConfigURL()
    let legacyConfig = try ArchebaseConfig(apiKey: "credential-v1", tags: ["device": "robot"])
    try legacyConfig.prettyJSONData().write(to: configURL)
    try credentialStore.save("credential-v1", account: configURL.standardizedFileURL.path)
    let store = ArchebaseConfigStore(configURL: configURL, credentialStore: credentialStore)

    #expect(try await store.load() == legacyConfig)
    let migrated = try ArchebaseConfig.decodePersisted(from: Data(contentsOf: configURL))
    let migratedAccount = try #require(migrated.credentialAccount)
    #expect(migratedAccount != configURL.standardizedFileURL.path)
    #expect(try credentialStore.load(account: migratedAccount) == "credential-v1")
}

@Test func migratedCredentialAccountSurvivesConfigPathChange() async throws {
    let credentialStore = InMemoryDeviceCredentialStore()
    let oldConfigURL = try temporaryConfigURL()
    let config = try ArchebaseConfig(apiKey: "credential-v1", tags: ["device": "robot"])
    try config.prettyJSONData().write(to: oldConfigURL)
    try credentialStore.save("credential-v1", account: oldConfigURL.standardizedFileURL.path)
    let oldStore = ArchebaseConfigStore(configURL: oldConfigURL, credentialStore: credentialStore)
    #expect(try await oldStore.load() == config)

    let newConfigURL = try temporaryConfigURL()
    try FileManager.default.copyItem(at: oldConfigURL, to: newConfigURL)
    let newStore = ArchebaseConfigStore(configURL: newConfigURL, credentialStore: credentialStore)

    #expect(try await newStore.load() == config)
}

@Test func legacyCredentialCanMigrateAfterConfigPathAlreadyChanged() async throws {
    let credentialStore = InMemoryDeviceCredentialStore()
    let oldConfigURL = try temporaryConfigURL()
    let newConfigURL = try temporaryConfigURL()
    let config = try ArchebaseConfig(apiKey: "credential-v1", tags: ["device": "robot"])
    try config.prettyJSONData().write(to: newConfigURL)
    try credentialStore.save("credential-v1", account: oldConfigURL.standardizedFileURL.path)
    let store = ArchebaseConfigStore(configURL: newConfigURL, credentialStore: credentialStore)

    #expect(try await store.load() == config)
    let migrated = try ArchebaseConfig.decodePersisted(from: Data(contentsOf: newConfigURL))
    let migratedAccount = try #require(migrated.credentialAccount)
    #expect(migratedAccount != oldConfigURL.standardizedFileURL.path)
    #expect(migratedAccount != newConfigURL.standardizedFileURL.path)
    #expect(try credentialStore.load(account: migratedAccount) == "credential-v1")
}

@Test func missingPersistedCredentialAccountCanRecoverFromMigratableCredential() async throws {
    let credentialStore = InMemoryDeviceCredentialStore()
    let configURL = try temporaryConfigURL()
    var persistedConfig = try ArchebaseConfig(apiKey: "credential-v1", tags: ["device": "robot"])
    persistedConfig.credentialAccount = "missing-account"
    try persistedConfig.prettyJSONData().write(to: configURL)
    try credentialStore.save("credential-v1", account: "old-container-account")
    let store = ArchebaseConfigStore(configURL: configURL, credentialStore: credentialStore)

    #expect(try await store.load() == (try ArchebaseConfig(apiKey: "credential-v1", tags: ["device": "robot"])))
    let migrated = try ArchebaseConfig.decodePersisted(from: Data(contentsOf: configURL))
    let migratedAccount = try #require(migrated.credentialAccount)
    #expect(migratedAccount != "missing-account")
    #expect(try credentialStore.load(account: migratedAccount) == "credential-v1")
}

private func temporaryConfigURL() throws -> URL {
    let root = FileManager.default.temporaryDirectory
        .appendingPathComponent("archebase-config-tests", isDirectory: true)
        .appendingPathComponent(UUID().uuidString, isDirectory: true)
    try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
    return root.appendingPathComponent("archebase-config.json")
}

private final class InMemoryDeviceCredentialStore: DeviceCredentialStoring, @unchecked Sendable {
    private var credentials: [String: String] = [:]
    private let lock = NSLock()

    func load(account: String) throws -> String {
        self.lock.lock()
        defer { self.lock.unlock() }
        guard let credential = self.credentials[account] else {
            throw DeviceCredentialStoreError.notFound
        }
        return credential
    }

    func save(_ credential: String, account: String) throws {
        self.lock.lock()
        defer { self.lock.unlock() }
        self.credentials[account] = credential
    }

    func delete(account: String) throws {
        self.lock.lock()
        defer { self.lock.unlock() }
        self.credentials.removeValue(forKey: account)
    }

    func loadMigratableCredential(excludingAccounts: Set<String>) throws -> StoredDeviceCredential {
        self.lock.lock()
        defer { self.lock.unlock() }
        guard let candidate = self.credentials
            .filter({ !excludingAccounts.contains($0.key) })
            .sorted(by: { $0.key > $1.key })
            .first else {
            throw DeviceCredentialStoreError.notFound
        }
        return StoredDeviceCredential(account: candidate.key, credential: candidate.value)
    }
}
