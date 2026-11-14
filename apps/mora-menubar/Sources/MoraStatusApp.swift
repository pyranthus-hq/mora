import Darwin
import AppKit
import SwiftUI

@main
struct MoraStatusApp: App {
    private let preview = CommandLine.arguments.contains("--preview")
    @StateObject private var model: StatusModel
    @NSApplicationDelegateAdaptor(PreviewWindowDelegate.self) private var appDelegate

    init() {
        signal(SIGPIPE, SIG_IGN)
        _model = StateObject(wrappedValue: StatusModel(preview: CommandLine.arguments.contains("--preview")))
    }

    private var needsAttention: Bool {
        guard model.error == nil, let snapshot = model.snapshot else { return true }
        return snapshot.menuNeedsAttention
    }

    var body: some Scene {
        MenuBarExtra {
            StatusPanel(model: model)
        } label: {
            Image(nsImage: needsAttention ? MenuBarIcon.attention : MenuBarIcon.normal)
            .accessibilityLabel(needsAttention ? "Mora Status, needs attention" : "Mora Status")
            .help(needsAttention ? "Mora — needs attention" : "Mora")
        }
        .menuBarExtraStyle(.window)
    }
}

private struct MoraMark: View {
    let size: CGFloat
    private static let icon = MenuBarIcon.normal

    var body: some View {
        Image(nsImage: Self.icon).renderingMode(.template)
            .resizable().scaledToFit()
            .frame(width: size, height: size)
            .accessibilityHidden(true)
    }
}

