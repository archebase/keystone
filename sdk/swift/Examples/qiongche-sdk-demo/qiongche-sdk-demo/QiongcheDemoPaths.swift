import DataGatewayClient
import Foundation

struct QiongcheDemoPaths: Sendable {
    let rootURL: URL
    let endpointsURL: URL
    let configURL: URL
    let stateURL: URL
    let persistRootURL: URL
    let demoFilesURL: URL

    nonisolated static var appDefault: QiongcheDemoPaths {
        let supportRoot = FileManager.default.urls(
            for: .applicationSupportDirectory,
            in: .userDomainMask
        )[0]
        let root = supportRoot
            .appendingPathComponent("Archebase", isDirectory: true)
            .standardizedFileURL

        return QiongcheDemoPaths(
            rootURL: root,
            endpointsURL: root.appendingPathComponent(ArchebasePublicEndpoints.endpointsFileName).standardizedFileURL,
            configURL: root.appendingPathComponent("archebase-config.json").standardizedFileURL,
            stateURL: root.appendingPathComponent("qiongche-sdk-state.json").standardizedFileURL,
            persistRootURL: root.appendingPathComponent("Uploads", isDirectory: true).standardizedFileURL,
            demoFilesURL: root.appendingPathComponent("Demo Files", isDirectory: true).standardizedFileURL
        )
    }
}

struct QiongcheDemoLocalState: Sendable {
    let paths: QiongcheDemoPaths
    let endpointsExists: Bool
    let configExists: Bool
    let stateExists: Bool
    let stateDeviceID: String?
    let initializedAt: Date?
    let stateReadError: String?
}

struct QiongcheDemoStateReader {
    private struct PersistedState: Decodable {
        var deviceID: String
        var initializedAtUnix: Int64

        enum CodingKeys: String, CodingKey {
            case deviceID = "device_id"
            case initializedAtUnix = "initialized_at_unix"
        }
    }

    static func read(paths: QiongcheDemoPaths, fileManager: FileManager = .default) -> QiongcheDemoLocalState {
        let endpointsExists = fileManager.fileExists(atPath: paths.endpointsURL.path)
        let configExists = fileManager.fileExists(atPath: paths.configURL.path)
        let stateExists = fileManager.fileExists(atPath: paths.stateURL.path)

        var stateDeviceID: String?
        var initializedAt: Date?
        var stateReadError: String?

        if stateExists {
            do {
                let state = try JSONDecoder().decode(PersistedState.self, from: Data(contentsOf: paths.stateURL))
                stateDeviceID = state.deviceID
                initializedAt = Date(timeIntervalSince1970: TimeInterval(state.initializedAtUnix))
            } catch {
                stateReadError = "state 文件无法解析"
            }
        }

        return QiongcheDemoLocalState(
            paths: paths,
            endpointsExists: endpointsExists,
            configExists: configExists,
            stateExists: stateExists,
            stateDeviceID: stateDeviceID,
            initializedAt: initializedAt,
            stateReadError: stateReadError
        )
    }
}
