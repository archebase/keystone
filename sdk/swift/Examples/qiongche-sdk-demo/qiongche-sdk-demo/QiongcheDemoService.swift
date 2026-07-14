import DataGatewayClient
import Foundation

actor QiongcheDemoService {
    private let paths: QiongcheDemoPaths
    private var client: DataGatewayClient?

    init(paths: QiongcheDemoPaths = .appDefault) {
        self.paths = paths
    }

    func localState() -> QiongcheDemoLocalState {
        QiongcheDemoStateReader.read(paths: self.paths)
    }

    func saveConfigAndInit(_ configString: String) async throws {
        let sdk = try QiongcheDataGatewaySDK(rootURL: self.paths.rootURL)
        try await sdk.saveConfigAndInit(configString: configString)
        self.client = nil
    }

    func checkReady() async -> Bool {
        do {
            let sdk = try QiongcheDataGatewaySDK(rootURL: self.paths.rootURL)
            return await sdk.isReadyToUpload()
        } catch {
            return false
        }
    }

    func makeSampleFile() throws -> URL {
        try FileManager.default.createDirectory(
            at: self.paths.demoFilesURL,
            withIntermediateDirectories: true
        )

        let timestamp = ISO8601DateFormatter().string(from: Date())
        let payload: [String: Any] = [
            "source": "qiongche-sdk-demo",
            "created_at": timestamp,
            "sequence": Int(Date().timeIntervalSince1970),
            "values": [
                "temperature": 21.6,
                "pressure": 101.3,
                "status": "demo",
            ],
        ]
        let data = try JSONSerialization.data(
            withJSONObject: payload,
            options: [.prettyPrinted, .sortedKeys]
        )
        let fileName = "qiongche-sdk-demo-\(Int(Date().timeIntervalSince1970)).json"
        let fileURL = self.paths.demoFilesURL.appendingPathComponent(fileName)
        try data.write(to: fileURL, options: [.atomic])
        return fileURL
    }

    func uploadSampleFile(_ fileURL: URL) async throws -> AsyncThrowingStream<UploadEvent, Error> {
        let client = try await self.loadClient()
        let request = UploadRequest(
            fileURL: fileURL,
            clientHints: ["source": "qiongche-sdk-demo"],
            rawTags: ["scene": "qiongche-demo"],
            displayName: fileURL.lastPathComponent
        )
        return await client.uploadEvents(request)
    }

    private func loadClient() async throws -> DataGatewayClient {
        if let client {
            return client
        }
        let client = try await DataGatewayClient.fromArchebaseConfig(
            configURL: self.paths.configURL,
            persistRootURL: self.paths.persistRootURL,
            endpointsURL: self.paths.endpointsURL,
            observability: .disabled
        )
        self.client = client
        return client
    }
}
