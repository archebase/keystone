import DataGatewayClient
import DGWStore
import SwiftUI
import UniformTypeIdentifiers

struct ContentView: View {
    @StateObject private var viewModel = UploadDemoViewModel()
    @State private var isFileImporterPresented = false

    var body: some View {
        NavigationStack {
            ScrollView {
                VStack(spacing: 18) {
                    StatusPanel(viewModel: viewModel)
                    EndpointsSection(viewModel: viewModel)
                    DeviceSection(viewModel: viewModel)
                    UploadSection(
                        viewModel: viewModel,
                        presentFileImporter: { isFileImporterPresented = true }
                    )
                    PendingUploadsSection(viewModel: viewModel)
                    ResultSection(result: viewModel.lastResult)
                    PathsSection(paths: viewModel.paths)
                }
                .padding()
            }
            .background(Color(.systemGroupedBackground))
            .navigationTitle("DP Simulator")
            .toolbar {
                ToolbarItem(placement: .topBarTrailing) {
                    Button {
                        Task { await viewModel.refreshLocalState() }
                    } label: {
                        Label("刷新", systemImage: "arrow.clockwise")
                    }
                    .disabled(viewModel.isBusy)
                }
            }
            .task {
                await viewModel.bootstrap()
                #if DEBUG
                await viewModel.runLaunchSelfTestIfRequested()
                #endif
            }
            .fileImporter(
                isPresented: $isFileImporterPresented,
                allowedContentTypes: [.data, .content, .text, .json],
                allowsMultipleSelection: false
            ) { result in
                switch result {
                case .success(let urls):
                    viewModel.selectFile(urls.first)
                case .failure(let error):
                    viewModel.errorMessage = UploadDemoViewModel.describe(error)
                }
            }
            .alert(
                "操作失败",
                isPresented: Binding(
                    get: { viewModel.errorMessage != nil },
                    set: { isPresented in
                        if !isPresented {
                            viewModel.errorMessage = nil
                        }
                    }
                )
            ) {
                Button("好", role: .cancel) {
                    viewModel.errorMessage = nil
                }
            } message: {
                Text(viewModel.errorMessage ?? "")
            }
        }
    }
}

private struct StatusPanel: View {
    @ObservedObject var viewModel: UploadDemoViewModel

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            HStack {
                Text(viewModel.statusMessage)
                    .font(.headline)
                    .frame(maxWidth: .infinity, alignment: .leading)
                if viewModel.isBusy {
                    ProgressView()
                }
            }

            HStack(spacing: 10) {
                StatusBadge(
                    title: viewModel.endpointsAvailable ? "Endpoints" : "No Endpoints",
                    systemImage: viewModel.endpointsAvailable ? "checkmark.seal.fill" : "exclamationmark.triangle.fill",
                    tint: viewModel.endpointsAvailable ? .green : .orange
                )
                StatusBadge(
                    title: viewModel.deviceReady ? "Device Ready" : "Device Pending",
                    systemImage: viewModel.deviceReady ? "iphone.gen3.circle.fill" : "iphone.gen3.slash",
                    tint: viewModel.deviceReady ? .blue : .secondary
                )
                StatusBadge(
                    title: "\(viewModel.pendingUploads.count) Pending",
                    systemImage: "tray.full.fill",
                    tint: viewModel.pendingUploads.isEmpty ? .secondary : .purple
                )
            }
        }
        .sectionBox()
    }
}

private struct EndpointsSection: View {
    @ObservedObject var viewModel: UploadDemoViewModel

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            SectionHeader(title: "Endpoints", systemImage: "globe")

            TextEditor(text: $viewModel.endpointsJSON)
                .font(.system(.footnote, design: .monospaced))
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled()
                .frame(minHeight: 180)
                .padding(8)
                .background(Color(.secondarySystemGroupedBackground))
                .clipShape(RoundedRectangle(cornerRadius: 8, style: .continuous))

            HStack {
                Button {
                    Task { await viewModel.saveEndpoints() }
                } label: {
                    Label("保存 Endpoints", systemImage: "square.and.arrow.down")
                }
                .buttonStyle(.borderedProminent)

                Button {
                    viewModel.fillSampleEndpoints()
                } label: {
                    Label("填入样例", systemImage: "curlybraces")
                }
                .buttonStyle(.bordered)

                Spacer(minLength: 0)
            }
            .disabled(viewModel.isBusy)
        }
        .sectionBox()
    }
}

private struct DeviceSection: View {
    @ObservedObject var viewModel: UploadDemoViewModel

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            SectionHeader(title: "Device", systemImage: "iphone")

