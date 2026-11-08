import Darwin
import AppKit
import SwiftUI

@main
struct MoraStatusApp: App {
    private let preview = CommandLine.arguments.contains("--preview")
    @StateObject private var model: StatusModel
    @NSApplicationDelegateAdaptor(PreviewWindowDelegate.self) private var appDelegate

    init() { _model = StateObject(wrappedValue: StatusModel(preview: CommandLine.arguments.contains("--preview"))) }

    var body: some Scene {
        MenuBarExtra {
            StatusPanel(model: model)
        } label: {
            Label("Mora", systemImage: model.error != nil ? "exclamationmark.circle.fill" : (model.snapshot?.aggregate.symbol ?? "circle.dashed"))
                .accessibilityLabel("Mora Status")
        }
        .menuBarExtraStyle(.window)
    }
}

final class PreviewWindowDelegate: NSObject, NSApplicationDelegate {
    private var window: NSWindow?
    func applicationDidFinishLaunching(_ notification: Notification) {
        guard CommandLine.arguments.contains("--preview") else { return }
        let view = StatusPanel(model: StatusModel(preview: true), isPreview: true)
        let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 420, height: 660), styleMask: [.titled, .closable], backing: .buffered, defer: false)
        window.title = "Mora Status Preview · fixture data"
        window.contentView = NSHostingView(rootView: view)
        window.center()
        window.makeKeyAndOrderFront(nil)
        self.window = window
    }
}

@MainActor
final class StatusModel: ObservableObject {
    @Published var snapshot: StatusSnapshot?
    @Published var isRefreshing = false
    @Published var lastUpdated: Date?
    @Published var error: String?
    @Published var detailText = "No collector output yet."

    private var timer: Timer?
    private let collector: URL
    private let preview: Bool

    init(preview: Bool = false) {
        self.preview = preview
        collector = Bundle.main.executableURL!.deletingLastPathComponent().appendingPathComponent("mora-tentacles")
        if preview {
            snapshot = try? StatusSnapshot.decode(Data(#"{"generated_at":"2026-11-08T08:00:00Z","devices":[{"id":"mac","name":"This Mac (sample)","connection":"connected","channels":[{"name":"Work shares","state":"healthy","detail":"Fixture data — no sync was checked.","last_success":"2026-11-08T07:45:00Z"},{"name":"Personal archive","state":"stale","detail":"Fixture data — displayed to demonstrate a stale channel.","last_success":"2026-11-07T07:45:00Z"}]}]}"#.utf8))
            lastUpdated = Date()
            detailText = "PREVIEW FIXTURE — no collector was run and no sync was checked."
            return
        }
        Task { await refresh() }
        timer = Timer.scheduledTimer(withTimeInterval: 60, repeats: true) { [weak self] _ in
            Task { @MainActor in await self?.refresh() }
        }
    }

    func refresh() async {
        guard !preview, !isRefreshing else { return }
        isRefreshing = true
        defer { isRefreshing = false }
        do {
            let collector = self.collector
            let result = try await Task.detached(priority: .utility) { try Self.runCollector(collector) }.value
            detailText = result.output
            guard result.status == 0 else { throw StatusError.collector(result.error.isEmpty ? "Collector exited with status \(result.status)." : result.error) }
            let decoded = try StatusSnapshot.decode(Data(result.output.utf8))
            snapshot = decoded
            lastUpdated = Date()
            error = nil
        } catch {
            self.error = error.localizedDescription
            if snapshot != nil { detailText = "Refresh failed: \(error.localizedDescription)\n\nShowing the previous result." }
        }
    }

    nonisolated private static func runCollector(_ url: URL) throws -> (output: String, error: String, status: Int32) {
        let process = Process()
        process.executableURL = url
        process.arguments = ["--config", NSString(string: "~/.config/mora-tentacles/config.json").expandingTildeInPath]
        // File-backed output avoids a full pipe blocking the collector before exit.
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        defer { try? FileManager.default.removeItem(at: directory) }
        let outURL = directory.appendingPathComponent("stdout")
        let errURL = directory.appendingPathComponent("stderr")
        FileManager.default.createFile(atPath: outURL.path, contents: nil, attributes: [.posixPermissions: 0o600])
        FileManager.default.createFile(atPath: errURL.path, contents: nil, attributes: [.posixPermissions: 0o600])
        let stdout = try FileHandle(forWritingTo: outURL), stderr = try FileHandle(forWritingTo: errURL)
        defer { try? stdout.close(); try? stderr.close() }
        process.standardOutput = stdout
        process.standardError = stderr
        try process.run()
        let deadline = Date().addingTimeInterval(30)
        while process.isRunning && Date() < deadline { Thread.sleep(forTimeInterval: 0.1) }
        if process.isRunning {
            process.terminate()
            let stopDeadline = Date().addingTimeInterval(1)
            while process.isRunning && Date() < stopDeadline { Thread.sleep(forTimeInterval: 0.05) }
            if process.isRunning { kill(process.processIdentifier, SIGKILL) }
            throw StatusError.collector("Collector timed out after 30 seconds.")
        }
        func boundedText(_ url: URL) throws -> String {
            let file = try FileHandle(forReadingFrom: url)
            defer { try? file.close() }
            let data = try file.read(upToCount: 1_048_577) ?? Data()
            guard data.count <= 1_048_576 else { throw StatusError.collector("Collector response exceeded the size limit.") }
            return String(data: data, encoding: .utf8) ?? ""
        }
        return (try boundedText(outURL), try boundedText(errURL), process.terminationStatus)
    }
}

struct StatusPanel: View {
    @ObservedObject var model: StatusModel
    var isPreview = false
    @State private var showDetails = false

    private var headline: String {
        if model.isRefreshing && model.snapshot == nil { return "Checking" }
        if model.error != nil && model.snapshot == nil { return "Unavailable" }
        if model.error != nil { return "Showing previous result" }
        guard let state = model.snapshot?.aggregate else { return "Unavailable" }
        switch state {
        case .healthy: return "Recent checks passed"
        case .failed: return "Needs attention"
        case .stale: return "Sync overdue"
        case .unknown: return "Checks incomplete"
        case .running: return "Sync in progress"
        }
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 15) {
            if isPreview {
                Label("PREVIEW · fixture data", systemImage: "eye")
                    .font(.system(size: 11, weight: .semibold)).foregroundStyle(.purple)
            }
            HStack(alignment: .center) {
                VStack(alignment: .leading, spacing: 3) {
                    Text("Mora").font(.system(size: 19, weight: .semibold))
                    Label(headline, systemImage: headlineSymbol)
                        .font(.system(size: 12, weight: .medium))
                        .foregroundStyle(headlineColor)
                }
                Spacer()
                Button { Task { await model.refresh() } } label: {
                    Image(systemName: model.isRefreshing ? "arrow.triangle.2.circlepath" : "arrow.clockwise")
                }
                .buttonStyle(.borderless)
                .disabled(model.isRefreshing)
                .help("Refresh now")
            }

            if let error = model.error {
                Label(error, systemImage: "exclamationmark.triangle.fill")
                    .font(.system(size: 11)).foregroundStyle(.orange).fixedSize(horizontal: false, vertical: true)
            }

            if let snapshot = model.snapshot {
                ScrollView {
                VStack(alignment: .leading, spacing: 10) {
                ForEach(snapshot.devices) { device in
                    deviceRow(device)
                    if device.id != snapshot.devices.last?.id { Divider() }
                }
                }
                }.frame(height: 470)
                if let lastUpdated = model.lastUpdated {
                    Text("Updated \(lastUpdated.formatted(.relative(presentation: .named)))")
                        .font(.system(size: 11)).foregroundStyle(.secondary)
                }
            } else if model.error == nil {
                HStack { ProgressView().controlSize(.small); Text("Checking tentacles…").foregroundStyle(.secondary) }
                    .font(.system(size: 12)).padding(.vertical, 12)
            } else {
                Text("No status data is available yet.").font(.system(size: 12)).foregroundStyle(.secondary)
            }

            Divider()
            HStack {
                Button("Open details") { showDetails = true }.buttonStyle(.link).font(.system(size: 11))
                Spacer()
                Button("Quit") { NSApplication.shared.terminate(nil) }.buttonStyle(.borderless).font(.system(size: 11))
            }
        }
        .padding(16)
        .frame(width: 400)
        .sheet(isPresented: $showDetails) {
            VStack(alignment: .leading, spacing: 10) {
                Text("Collector details").font(.headline)
                ScrollView { Text(model.detailText).font(.system(.caption, design: .monospaced)).textSelection(.enabled)
                    .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading) }
                HStack { Spacer(); Button("Done") { showDetails = false }.keyboardShortcut(.defaultAction) }
            }.padding(16).frame(width: 500, height: 330)
        }
    }