final class PreviewWindowDelegate: NSObject, NSApplicationDelegate {
    private var window: NSWindow?
    func applicationDidFinishLaunching(_ notification: Notification) {
        // Finder/login launches intentionally open review; --preview remains fixture-only.
        if CommandLine.arguments.contains("--review") || !CommandLine.arguments.contains("--preview") {
            let directory = Bundle.main.executableURL!.deletingLastPathComponent()
            ReviewWindowController.shared.show(mora: directory.appendingPathComponent("mora"), preview: CommandLine.arguments.contains("--preview"))
            return
        }
        guard CommandLine.arguments.contains("--preview") else { return }
        let view = StatusPanel(model: StatusModel(preview: true), isPreview: true)
        let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 320, height: 300), styleMask: [.titled, .closable], backing: .buffered, defer: false)
        window.title = "Mora Status Preview · fixture data"
        let hosting = NSHostingView(rootView: view)
        window.contentView = hosting
        window.setContentSize(hosting.fittingSize)
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
    let collector: URL
    let reviewBinary: URL
    private let preview: Bool

    init(preview: Bool = false) {
        self.preview = preview
        collector = Bundle.main.executableURL!.deletingLastPathComponent().appendingPathComponent("mora-tentacles")
        reviewBinary = Bundle.main.executableURL!.deletingLastPathComponent().appendingPathComponent("mora")
        if preview {
            snapshot = try? StatusSnapshot.decode(Data(#"{"generated_at":"2026-11-08T08:00:00Z","devices":[{"id":"mac","name":"This Mac","connection":"connected","channels":[{"name":"Work shares","state":"healthy","detail":"Fixture data — no sync was checked.","last_success":"2026-11-08T07:45:00Z"},{"name":"Personal archive","state":"stale","detail":"Fixture data — displayed to demonstrate a stale channel.","last_success":"2026-11-07T07:45:00Z"}]},{"id":"devbox","name":"Devbox","connection":"connected","channels":[{"name":"Work shares","state":"healthy","detail":"Fixture data — no sync was checked."}]},{"id":"windows","name":"Windows (WSL)","connection":"connected","channels":[{"name":"Windows → Mac","state":"unknown","detail":"Fixture data — return path not configured."}]}],"review":{"saved":3,"held":1},"given":{"at":"2026-11-08T07:30:00Z","surface":"hook-session-start","count":3},"sources":[{"key":"gmail:work","state":"fresh","last_success_at":"2026-11-08T07:45:00Z","detail":"Fixture data — no source content was read."},{"key":"calendar","state":"stale","last_success_at":"2026-11-07T07:45:00Z","detail":"Fixture data — stale connector example."},{"key":"imessage","state":"provider-paused","detail":"Fixture data — unknown state example."}]}"#.utf8))
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

    nonisolated static func runCollector(_ url: URL, arguments: [String]? = nil) throws -> (output: String, error: String, status: Int32) {
        let process = Process()
        process.executableURL = url
        process.arguments = arguments ?? ["--config", NSString(string: "~/.config/mora-tentacles/config.json").expandingTildeInPath]
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
    @State private var expandedDevice: String?
    @State private var expandedSources = false

    private var headline: String {
        if model.isRefreshing && model.snapshot == nil { return "Checking…" }
        if model.error != nil { return model.snapshot == nil ? "Unavailable" : "Previous result" }
        return model.snapshot?.aggregate.summary ?? "Checking…"
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            if isPreview {
                Text("PREVIEW · fixture data").font(.system(size: 10)).foregroundStyle(.secondary)
            }
            HStack(spacing: 9) {
                MoraMark(size: 26)
                VStack(alignment: .leading, spacing: 2) {
                    Text("Mora").font(.system(size: 15, weight: .semibold))
                    Text(headline).font(.system(size: 11)).foregroundStyle(.secondary)
                }
                Spacer()
                Button { Task { await model.refresh() } } label: {
                    Image(systemName: "arrow.clockwise").font(.system(size: 12))
                }
                .buttonStyle(.borderless).disabled(model.isRefreshing)
                .accessibilityLabel("Refresh status").help("Refresh status")
            }

            Button {
                ReviewWindowController.shared.show(mora: model.reviewBinary, preview: isPreview)
            } label: {
                VStack(alignment: .leading, spacing: 2) {
                    Text(model.snapshot?.review?.rowText ?? "Review agent notes").font(.system(size: 12))
                    Text("Open the read-only saved and held queues.").font(.system(size: 11)).foregroundStyle(.secondary)
                }.frame(maxWidth: .infinity, alignment: .leading).contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel("Review agent notes")
            .accessibilityHint("Open the read-only saved and held queues")

            if let given = model.snapshot?.given, let text = given.rowText(now: Date(), timeZone: .current) {
                VStack(alignment: .leading, spacing: 2) {
                    Text(text).font(.system(size: 12))
                    Text([given.surfaceText, "Run mora given in a terminal for the list."].compactMap { $0 }.joined(separator: " "))
                        .font(.system(size: 11)).foregroundStyle(.secondary)
                }
            }

            if let error = model.error {
                Text(error).font(.system(size: 11)).foregroundStyle(.orange)
                    .fixedSize(horizontal: false, vertical: true)
            }

            if let snapshot = model.snapshot {
                ScrollView {
                    VStack(spacing: 0) {
                        ForEach(snapshot.devices) { device in
                            deviceRow(device)
                            if device.id != snapshot.devices.last?.id { Divider() }
                        }
                        Divider()
                        sourceRow(snapshot)
                    }
                }.frame(height: listHeight(snapshot.devices))
            } else if model.error == nil {
                ProgressView().controlSize(.small).padding(.vertical, 8)
            }

            Divider()
            HStack(spacing: 12) {
                if let lastUpdated = model.lastUpdated {
                    HStack(spacing: 3) {
                        Text("Checked")
                        Text(lastUpdated, style: .relative)
                        Text("ago")
                    }.foregroundStyle(.secondary).accessibilityElement(children: .combine)
                }
                Spacer()
                Button("Details") { showDetails = true }.buttonStyle(.borderless)
                Button("Quit") { NSApplication.shared.terminate(nil) }.buttonStyle(.borderless)
            }.font(.system(size: 10))
        }
        .padding(14)
        .frame(width: 320)
        .fixedSize(horizontal: false, vertical: true)
        .sheet(isPresented: $showDetails) {
            VStack(alignment: .leading, spacing: 10) {
                Text("Collector details").font(.headline)
                ScrollView { Text(model.detailText).font(.system(.caption, design: .monospaced)).textSelection(.enabled)
                    .frame(maxWidth: .infinity, alignment: .topLeading) }
                HStack { Spacer(); Button("Done") { showDetails = false }.keyboardShortcut(.defaultAction) }
            }.padding(16).frame(width: 500, height: 330)
        }
    }

    private func deviceRow(_ device: DeviceStatus) -> some View {
        let expanded = expandedDevice == device.id
        return VStack(alignment: .leading, spacing: 0) {
            Button {
                expandedDevice = expanded ? nil : device.id
            } label: {
                HStack(spacing: 9) {
                    Image(systemName: device.state.symbol)
                        .foregroundStyle(color(for: device.state)).frame(width: 14)
                    VStack(alignment: .leading, spacing: 3) {
                        Text(device.name).font(.system(size: 12, weight: .medium))
                        Text(device.state.summary).font(.system(size: 10)).foregroundStyle(.secondary)
                    }
                    Spacer()
                    Text(device.connection.rawValue.capitalized).font(.system(size: 10)).foregroundStyle(.secondary)
                    Image(systemName: expanded ? "chevron.down" : "chevron.right")
                        .font(.system(size: 9, weight: .semibold)).foregroundStyle(.tertiary)
                }.frame(height: 52).contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel("\(device.name), \(device.connection.rawValue), \(device.state.summary)")
            .accessibilityValue(expanded ? "Expanded" : "Collapsed")
            .accessibilityHint("Show or hide channel details")

            if expanded {
                ScrollView {
                    VStack(alignment: .leading, spacing: 10) {
                        if let detail = device.detail, !detail.isEmpty {
                            Text(detail).foregroundStyle(.secondary)
                                .fixedSize(horizontal: false, vertical: true)
                        }
                        if device.connection != .connected {
                            Text("Device unavailable; channel results may be cached.").foregroundStyle(.orange)
                        }
                        if device.channels.isEmpty { Text("No channel data").foregroundStyle(.secondary) }
                        ForEach(device.channels) { channel in
                            VStack(alignment: .leading, spacing: 3) {
                                HStack {
                                    Text(channel.name).fontWeight(.medium)
                                    Spacer()
                                    Text(channel.state.label).foregroundStyle(color(for: channel.state))
                                }
                                if let last = channel.lastSuccess {
                                    Text("\(channel.name == "Search index" ? "Last indexed snapshot" : "Last success") \(last.formatted(.relative(presentation: .named)))")
                                        .foregroundStyle(.secondary)
                                }
                                if let detail = channel.detail, !detail.isEmpty {
                                    Text(detail).foregroundStyle(.secondary)
                                        .fixedSize(horizontal: false, vertical: true)
                                }
                            }
                        }
                    }.font(.system(size: 10)).frame(maxWidth: .infinity, alignment: .leading)
                }
                .frame(height: channelHeight(device))
                .padding(.leading, 23).padding(.bottom, 10)
            }
        }
    }

    private func channelHeight(_ device: DeviceStatus) -> CGFloat {
        min(240, CGFloat(max(1, device.channels.count)) * 82 + (device.detail == nil ? 0 : 30))
    }

    private func listHeight(_ devices: [DeviceStatus]) -> CGFloat {
        let rows = CGFloat(devices.count * 52 + max(0, devices.count - 1))
        let expanded = devices.first { $0.id == expandedDevice }.map { channelHeight($0) + 10 } ?? 0
        let sourceHeight: CGFloat
        if expandedSources, let snapshot = model.snapshot {
            if let sourcesError = snapshot.sourcesError { sourceHeight = min(100, CGFloat(sourcesError.count / 45 + 50)) }
            else { sourceHeight = min(220, CGFloat(max(1, snapshot.sources?.count ?? 0)) * 58) }
        } else { sourceHeight = 0 }
        return min(400, rows + 50 + expanded + sourceHeight)
    }

    private func sourceRow(_ snapshot: StatusSnapshot) -> some View {
        VStack(alignment: .leading, spacing: 0) {
            Button { expandedSources.toggle() } label: {
                HStack(spacing: 9) {
                    Image(systemName: sourceSummarySymbol(snapshot))
                        .foregroundStyle(sourceSummaryColor(snapshot)).frame(width: 14)
                    VStack(alignment: .leading, spacing: 3) {
                        Text("Sources").font(.system(size: 12, weight: .medium))
                        Text(model.error == nil ? snapshot.sourcesSummary : "Previous · \(snapshot.sourcesSummary)")
                            .font(.system(size: 10)).foregroundStyle(model.error == nil ? Color.secondary : Color.orange)
                    }
                    Spacer()
                    Image(systemName: expandedSources ? "chevron.down" : "chevron.right")
                        .font(.system(size: 9, weight: .semibold)).foregroundStyle(.tertiary)
                }.frame(height: 48).contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel("Enabled connector sources, \(snapshot.sourcesSummary)")
            .accessibilityValue(expandedSources ? "Expanded" : "Collapsed")
            .accessibilityHint("Show or hide connector freshness details")

            if expandedSources {
                VStack(alignment: .leading, spacing: 9) {
                    if model.error != nil {
                        Text("Previous snapshot — source results may be stale.")
                            .foregroundStyle(.orange).fixedSize(horizontal: false, vertical: true)
                    }
                    if let error = snapshot.sourcesError {
                        Text(error).foregroundStyle(.orange).fixedSize(horizontal: false, vertical: true)
                    } else if let sources = snapshot.sources {
                        if sources.isEmpty {
                            Text("No enabled sources.").foregroundStyle(.secondary)
                        }
                        ForEach(sources) { source in
                            VStack(alignment: .leading, spacing: 3) {
                                HStack(spacing: 6) {
                                    Image(systemName: source.symbol).foregroundStyle(sourceColor(source))
                                    Text(source.key).fontWeight(.medium)
                                    Spacer()
                                    Text(source.label).foregroundStyle(sourceColor(source))
                                }
                                if let last = source.lastSuccessAt {
                                    Text("Last source sync \(last.formatted(.relative(presentation: .named)))")
                                        .foregroundStyle(.secondary)
                                }
                                if let detail = source.detail, !detail.isEmpty {
                                    Text(detail).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                                }
                            }
                        }
                    } else {
                        Text("Source checks unavailable.").foregroundStyle(.secondary)
                    }
                }
                .font(.system(size: 10)).frame(maxWidth: .infinity, alignment: .leading)
                .padding(.leading, 23).padding(.bottom, 10)
            }
        }
    }

    private func sourceColor(_ source: SourceStatus) -> Color {
        switch source.normalizedState { case "fresh": .green; case "stale": .orange; case "failed": .red; default: .secondary }
    }
    private func sourceSummarySymbol(_ snapshot: StatusSnapshot) -> String {
        if model.error != nil { return "questionmark.circle.fill" }
        if snapshot.sourcesError != nil || snapshot.sources == nil { return "questionmark.circle.fill" }
        if snapshot.sources?.contains(where: { $0.normalizedState == "failed" }) == true { return "xmark.circle.fill" }
        if snapshot.sources?.contains(where: { $0.normalizedState == "stale" }) == true { return "clock.fill" }
        if snapshot.sources?.isEmpty == false && snapshot.sources?.allSatisfy({ $0.normalizedState == "fresh" }) == true { return "checkmark.circle.fill" }
        return "questionmark.circle.fill"
    }
    private func sourceSummaryColor(_ snapshot: StatusSnapshot) -> Color {
        if model.error != nil { return .secondary }
        if snapshot.sourcesError != nil || snapshot.sources == nil { return .secondary }
        if snapshot.sources?.contains(where: { $0.normalizedState == "failed" }) == true { return .red }
        if snapshot.sources?.contains(where: { $0.normalizedState == "stale" }) == true { return .orange }
        if snapshot.sources?.isEmpty == false && snapshot.sources?.allSatisfy({ $0.normalizedState == "fresh" }) == true { return .green }
        return .secondary
    }

    private func color(for state: ChannelState) -> Color {
        switch state { case .healthy: .green; case .failed: .red; case .stale: .orange; case .unknown, .running: .secondary }
    }
}