            TextField("Device ID", text: $viewModel.deviceID)
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled()
                .textFieldStyle(.roundedBorder)

            HStack {
                Button {
                    Task { await viewModel.initializeDevice() }
                } label: {
                    Label("Init Device", systemImage: "checkmark.circle")
                }
                .buttonStyle(.borderedProminent)

                Button(role: .destructive) {
                    Task { await viewModel.reinitializeDevice() }
                } label: {
                    Label("Reinit Device", systemImage: "arrow.triangle.2.circlepath")
                }
                .buttonStyle(.bordered)

                Spacer(minLength: 0)
            }
            .disabled(viewModel.isBusy)

            if !viewModel.deviceTags.isEmpty {
                KeyValueBlock(title: "Device Tags", values: viewModel.deviceTags)
            }
        }
        .sectionBox()
    }
}

private struct UploadSection: View {
    @ObservedObject var viewModel: UploadDemoViewModel
    let presentFileImporter: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            SectionHeader(title: "Upload", systemImage: "square.and.arrow.up")

            HStack {
                Button {
                    presentFileImporter()
                } label: {
                    Label("选择文件", systemImage: "folder")
                }
                .buttonStyle(.bordered)

                Button {
                    Task { await viewModel.createSampleFile() }
                } label: {
                    Label("生成样例文件", systemImage: "doc.badge.plus")
                }
                .buttonStyle(.bordered)

                Spacer(minLength: 0)
            }
            .disabled(viewModel.isBusy)

            Text(viewModel.selectedFileURL?.lastPathComponent ?? "尚未选择文件")
                .font(.subheadline)
                .foregroundStyle(.secondary)
                .lineLimit(2)
                .frame(maxWidth: .infinity, alignment: .leading)

            MetadataEditors(viewModel: viewModel)

            Button {
                Task { await viewModel.uploadSelectedFile() }
            } label: {
                Label("上传选中文件", systemImage: "paperplane.fill")
                    .frame(maxWidth: .infinity)
            }
            .buttonStyle(.borderedProminent)
            .disabled(viewModel.isBusy || !viewModel.deviceReady)

            VStack(alignment: .leading, spacing: 8) {
                Text(viewModel.uploadStatus)
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
                if let uploadProgress = viewModel.uploadProgress {
                    ProgressView(value: uploadProgress)
                }
            }
        }
        .sectionBox()
    }
}

private struct MetadataEditors: View {
    @ObservedObject var viewModel: UploadDemoViewModel

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            Text("Client Hints")
                .font(.caption.weight(.semibold))
                .foregroundStyle(.secondary)
            TextEditor(text: $viewModel.clientHintsJSON)
                .metadataEditor(height: 88)

            Text("Raw Tags")
                .font(.caption.weight(.semibold))
                .foregroundStyle(.secondary)
            TextEditor(text: $viewModel.rawTagsJSON)
                .metadataEditor(height: 88)
        }
    }
}

private struct PendingUploadsSection: View {
    @ObservedObject var viewModel: UploadDemoViewModel

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            SectionHeader(title: "Pending Uploads", systemImage: "tray.full")

            HStack {
                Button {
                    Task { await viewModel.refreshPendingUploads() }
                } label: {
                    Label("List", systemImage: "list.bullet")
                }
                .buttonStyle(.bordered)

                Button {
                    Task { await viewModel.resumeAllPending() }
                } label: {
                    Label("Resume All", systemImage: "play.fill")
                }
                .buttonStyle(.borderedProminent)

                Button(role: .destructive) {
                    Task { await viewModel.abortAllPending() }
                } label: {
                    Label("Abort All", systemImage: "xmark.octagon")
                }
                .buttonStyle(.bordered)

                Spacer(minLength: 0)
            }
            .disabled(viewModel.isBusy || !viewModel.deviceReady)

            if viewModel.pendingUploads.isEmpty {
                Text("当前没有 SDK 本地 active 快照。")
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
                    .frame(maxWidth: .infinity, alignment: .leading)
            } else {
                VStack(spacing: 10) {
                    ForEach(viewModel.pendingUploads, id: \.logicalUploadID) { upload in
                        PendingUploadRow(
                            upload: upload,
                            isBusy: viewModel.isBusy,
                            resume: { Task { await viewModel.resume(upload) } },
                            abort: { Task { await viewModel.abort(upload) } }
                        )
                    }
                }
            }
        }
        .sectionBox()
    }
}

private struct PendingUploadRow: View {
    let upload: PendingUploadInfo
    let isBusy: Bool
    let resume: () -> Void
    let abort: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            Text(upload.logicalUploadID)
                .font(.system(.subheadline, design: .monospaced).weight(.semibold))
                .lineLimit(2)
                .textSelection(.enabled)

