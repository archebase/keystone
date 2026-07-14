import SwiftUI

struct ContentView: View {
    @StateObject private var viewModel = QiongcheDemoViewModel()

    var body: some View {
        NavigationStack {
            ScrollView {
                VStack(spacing: 16) {
                    StatusStrip(viewModel: viewModel)
                    ConfigPanel(viewModel: viewModel)
                    LocalStatePanel(state: viewModel.localState)
                    ReadyPanel(viewModel: viewModel)
                    QiongcheUploadPanel(viewModel: viewModel)
                    PathsPanel(paths: viewModel.localState.paths)
                }
                .padding()
            }
            .background(Color(.systemGroupedBackground))
            .navigationTitle("Qiongche SDK")
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

private struct StatusStrip: View {
    @ObservedObject var viewModel: QiongcheDemoViewModel

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            HStack {
                Text(viewModel.statusMessage)
                    .font(.headline)
                    .frame(maxWidth: .infinity, alignment: .leading)
                if viewModel.isBusy {
                    ProgressView()
                }
            }

            HStack(spacing: 8) {
                StateBadge(title: viewModel.localState.endpointsExists ? "Endpoint" : "No Endpoint", systemImage: "globe", tint: viewModel.localState.endpointsExists ? .green : .orange)
                StateBadge(title: viewModel.localState.configExists ? "Config" : "No Config", systemImage: "key", tint: viewModel.localState.configExists ? .green : .orange)
                StateBadge(title: viewModel.localState.stateExists ? "State" : "No State", systemImage: "doc.text", tint: viewModel.localState.stateExists ? .blue : .secondary)
            }
        }
        .panelBox()
    }
}

private struct ConfigPanel: View {
    @ObservedObject var viewModel: QiongcheDemoViewModel

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            PanelHeader(title: "配置", systemImage: "curlybraces")
            TextEditor(text: $viewModel.configString)
                .font(.system(.footnote, design: .monospaced))
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled()
                .frame(minHeight: 190)
                .padding(8)
                .background(Color(.secondarySystemGroupedBackground))
                .clipShape(RoundedRectangle(cornerRadius: 8, style: .continuous))

            HStack {
                Button {
                    viewModel.fillSampleConfig()
                } label: {
                    Label("填入示例配置", systemImage: "doc.badge.plus")
                }
                .buttonStyle(.bordered)

                Button {
                    Task { await viewModel.saveConfigAndInit() }
                } label: {
                    Label("保存并初始化", systemImage: "square.and.arrow.down")
                }
                .buttonStyle(.borderedProminent)
            }
            .disabled(viewModel.isBusy)
        }
        .panelBox()
    }
}

private struct LocalStatePanel: View {
    let state: QiongcheDemoLocalState

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            PanelHeader(title: "本地状态", systemImage: "externaldrive")
            KeyValueLine(title: "endpoint", value: state.endpointsExists ? "存在" : "缺失")
            KeyValueLine(title: "config", value: state.configExists ? "存在" : "缺失")
            KeyValueLine(title: "state", value: state.stateExists ? "存在" : "缺失")
            if let deviceID = state.stateDeviceID {
                KeyValueLine(title: "device_id", value: deviceID)
            }
            if let initializedAt = state.initializedAt {
                KeyValueLine(title: "initialized", value: initializedAt.formatted(date: .abbreviated, time: .standard))
            }
            if let stateReadError = state.stateReadError {
                Text(stateReadError)
                    .font(.footnote)
                    .foregroundStyle(.orange)
            }
        }
        .panelBox()
    }
}

