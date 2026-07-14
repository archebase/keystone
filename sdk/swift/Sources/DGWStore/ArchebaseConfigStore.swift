import DGWControlPlane
import Foundation

/// Actor responsible for loading and atomically replacing `archebase-config.json`.
public actor ArchebaseConfigStore {
    private let configURL: URL
    private let fileManager: FileManager

    /// Creates a store bound to one configuration file URL.
    public init(configURL: URL, fileManager: FileManager = .default) {
        self.configURL = configURL.standardizedFileURL
        self.fileManager = fileManager
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
            return try ArchebaseConfig.decodeValidated(from: Data(contentsOf: self.configURL))
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
        let data = try config.prettyJSONData()
        do {
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
            throw error
        } catch {
            throw DataGatewayClientError.persistenceFailed("failed to write archebase config: \(error.localizedDescription)")
        }
    }
}
