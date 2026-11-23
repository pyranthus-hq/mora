import AppKit
import CryptoKit
import LocalAuthentication
import Security
import SwiftUI

/// The Secure Enclave key that signs Keep as my own words approvals. macOS asks
/// for Touch ID or the login password on every signature; the key never leaves
/// the Secure Enclave. Mora trusts its public key only after the owner runs
/// `mora review trust-app` in a terminal.
enum ApprovalKey {
    static var directory: URL {
        FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent("Library/Application Support/Mora Status", isDirectory: true)
    }
    static var blobURL: URL { directory.appendingPathComponent("approval-key") }
    static var publicURL: URL { directory.appendingPathComponent("approval-key.pem") }

    /// Loads the key, or makes one on first use (making it asks for nothing).
    static func load(context: LAContext? = nil) throws -> SecureEnclave.P256.Signing.PrivateKey {
        if let blob = try? Data(contentsOf: blobURL) {
            return try SecureEnclave.P256.Signing.PrivateKey(dataRepresentation: blob, authenticationContext: context)
        }
        guard let access = SecAccessControlCreateWithFlags(nil, kSecAttrAccessibleWhenUnlockedThisDeviceOnly, [.privateKeyUsage, .userPresence], nil) else {
            throw StatusError.collector("Could not make the approval key.")
        }
        let key = try SecureEnclave.P256.Signing.PrivateKey(accessControl: access, authenticationContext: context)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        try key.dataRepresentation.write(to: blobURL, options: .atomic)
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: blobURL.path)
        try Data(key.publicKey.pemRepresentation.utf8).write(to: publicURL, options: .atomic)
        return key
    }

    /// The same fingerprint `mora review trust-app` prints, or nil before the key exists.
    static var fingerprint: String? {
        guard let blob = try? Data(contentsOf: blobURL),
              let key = try? SecureEnclave.P256.Signing.PrivateKey(dataRepresentation: blob) else { return nil }
        return approvalFingerprint(key.publicKey.derRepresentation)
    }

    /// Signs one approval. Blocks while macOS shows the Touch ID or password prompt.
    static func sign(configDir: String, vault: String, kind: String, id: String, digest: String) throws -> (unix: String, signature: String) {
        let context = LAContext()
        context.touchIDAuthenticationAllowableReuseDuration = 0
        context.localizedReason = "keep this note as your own words"
        let key = try load(context: context)
        let unix = String(Int64(Date().timeIntervalSince1970))
        let signature = try key.signature(for: approvalMessage(configDir: configDir, vault: vault, kind: kind, id: id, digest: digest, unix: unix))
        return (unix, signature.derRepresentation.base64EncodedString())
    }
}