private struct ReadyPanel: View {
    @ObservedObject var viewModel: QiongcheDemoViewModel

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            PanelHeader(title: "Ready", systemImage: "checkmark.seal")
            HStack {
                StateBadge(title: readyTitle, systemImage: readyIcon, tint: readyTint)
                Spacer(minLength: 0)
                Button {
                    Task { await viewModel.checkReady() }
                } label: {
                    Label("检查", systemImage: "dot.radiowaves.left.and.right")
                }
                .buttonStyle(.borderedProminent)
                .disabled(viewModel.isBusy)
            }
            if let lastReadyCheck = viewModel.lastReadyCheck {
                Text(lastReadyCheck.formatted(date: .abbreviated, time: .standard))
                    .font(.footnote)
                    .foregroundStyle(.secondary)
            }
        }
        .panelBox()
    }

    private var readyTitle: String {
        switch viewModel.ready {
        case .some(true): return "Ready"
        case .some(false): return "Not Ready"
        case .none: return "Unchecked"
        }
    }

    private var readyIcon: String {
        switch viewModel.ready {
        case .some(true): return "checkmark.circle.fill"
        case .some(false): return "xmark.circle.fill"
        case .none: return "minus.circle"
        }
    }

    private var readyTint: Color {
        switch viewModel.ready {
        case .some(true): return .green
        case .some(false): return .orange
        case .none: return .secondary
        }
    }
}

struct QiongcheUploadPanel: View {
    @ObservedObject var viewModel: QiongcheDemoViewModel

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            PanelHeader(title: "上传验证", systemImage: "paperplane")
            HStack {
                Button {
                    Task { await viewModel.makeSampleFile() }
                } label: {
                    Label("生成文件", systemImage: "doc.badge.plus")
                }
                .buttonStyle(.bordered)

                Button {
                    Task { await viewModel.uploadSampleFile() }
                } label: {
                    Label("上传文件", systemImage: "square.and.arrow.up")
                }
                .buttonStyle(.borderedProminent)
                .disabled(viewModel.isBusy || viewModel.sampleFileURL == nil)
            }

            Text(viewModel.sampleFileURL?.lastPathComponent ?? "未生成文件")
                .font(.footnote)
                .foregroundStyle(.secondary)
                .lineLimit(2)
                .frame(maxWidth: .infinity, alignment: .leading)

            if let resultSummary = viewModel.resultSummary {
                Text(resultSummary)
                    .font(.footnote.monospaced())
                    .frame(maxWidth: .infinity, alignment: .leading)
            }

            if !viewModel.uploadEvents.isEmpty {
                VStack(alignment: .leading, spacing: 6) {
                    ForEach(Array(viewModel.uploadEvents.suffix(8).enumerated()), id: \.offset) { _, event in
                        Text(event)
                            .font(.caption.monospaced())
                            .foregroundStyle(.secondary)
                    }
                }
                .frame(maxWidth: .infinity, alignment: .leading)
            }
        }
        .panelBox()
    }
}

private struct PathsPanel: View {
    let paths: QiongcheDemoPaths

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            PanelHeader(title: "路径", systemImage: "folder")
            KeyValueLine(title: "root", value: paths.rootURL.path)
            KeyValueLine(title: "uploads", value: paths.persistRootURL.path)
        }
        .panelBox()
    }
}

private struct PanelHeader: View {
    var title: String
    var systemImage: String

    var body: some View {
        Label(title, systemImage: systemImage)
            .font(.headline)
            .frame(maxWidth: .infinity, alignment: .leading)
    }
}

private struct StateBadge: View {
    var title: String
    var systemImage: String
    var tint: Color

    var body: some View {
        Label(title, systemImage: systemImage)
            .font(.caption.weight(.semibold))
            .lineLimit(1)
            .padding(.horizontal, 10)
            .padding(.vertical, 7)
            .background(tint.opacity(0.14))
            .foregroundStyle(tint)
            .clipShape(RoundedRectangle(cornerRadius: 8, style: .continuous))
    }
}

private struct KeyValueLine: View {
    var title: String
    var value: String

    var body: some View {
        VStack(alignment: .leading, spacing: 3) {
            Text(title)
                .font(.caption.weight(.semibold))
                .foregroundStyle(.secondary)
            Text(value)
                .font(.footnote.monospaced())
                .textSelection(.enabled)
                .lineLimit(3)
                .frame(maxWidth: .infinity, alignment: .leading)
        }
    }
}

private extension View {
    func panelBox() -> some View {
        self
            .padding(14)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(Color(.secondarySystemGroupedBackground))
            .clipShape(RoundedRectangle(cornerRadius: 8, style: .continuous))
    }
}