            Grid(alignment: .leading, horizontalSpacing: 12, verticalSpacing: 6) {
                GridRow {
                    Text("Upload ID")
                        .foregroundStyle(.secondary)
                    Text(upload.uploadID)
                        .lineLimit(1)
                        .truncationMode(.middle)
                }
                GridRow {
                    Text("Phase")
                        .foregroundStyle(.secondary)
                    Text(upload.phase.rawValue)
                }
                GridRow {
                    Text("Size")
                        .foregroundStyle(.secondary)
                    Text(UploadDemoViewModel.formatBytes(upload.fileSize))
                }
                GridRow {
                    Text("Updated")
                        .foregroundStyle(.secondary)
                    Text(upload.updatedAt.formatted(date: .abbreviated, time: .shortened))
                }
            }
            .font(.caption)

            HStack {
                Button(action: resume) {
                    Label("Resume", systemImage: "play.fill")
                }
                .buttonStyle(.borderedProminent)

                Button(role: .destructive, action: abort) {
                    Label("Abort", systemImage: "trash")
                }
                .buttonStyle(.bordered)

                Spacer(minLength: 0)
            }
            .disabled(isBusy)
        }
        .padding(12)
        .background(Color(.secondarySystemGroupedBackground))
        .clipShape(RoundedRectangle(cornerRadius: 8, style: .continuous))
    }
}

private struct ResultSection: View {
    let result: UploadResult?

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            SectionHeader(title: "Last Result", systemImage: "checkmark.seal")

            if let result {
                KeyValueBlock(values: [
                    "logicalUploadID": result.logicalUploadID,
                    "uploadID": result.uploadID,
                    "bucket": result.bucket,
                    "objectKey": result.objectKey,
                    "fileSize": UploadDemoViewModel.formatBytes(result.fileSize),
                    "ossObjectETag": result.ossObjectETag,
                ])
            } else {
                Text("尚未完成上传或恢复。")
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
            }
        }
        .sectionBox()
    }
}

private struct PathsSection: View {
    let paths: GatewayPaths

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            SectionHeader(title: "Local Paths", systemImage: "externaldrive")
            KeyValueBlock(values: [
                "endpoints": paths.endpointsURL.path,
                "config": paths.configURL.path,
                "uploads": paths.persistRootURL.path,
            ])
        }
        .sectionBox()
    }
}

private struct SectionHeader: View {
    let title: String
    let systemImage: String

    var body: some View {
        Label(title, systemImage: systemImage)
            .font(.title3.weight(.semibold))
            .frame(maxWidth: .infinity, alignment: .leading)
    }
}

private struct StatusBadge: View {
    let title: String
    let systemImage: String
    let tint: Color

    var body: some View {
        Label(title, systemImage: systemImage)
            .font(.caption.weight(.semibold))
            .foregroundStyle(tint)
            .padding(.horizontal, 10)
            .padding(.vertical, 7)
            .background(tint.opacity(0.12))
            .clipShape(Capsule())
    }
}

private struct KeyValueBlock: View {
    var title: String?
    let values: [String: String]

    init(title: String? = nil, values: [String: String]) {
        self.title = title
        self.values = values
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            if let title {
                Text(title)
                    .font(.caption.weight(.semibold))
                    .foregroundStyle(.secondary)
            }

            ForEach(values.keys.sorted(), id: \.self) { key in
                VStack(alignment: .leading, spacing: 2) {
                    Text(key)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                    Text(values[key] ?? "")
                        .font(.system(.footnote, design: .monospaced))
                        .textSelection(.enabled)
                        .frame(maxWidth: .infinity, alignment: .leading)
                }
            }
        }
        .padding(10)
        .background(Color(.secondarySystemGroupedBackground))
        .clipShape(RoundedRectangle(cornerRadius: 8, style: .continuous))
    }
}

private extension View {
    func sectionBox() -> some View {
        self
            .padding(16)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(Color(.systemBackground))
            .clipShape(RoundedRectangle(cornerRadius: 10, style: .continuous))
    }
}

private extension TextEditor {
    func metadataEditor(height: CGFloat) -> some View {
        self
            .font(.system(.footnote, design: .monospaced))
            .textInputAutocapitalization(.never)
            .autocorrectionDisabled()
            .frame(minHeight: height)
            .padding(8)
            .background(Color(.secondarySystemGroupedBackground))
            .clipShape(RoundedRectangle(cornerRadius: 8, style: .continuous))
    }
}

#Preview {
    ContentView()
}
