import AppKit
import SwiftUI

/// The review window is intentionally read-only until the core exposes an
/// enrolled-verifier decision receipt. It only renders the canonical CLI data.
@MainActor
final class ReviewWindowController {
    static let shared = ReviewWindowController()
    private var window: NSWindow?
    private var model: ReviewModel?

    func show(mora: URL, preview: Bool) {
        if let window, let model {
            window.makeKeyAndOrderFront(nil)
            NSApp.activate(ignoringOtherApps: true)
            Task { await model.load() }
            return
        }
        let model = ReviewModel(mora: mora, preview: preview)
        let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 520, height: 560), styleMask: [.titled, .closable, .miniaturizable, .resizable], backing: .buffered, defer: false)
        window.title = "Agent notes to review"
        window.isReleasedWhenClosed = false
        window.contentMinSize = NSSize(width: 400, height: 360)
        window.contentView = NSHostingView(rootView: ReviewView(model: model))
        window.center()
        window.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
        self.window = window
        self.model = model
        Task { await model.load() }
    }
}

enum ReviewLoadError: LocalizedError {
    case unavailable(String)
    case failed(String)

    var errorDescription: String? {
        switch self {
        case .unavailable(let text): return "Review is unavailable: \(text)"
        case .failed(let text): return "Review failed: \(text)"
        }
    }
}

@MainActor
final class ReviewModel: ObservableObject {
    @Published var notes: [ReviewRow] = []
    @Published var selected: ReviewPreview?
    @Published var queueError: ReviewLoadError?
    @Published var previewError: ReviewLoadError?
    @Published var loadingQueue = false
    @Published var loadingPreview = false

    private let mora: URL
    private let previewMode: Bool

    init(mora: URL, preview: Bool) {
        self.mora = mora
        previewMode = preview
    }

    func load() async {
        guard !loadingQueue else { return }
        loadingQueue = true
        queueError = nil
        previewError = nil
        defer { loadingQueue = false }
        do {
            if previewMode {
                let fixture = try Self.fixture()
                notes = [ReviewRow(id: fixture.id, kind: fixture.kind, scope: fixture.scope, type: fixture.type, title: fixture.title)]
                selected = fixture
                return
            }
            guard FileManager.default.isExecutableFile(atPath: mora.path) else {
                throw ReviewLoadError.unavailable("The installed Mora binary was not found.")
            }
            let mora = self.mora
            let result = try await Task.detached(priority: .utility) { try StatusModel.runCollector(mora, arguments: ["review", "--list", "--json"]) }.value
            guard result.status == 0 else { throw ReviewLoadError.failed(Self.commandMessage(result)) }
            let receipt = try JSONDecoder().decode(ReviewListReceipt.self, from: Data(result.output.utf8))
            guard receipt.isCanonical else { throw ReviewLoadError.failed("Mora returned an unsupported review list.") }
            notes = receipt.notes
            selected = nil
        } catch let error as ReviewLoadError {
            queueError = error
        } catch {
            queueError = .failed(Self.short(error.localizedDescription))
        }
    }

    func show(_ row: ReviewRow) async {
        guard !loadingPreview else { return }
        loadingPreview = true
        previewError = nil
        defer { loadingPreview = false }
        do {
            if previewMode {
                let fixture = try Self.fixture()
                guard fixture.id == row.id else { throw ReviewLoadError.failed("The fixture item is unavailable.") }
                selected = fixture
                return
            }
            let mora = self.mora
            let result = try await Task.detached(priority: .utility) { try StatusModel.runCollector(mora, arguments: ["review", "--show", row.kind, row.id, "--json"]) }.value
            guard result.status == 0 else { throw ReviewLoadError.failed(Self.commandMessage(result)) }
            let receipt = try JSONDecoder().decode(ReviewPreviewReceipt.self, from: Data(result.output.utf8))
            guard receipt.isCanonical, receipt.preview.id == row.id, receipt.preview.kind == row.kind else {
                throw ReviewLoadError.failed("Mora returned a mismatched preview.")
            }
            selected = receipt.preview
        } catch let error as ReviewLoadError {
            previewError = error
        } catch {
            previewError = .failed(Self.short(error.localizedDescription))
        }
    }

    private static func commandMessage(_ result: (output: String, error: String, status: Int32)) -> String {
        short(result.error.isEmpty ? "Mora exited with status \(result.status)." : result.error)
    }

    private static func short(_ text: String) -> String { String(text.prefix(200)) }

    private static func fixture() throws -> ReviewPreview {
        guard let url = Bundle.main.url(forResource: "ReviewPreviewFixture", withExtension: "json") else {
            throw ReviewLoadError.unavailable("The bundled review fixture is missing.")
        }
        let receipt = try JSONDecoder().decode(ReviewPreviewReceipt.self, from: Data(contentsOf: url))
        guard receipt.isCanonical else { throw ReviewLoadError.failed("The bundled review fixture is invalid.") }
        return receipt.preview
    }
}

