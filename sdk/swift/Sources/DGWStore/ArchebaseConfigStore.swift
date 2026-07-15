// SPDX-FileCopyrightText: 2026 ArcheBase
// SPDX-License-Identifier: MulanPSL-2.0

import DGWControlPlane
import Foundation

/// Actor responsible for loading and atomically replacing `archebase-config.json`.
public actor ArchebaseConfigStore {
    private let configURL: URL
    private let fileManager: FileManager
    private let credentialStore: any DeviceCredentialStoring

    /// Creates a store bound to one configuration file URL.
    public init(configURL: URL, fileManager: FileManager = .default) {
        self.configURL = configURL.standardizedFileURL
        self.fileManager = fileManager
        self.credentialStore = KeychainDeviceCredentialStore()
    }

    package init(
        configURL: URL,
        fileManager: FileManager = .default,
        credentialStore: any DeviceCredentialStoring
    ) {
        self.configURL = configURL.standardizedFileURL
        self.fileManager = fileManager
        self.credentialStore = credentialStore
    }

    /// Returns whether the configuration file currently exists.
    public func exists() -> Bool {
        self.fileManager.fileExists(atPath: self.configURL.path)
    }

    /// Returns the standardized configuration file URL used by this store.
    public func resolvedConfigURL() -> URL {
        self.configURL
    }

    /// Loads and validates the current device configuration.
    public func load() throws -> ArchebaseConfig {
        guard self.exists() else {
            throw DataGatewayClientError.notInitialized(configURL: self.configURL)
        }
        do {
            let persisted = try ArchebaseConfig.decodePersisted(from: Data(contentsOf: self.configURL))
            if let account = persisted.credentialAccount {
                let apiKey: String
                do {
                    apiKey = try self.credentialStore.load(account: account)
                } catch DeviceCredentialStoreError.notFound {
                    apiKey = try self.credentialStore
                        .loadMigratableCredential(excludingAccounts: [account, self.legacyCredentialAccount])
                        .credential
                    try self.migrateLegacyCredentialAccount(apiKey: apiKey, persisted: persisted)
                }
                return try ArchebaseConfig(apiKey: apiKey, tags: persisted.tags)
            }
            let apiKey = try self.loadLegacyCredentialForMigration()
            try self.migrateLegacyCredentialAccount(apiKey: apiKey, persisted: persisted)
            return try ArchebaseConfig(apiKey: apiKey, tags: persisted.tags)
        } catch let error as DataGatewayClientError {
            throw error
        } catch {
            throw DataGatewayClientError.invalidConfiguration("failed to load archebase config: \(error.localizedDescription)")
        }
    }

    /// Writes the initial device configuration, rejecting an existing file.
    public func initialize(_ config: ArchebaseConfig) throws {
        guard !self.exists() else {
            throw DataGatewayClientError.alreadyInitialized(configURL: self.configURL)
        }
        try self.write(config, replacingExisting: false)
    }

    /// Replaces the existing device configuration after a successful reinit.
    public func replaceForReinit(_ config: ArchebaseConfig) throws {
        guard self.exists() else {
            throw DataGatewayClientError.notInitialized(configURL: self.configURL)
        }
        _ = try self.load()
        try self.write(config, replacingExisting: true)
    }

    /// Writes or replaces the device configuration without exposing reinit semantics.
    package func replaceOrInitialize(_ config: ArchebaseConfig) throws {
        try self.write(config, replacingExisting: true)
    }

    private func write(_ config: ArchebaseConfig, replacingExisting: Bool) throws {
        let account = try self.credentialAccountForWrite()
        var persistedConfig = config
        persistedConfig.credentialAccount = account
        let data = try persistedConfig.prettyJSONData()
        let previousCredential = try? self.credentialStore.load(account: account)
        do {
            try self.credentialStore.save(config.apiKey, account: account)
            try AtomicFileWriter.write(data, to: self.configURL, fileManager: self.fileManager) { temporaryURL, destination, fileManager in
                if replacingExisting {
                    try AtomicFileWriter.replaceOrMoveTemporaryItem(temporaryURL, to: destination, fileManager: fileManager)
                } else {
                    do {
                        try AtomicFileWriter.moveTemporaryItem(temporaryURL, to: destination, fileManager: fileManager)
                    } catch {
                        if fileManager.fileExists(atPath: destination.path) {
                            throw DataGatewayClientError.alreadyInitialized(configURL: destination)
                        }
                        throw error
                    }
                }
            }
            let loaded = try self.load()
            guard loaded == config else {
                throw DataGatewayClientError.persistenceFailed("archebase config verification failed after write")
            }
        } catch let error as DataGatewayClientError {
            self.restoreCredential(previousCredential, account: account)
            throw error
        } catch {
            self.restoreCredential(previousCredential, account: account)
            throw DataGatewayClientError.persistenceFailed("failed to write archebase config: \(error.localizedDescription)")
        }
    }

    private func credentialAccountForWrite() throws -> String {
        guard self.exists() else {
            return Self.makeCredentialAccount()
        }
        let persisted = try? ArchebaseConfig.decodePersisted(from: Data(contentsOf: self.configURL))
        if let account = persisted?.credentialAccount {
            return account
        }
        return Self.makeCredentialAccount()
    }

    private static func makeCredentialAccount() -> String {
        "archebase-device-api-key-\(UUID().uuidString.lowercased())"
    }

    private func loadLegacyCredentialForMigration() throws -> String {
        do {
            return try self.credentialStore.load(account: self.legacyCredentialAccount)
        } catch DeviceCredentialStoreError.notFound {
            return try self.credentialStore
                .loadMigratableCredential(excludingAccounts: [self.legacyCredentialAccount])
                .credential
        }
    }

    private func migrateLegacyCredentialAccount(apiKey: String, persisted: ArchebaseConfig) throws {
        let account = Self.makeCredentialAccount()
        var migratedConfig = persisted
        migratedConfig.apiKey = apiKey
        migratedConfig.credentialAccount = account
        let data = try migratedConfig.prettyJSONData()
        try self.credentialStore.save(apiKey, account: account)
        do {
            try AtomicFileWriter.write(data, to: self.configURL, fileManager: self.fileManager) { temporaryURL, destination, fileManager in
                try AtomicFileWriter.replaceOrMoveTemporaryItem(temporaryURL, to: destination, fileManager: fileManager)
            }
        } catch {
            try? self.credentialStore.delete(account: account)
            throw error
        }
    }

    private var legacyCredentialAccount: String {
        self.configURL.path
    }

    private func restoreCredential(_ credential: String?, account: String) {
        if let credential {
            try? self.credentialStore.save(credential, account: account)
        } else {
            try? self.credentialStore.delete(account: account)
        }
    }
}
