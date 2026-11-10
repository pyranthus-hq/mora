import AppKit
import SwiftUI

/// Opens the one review window. The window runs only review-list and review-act
/// through the collector. It never edits a note and never marks one as the owner's own.
@MainActor
final class ReviewWindowController {
    static let shared = ReviewWindowController()
    private var window: NSWindow?
    private var model: ReviewModel?

    func show(collector: URL, preview: Bool, onChange: @escaping () -> Void) {
        if let window, let model {
            window.makeKeyAndOrderFront(nil)
            NSApp.activate(ignoringOtherApps: true)
            Task { await model.load() }
            return
        }
        let model = ReviewModel(collector: collector, preview: preview, onChange: onChange)
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

@MainActor
final class ReviewModel: ObservableObject {
    @Published var notes: [ReviewNote] = []
    @Published var more = 0
    @Published var error: String?
    @Published var loading = false
    @Published var busy: Set<String> = []
    @Published var failures: [String: String] = [:]

    private let collector: URL
    private let preview: Bool
    private let onChange: () -> Void

    init(collector: URL, preview: Bool, onChange: @escaping () -> Void) {
        self.collector = collector
        self.preview = preview
        self.onChange = onChange
    }

    var moreLine: String? { ReviewList.moreLine(more) }

    func load() async {
        guard !loading else { return }
        loading = true
        defer { loading = false }
        failures = [:]
        if preview {
            notes = Self.fixtures; more = 0; error = nil
            return
        }
        do {
            let collector = self.collector
            let result = try await Task.detached(priority: .utility) { try StatusModel.runCollector(collector, arguments: ["review-list"]) }.value
            let list = try JSONDecoder().decode(ReviewList.self, from: Data(result.output.utf8))
            if let message = list.error { throw StatusError.collector(message) }
            guard result.status == 0 else { throw StatusError.collector("The collector could not list the notes.") }
            notes = list.notes; more = list.more; error = nil
        } catch {
            notes = []; more = 0
            self.error = Self.short(error.localizedDescription)
        }
    }

    func act(_ note: ReviewNote, keep: Bool) async {
        guard !busy.contains(note.id) else { return }
        busy.insert(note.id)
        defer { busy.remove(note.id) }
        failures[note.id] = nil
        if !preview {
            do {
                let collector = self.collector
                let args = ["review-act", keep ? "keep" : "discard", note.kind, note.id]
                let result = try await Task.detached(priority: .utility) { try StatusModel.runCollector(collector, arguments: args) }.value
                let done = try JSONDecoder().decode(ReviewActResult.self, from: Data(result.output.utf8))
                guard done.ok, result.status == 0 else { throw StatusError.collector(done.error ?? "The action did not complete.") }
            } catch {
                failures[note.id] = Self.short(error.localizedDescription)
                return
            }
        }
        notes.removeAll { $0.id == note.id }
        onChange()
    }

    private static func short(_ text: String) -> String { String(text.prefix(200)) }

    private static let fixtures: [ReviewNote] = [
        ReviewNote(id: "mem_fixture_saved", kind: "saved", scope: "global", type: "insight", title: "Fixture: a saved note", text: "Fixture text. No collector ran."),
        ReviewNote(id: "p_fixture_held", kind: "held", scope: "project:demo", type: "fact", title: "Fixture: a held note", text: "Fixture text for a note that is not saved yet."),
        ReviewNote(id: "mem_fixture_long", kind: "saved", scope: "global", type: "insight", title: "**Bold** [a link](https://example.com) https://example.com/path", text: (1...60).map { "Line \($0): **not bold**, [not a link](https://example.com), https://example.com/\($0)" }.joined(separator: "\n"))
    ]
}

struct ReviewView: View {
    @ObservedObject var model: ReviewModel
    @State private var pendingDiscard: ReviewNote?

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            Text(verbatim: "These notes were written by agents. Keep saves a note as an agent note. To mark a note as your own statement, run mora confirm <id> --mine in a terminal.")
                .font(.system(size: 11)).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            HStack {
                if model.loading { ProgressView().controlSize(.small) }
                Spacer()
                Button("Refresh") { Task { await model.load() } }.disabled(model.loading)
            }
            if let error = model.error {
                Text(verbatim: error).font(.system(size: 12)).foregroundStyle(.orange)
            } else if model.notes.isEmpty && !model.loading {
                Text("No agent notes wait for review.").font(.system(size: 12)).foregroundStyle(.secondary)
            }
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 10) {
                    ForEach(model.notes) { card($0) }
                    if let line = model.moreLine {
                        Text(verbatim: line).font(.system(size: 11)).foregroundStyle(.secondary)
                    }
                }
            }
        }
        .padding(14)
        .frame(minWidth: 400, minHeight: 360)
        .alert("Discard this note?", isPresented: Binding(get: { pendingDiscard != nil }, set: { if !$0 { pendingDiscard = nil } }), presenting: pendingDiscard) { note in
            Button("Discard", role: .destructive) { Task { await model.act(note, keep: false) } }
            Button("Cancel", role: .cancel) {}
        } message: { _ in
            Text("This removes it.")
        }
    }

    private func card(_ note: ReviewNote) -> some View {
        let working = model.busy.contains(note.id)
        return VStack(alignment: .leading, spacing: 6) {
            Text(verbatim: note.title).font(.system(size: 13, weight: .semibold)).textSelection(.enabled)
            Text(verbatim: note.kindLine).font(.system(size: 11)).foregroundStyle(.secondary).textSelection(.enabled)
            ScrollView {
                Text(verbatim: note.text).font(.system(size: 12)).textSelection(.enabled)
                    .frame(maxWidth: .infinity, alignment: .topLeading)
            }.frame(height: 130)
            if let failure = model.failures[note.id] {
                Text(verbatim: failure).font(.system(size: 11)).foregroundStyle(.orange)
            }
            HStack {
                Button("Keep") { Task { await model.act(note, keep: true) } }
                Button("Discard") { pendingDiscard = note }
                if working { ProgressView().controlSize(.small) }
            }.disabled(working)
        }
        .padding(10)
        .background(RoundedRectangle(cornerRadius: 8).fill(Color.secondary.opacity(0.08)))
    }
}