    @ViewBuilder private func deviceRow(_ device: DeviceStatus) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(spacing: 7) {
                Image(systemName: device.connection == .connected ? "laptopcomputer" : "laptopcomputer.and.arrow.down")
                    .foregroundStyle(device.connection == .connected ? Color.secondary : Color.orange)
                Text(device.name).font(.system(size: 13, weight: .semibold))
                Spacer()
                Text(device.connection.rawValue.capitalized).font(.system(size: 11)).foregroundStyle(.secondary)
            }
            if let detail = device.detail, !detail.isEmpty {
                Text(detail).font(.system(size: 11)).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            }
            if device.channels.isEmpty {
                Label("No channel data", systemImage: ChannelState.unknown.symbol)
                    .font(.system(size: 11)).foregroundStyle(.secondary)
            }
            ForEach(device.channels) { channel in
                HStack(alignment: .top, spacing: 7) {
                    Image(systemName: channel.state.symbol).foregroundStyle(color(for: channel.state)).frame(width: 14)
                    VStack(alignment: .leading, spacing: 2) {
                        HStack {
                            Text(channel.name).font(.system(size: 11, weight: .medium))
                            Spacer()
                            Text(channel.state.label).font(.system(size: 11)).foregroundStyle(color(for: channel.state))
                        }
                        if let detail = channel.detail, !detail.isEmpty {
                            Text(detail).font(.system(size: 11)).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                        }
                        if let last = channel.lastSuccess {
                            Text("Last success \(last.formatted(.relative(presentation: .named)))")
                                .font(.system(size: 11)).foregroundStyle(.secondary)
                        }
                    }
                }.padding(.leading, 22)
            }
        }.padding(.vertical, 2)
    }

    private var headlineSymbol: String {
        if model.error != nil { return "exclamationmark.circle.fill" }
        return model.snapshot?.aggregate.symbol ?? "questionmark.circle"
    }
    private var headlineColor: Color {
        if model.error != nil { return .orange }
        return model.snapshot.map { color(for: $0.aggregate) } ?? .secondary
    }
    private func color(for state: ChannelState) -> Color {
        switch state { case .healthy: .green; case .failed: .red; case .stale: .orange; case .unknown, .running: .secondary }
    }
}