struct ReviewView: View {
    @ObservedObject var model: ReviewModel

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack {
                Text("Agent notes to review").font(.system(size: 15, weight: .semibold))
                Spacer()
                if model.loadingQueue { ProgressView().controlSize(.small) }
                Button("Refresh") { Task { await model.load() } }
                    .disabled(model.loadingQueue || model.loadingPreview)
                    .accessibilityLabel("Refresh review queue")
            }
            Text("This window reads Mora’s saved and held queues. Desktop approvals are unavailable in this build, so no action can change a note.")
                .font(.system(size: 11)).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            if let error = model.queueError {
                Text(verbatim: error.localizedDescription).font(.system(size: 12)).foregroundStyle(.orange)
            } else if model.notes.isEmpty && !model.loadingQueue {
                Text("No agent notes wait for review.").font(.system(size: 12)).foregroundStyle(.secondary)
            }
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 8) {
                    ForEach(model.notes) { row in queueRow(row) }
                }
            }.frame(maxHeight: 155)
            Divider()
            previewArea
        }
        .padding(14)
        .frame(minWidth: 400, minHeight: 360)
    }

    @ViewBuilder private var previewArea: some View {
        if model.loadingPreview {
            HStack { ProgressView(); Text("Loading full preview…").font(.system(size: 12)) }
        } else if let error = model.previewError {
            Text(verbatim: error.localizedDescription).font(.system(size: 12)).foregroundStyle(.orange)
        } else if let preview = model.selected {
            previewCard(preview)
        } else {
            Text("Choose a note to see its full bounded preview.").font(.system(size: 12)).foregroundStyle(.secondary)
        }
    }

    private func queueRow(_ row: ReviewRow) -> some View {
        Button { Task { await model.show(row) } } label: {
            VStack(alignment: .leading, spacing: 2) {
                Text(verbatim: row.title).font(.system(size: 13, weight: .semibold)).lineLimit(1)
                Text(verbatim: row.summary).font(.system(size: 11)).foregroundStyle(.secondary)
            }.frame(maxWidth: .infinity, alignment: .leading).contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .padding(8)
        .background(RoundedRectangle(cornerRadius: 6).fill(Color.secondary.opacity(0.08)))
        .accessibilityLabel("Show preview for \(row.title)")
    }

    private func previewCard(_ preview: ReviewPreview) -> some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 7) {
                Text(verbatim: preview.title).font(.system(size: 14, weight: .semibold)).textSelection(.enabled)
                Text(verbatim: "\(preview.kind.capitalized) · \(preview.state) · \(preview.scope) · \(preview.type)").font(.system(size: 11)).foregroundStyle(.secondary).textSelection(.enabled)
                Text(verbatim: "Source: \(preview.source) · Provenance: \(preview.provenance)").font(.system(size: 11)).foregroundStyle(.secondary).textSelection(.enabled)
                NoteText(text: preview.text)
                ForEach(preview.details) { detail in
                    Text(verbatim: "\(detail.label): \(detail.value)").font(.system(size: 11)).textSelection(.enabled)
                }
                Text(verbatim: "Canonical revision: \(preview.canonicalRevision)").font(.system(size: 10)).foregroundStyle(.secondary).textSelection(.enabled)
                Text(verbatim: "Display digest: \(preview.displayDigest)").font(.system(size: 10)).foregroundStyle(.secondary).textSelection(.enabled)
                if !preview.complete {
                    Text(verbatim: "Incomplete preview: \(preview.incompleteness.joined(separator: ", ")). Keep is unavailable.")
                        .font(.system(size: 11)).foregroundStyle(.orange).fixedSize(horizontal: false, vertical: true)
                }
                Text("Index visibility is not reported by this read-only preview. Search may lag; this window does not claim the note is indexed.")
                    .font(.system(size: 11)).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                HStack {
                    if preview.complete { Button("Keep (unavailable)") {}.disabled(true) }
                    Button("Discard (unavailable)") {}.disabled(true)
                }
                Text("To make a decision, run mora review in your own terminal. This app has no enrolled trusted-verifier decision path.")
                    .font(.system(size: 11)).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            }
            .padding(10)
            .background(RoundedRectangle(cornerRadius: 8).fill(Color.secondary.opacity(0.08)))
        }
    }
}

/// The note text at its own height, up to 180 points; longer text scrolls inside.
private struct NoteText: View {
    let text: String

    private var label: some View {
        Text(verbatim: text).font(.system(size: 12)).textSelection(.enabled)
            .frame(maxWidth: .infinity, alignment: .topLeading)
    }

    var body: some View {
        ViewThatFits(in: .vertical) {
            label.fixedSize(horizontal: false, vertical: true)
            ScrollView { label }
        }
        .frame(maxHeight: 180)
    }
}