/// Opens the one review window. The window runs only review-list and review-act
/// through the collector. It never edits a note. It marks a note as the owner's
/// own only through the separate Keep as my own words button, after macOS
/// accepts Touch ID or the login password for that one note.
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
    @Published var gate = ReviewGate()
    @Published var busy: Set<String> = []
    @Published var failures: [String: String] = [:]
    @Published var keyFingerprint: String? = ApprovalKey.fingerprint
    @Published var configDir = ""
    @Published var vault = ""

    /// Keep as my own words also needs the destination, which the window shows and the key signs.
    func canKeepAsMine(_ note: ReviewNote) -> Bool { note.canKeepAsMine && !configDir.isEmpty && !vault.isEmpty }

    private let collector: URL
    private let preview: Bool
    private let onChange: () -> Void

    init(collector: URL, preview: Bool, onChange: @escaping () -> Void) {
        self.collector = collector
        self.preview = preview
        self.onChange = onChange
    }

    var moreLine: String? { ReviewList.moreLine(more) }

    var loading: Bool { gate.fetching }

    func load() async {
        // A result older than the last successful action is dropped and fetched again.
        while let number = gate.beginFetch() {
            failures = [:]
            var fetched: (notes: [ReviewNote], more: Int, error: String?)
            var destination = ("", "")
            if preview {
                fetched = (Self.fixtures, 0, nil)
                destination = ("/preview/config", "/preview/vault")
            } else {
                do {
                    let collector = self.collector
                    let result = try await Task.detached(priority: .utility) { try StatusModel.runCollector(collector, arguments: ["review-list"]) }.value
                    let list = try JSONDecoder().decode(ReviewList.self, from: Data(result.output.utf8))
                    if let message = list.error { throw StatusError.collector(message) }
                    guard result.status == 0 else { throw StatusError.collector("The collector could not list the notes.") }
                    fetched = (list.notes, list.more, nil)
                    destination = (list.configDir, list.vault)
                } catch {
                    fetched = ([], 0, Self.short(error.localizedDescription))
                }
            }
            if gate.finishFetch(number) {
                notes = fetched.notes; more = fetched.more; error = fetched.error
                (configDir, vault) = destination
                return
            }
        }
    }

    enum Action { case keep, mine, discard }

    func act(_ note: ReviewNote, _ action: Action) async {
        let allowed: Bool
        switch action {
        case .keep: allowed = note.canKeep
        case .mine: allowed = canKeepAsMine(note)
        case .discard: allowed = true
        }
        guard !busy.contains(note.id), allowed, gate.beginAction() else { return }
        busy.insert(note.id)
        defer { busy.remove(note.id) }
        failures[note.id] = nil
        var approval: (unix: String, signature: String)?
        if action == .mine, !preview {
            // The Secure Enclave asks for Touch ID or the password for this one signature.
            do {
                let kind = note.kind, id = note.id, digest = note.confirm, configDir = self.configDir, vault = self.vault
                approval = try await Task.detached(priority: .userInitiated) { try ApprovalKey.sign(configDir: configDir, vault: vault, kind: kind, id: id, digest: digest) }.value
                keyFingerprint = ApprovalKey.fingerprint
            } catch {
                failures[note.id] = "Not approved. Nothing was changed."
                gate.endAction(succeeded: false)
                return
            }
        }
        if !preview {
            do {
                let collector = self.collector
                var args = ["review-act", action == .keep ? "keep" : action == .mine ? "mine" : "discard", note.kind, note.id]
                if action == .mine, let approval { args += [note.confirm, approval.unix, approval.signature] }
                let result = try await Task.detached(priority: .utility) { try StatusModel.runCollector(collector, arguments: args) }.value
                let done = try JSONDecoder().decode(ReviewActResult.self, from: Data(result.output.utf8))
                guard done.ok, result.status == 0 else { throw StatusError.collector(done.error ?? "The action did not complete.") }
            } catch {
                failures[note.id] = Self.short(error.localizedDescription)
                gate.endAction(succeeded: false)
                return
            }
        }
        gate.endAction(succeeded: true)
        notes.removeAll { $0.id == note.id }
        onChange()
    }

    private static func short(_ text: String) -> String { String(text.prefix(200)) }

    private static let fixtures: [ReviewNote] = [
        ReviewNote(id: "p_fixture_redacted", kind: "held", scope: "project:demo", type: "fact", title: "Fixture: a redacted note", text: "token: [REDACTED]", redacted: true),
        ReviewNote(id: "mem_fixture_cut", kind: "saved", scope: "global", type: "insight", title: "Fixture: a cut note", text: "Fixture text that was cut.", truncated: true, hiddenChars: 5100),
        ReviewNote(id: "mem_fixture_saved", kind: "saved", scope: "global", type: "insight", title: "Fixture: a saved note", text: "Fixture text. No collector ran.", confirm: String(repeating: "a", count: 64), fields: [["scope", "global"], ["type", "insight"], ["title", "Fixture: a saved note"]]),
        ReviewNote(id: "p_fixture_held", kind: "held", scope: "project:demo", type: "fact", title: "Fixture: a held note", text: "Fixture text for a note that is not saved yet."),
        ReviewNote(id: "mem_fixture_long", kind: "saved", scope: "global", type: "insight", title: "**Bold** [a link](https://example.com) https://example.com/path", text: (1...60).map { "Line \($0): **not bold**, [not a link](https://example.com), https://example.com/\($0)" }.joined(separator: "\n"))
    ]
}

struct ReviewView: View {
    @ObservedObject var model: ReviewModel
    @State private var pendingDiscard: ReviewNote?

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            Text(verbatim: "These notes were written by agents. Keep saves a note as an agent note. Keep as my own words marks the exact text and fields shown as your own statement; macOS asks for Touch ID or your password first.")
                .font(.system(size: 11)).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            if let fingerprint = model.keyFingerprint {
                Text(verbatim: "Approval key \(fingerprint). Trust it once with mora review trust-app in Terminal.")
                    .font(.system(size: 10)).foregroundStyle(.secondary).textSelection(.enabled).fixedSize(horizontal: false, vertical: true)
            }
            HStack {
                if model.loading { ProgressView().controlSize(.small) }
                Spacer()
                Button("Refresh") { Task { await model.load() } }.disabled(!model.gate.canRefresh)
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
            Button("Discard", role: .destructive) { Task { await model.act(note, .discard) } }
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
            NoteText(text: note.text)
            if model.canKeepAsMine(note) {
                VStack(alignment: .leading, spacing: 1) {
                    ForEach(Array(note.fieldLines.enumerated()), id: \.offset) { Text(verbatim: $0.element) }
                    Text(verbatim: "goes to vault: \(model.vault)")
                }
                .font(.system(size: 10)).foregroundStyle(.secondary).textSelection(.enabled)
            }
            if let warning = note.keepWarning {
                Text(verbatim: warning).font(.system(size: 11)).foregroundStyle(.orange).fixedSize(horizontal: false, vertical: true)
            }
            if let failure = model.failures[note.id] {
                Text(verbatim: failure).font(.system(size: 11)).foregroundStyle(.orange)
            }
            HStack {
                if note.canKeep { Button("Keep") { Task { await model.act(note, .keep) } } }
                Button("Discard") { pendingDiscard = note }
                Spacer()
                if model.canKeepAsMine(note) { Button("Keep as my own words…") { Task { await model.act(note, .mine) } } }
                if working { ProgressView().controlSize(.small) }
            }.disabled(working || !model.gate.canAct)
        }
        .padding(10)
        .background(RoundedRectangle(cornerRadius: 8).fill(Color.secondary.opacity(0.08)))
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
