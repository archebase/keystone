import Foundation

/// Shared helper for protected temporary writes followed by an atomic move or replace.
package enum AtomicFileWriter {
    package static func write(
        _ data: Data,
        to destination: URL,
        fileManager: FileManager = .default,
        operation: (_ temporaryURL: URL, _ destination: URL, _ fileManager: FileManager) throws -> Void
    ) throws {
        let resolvedDestination = destination.standardizedFileURL
        let parent = resolvedDestination.deletingLastPathComponent()
        let temporaryURL = parent.appendingPathComponent(".\(resolvedDestination.lastPathComponent).\(UUID().uuidString).tmp")

        do {
            try fileManager.createDirectory(at: parent, withIntermediateDirectories: true)
            try Self.writeProtected(data, to: temporaryURL)
            try operation(temporaryURL, resolvedDestination, fileManager)
            try? fileManager.removeItem(at: temporaryURL)
        } catch {
            try? fileManager.removeItem(at: temporaryURL)
            throw error
        }
    }

    package static func moveTemporaryItem(
        _ temporaryURL: URL,
        to destination: URL,
        fileManager: FileManager
    ) throws {
        try fileManager.moveItem(at: temporaryURL, to: destination)
    }

    package static func replaceOrMoveTemporaryItem(
        _ temporaryURL: URL,
        to destination: URL,
        fileManager: FileManager
    ) throws {
        if fileManager.fileExists(atPath: destination.path) {
            _ = try fileManager.replaceItemAt(destination, withItemAt: temporaryURL)
            return
        }

        do {
            try fileManager.moveItem(at: temporaryURL, to: destination)
        } catch {
            if fileManager.fileExists(atPath: destination.path) {
                _ = try fileManager.replaceItemAt(destination, withItemAt: temporaryURL)
                return
            }
            throw error
        }
    }

    private static func writeProtected(_ data: Data, to url: URL) throws {
        #if os(iOS)
        try data.write(to: url, options: [.completeFileProtectionUnlessOpen])
        #else
        try data.write(to: url, options: [])
        #endif
    }
}
