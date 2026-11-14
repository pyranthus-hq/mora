import AppKit
import SwiftUI

@MainActor
final class ReviewWindowController {
    static let shared = ReviewWindowController()
    private var window: NSWindow?
    private var model: ReviewModel?

    func close() { window?.close() }

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
    @Published var approvalStatus = "Desktop approvals are unavailable in this build."
    @Published var ownerStatus: OwnerProofStatus?
    @Published var applying = false
    @Published var decisionResult = ""
    @Published var indexVisibilityResult = ""
    @Published var projects: [ProjectRuleProject] = []
    @Published var projectRules: [ProjectRuleRow] = []
    @Published var selectedProject: ProjectRuleProject?
    @Published var projectRuleError: ReviewLoadError?
    @Published var loadingProjectRules = false

    private let mora: URL
    private let previewMode: Bool
    private var state = ReviewSelectionState()
    private var projectState = ProjectRuleSelectionState()
    private var selectedProjectRoot: String?
    private var selectedProjectAction: String?

    init(mora: URL, preview: Bool) {
        self.mora = mora
        previewMode = preview
    }

    func load() async {
        guard !loadingQueue else { return }
        loadingQueue = true
        queueError = nil
        previewError = nil
        notes = []; selected = nil; loadingPreview = false
        let generation = state.beginRefresh()
        defer { if generation == state.generation { loadingQueue = false } }
        do {
            if previewMode {
                let receipt = try Self.fixture()
                let row = ReviewRow(id: receipt.preview.id, kind: receipt.preview.kind, scope: receipt.preview.scope, type: receipt.preview.type, title: receipt.preview.title)
                guard row.isSafe, state.acceptQueue([row], generation: generation) else { return }
                notes = [row]
            } else {
                guard FileManager.default.isExecutableFile(atPath: mora.path) else {
                    throw ReviewLoadError.unavailable("The installed Mora binary was not found.")
                }
                let mora = self.mora
                let result = try await Task.detached(priority: .utility) { try StatusModel.runCollector(mora, arguments: ["review", "--list", "--json"]) }.value
                guard result.status == 0 else { throw ReviewLoadError.failed(Self.commandMessage(result)) }
                let receipt = try JSONDecoder().decode(ReviewListReceipt.self, from: Data(result.output.utf8))
                let rows = try receipt.validated()
                guard state.acceptQueue(rows, generation: generation) else { return }
                notes = rows
            }
        } catch let error as ReviewLoadError {
            if generation == state.generation { notes = []; selected = nil; queueError = error }
        } catch {
            if generation == state.generation { notes = []; selected = nil; queueError = .failed(Self.short(error.localizedDescription)) }
        }
        await loadProjectRules()
        await refreshApprovalStatus()
    }

    func selectProject(_ project: ProjectRuleProject) async {
        selectedProject = project; projectRules = []; clearProjectPreview()
        await loadProjectRules(project.root)
    }

    func loadProjectRules(_ root: String? = nil) async {
        loadingProjectRules = true; projectRuleError = nil
        let generation = projectState.beginRefresh(root: root)
        defer { if generation == projectState.generation { loadingProjectRules = false } }
        do {
            if previewMode {
                let fixture = try Self.projectFixture()
                if root == nil {
                    guard projectState.acceptsProjects(generation: generation) else { return }
                    projects = fixture.projects
                    let nextProject = projectState.selectedProject(from: fixture.projects, keeping: selectedProject)
                    if selectedProject != nextProject { selectedProject = nextProject; clearProjectPreview() }
                    if let project = selectedProject {
                        loadingProjectRules = false
                        await loadProjectRules(project.root)
                    }
                    return
                }
                guard projectState.accept(fixture.rules, root: root, generation: generation) else { return }
                projectRules = fixture.rules
                return
            }
            let client = NativeReviewClient(mora: mora)
            let receipt = try await Task.detached(priority: .utility) { try client.projectRules(root: root) }.value
            if root == nil {
                guard projectState.acceptsProjects(generation: generation) else { return }
                projects = receipt.projects
                let nextProject = projectState.selectedProject(from: receipt.projects, keeping: selectedProject)
                if selectedProject != nextProject { selectedProject = nextProject; clearProjectPreview() }
                if let project = selectedProject {
                    loadingProjectRules = false
                    await loadProjectRules(project.root)
                }
                return
            }
            guard projectState.accept(receipt.rules, root: root, generation: generation) else { return }
            projectRules = receipt.rules
        } catch {
            if generation == projectState.generation { projectRules = []; projectRuleError = .failed(Self.short(error.localizedDescription)) }
        }
    }

    func refreshApprovalStatus() async {
        guard !previewMode else { approvalStatus = "Preview fixture mode is read-only."; ownerStatus = nil; return }
        do {
            let client = NativeReviewClient(mora: mora)
            let status = try await Task.detached(priority: .utility) { try client.status() }.value
            ownerStatus = status
            approvalStatus = status.enrolled && !status.readOnly ? "Desktop approvals are enabled for this installed app." : "Enable desktop approvals in a production-signed Mora Status app."
        } catch {
            ownerStatus = nil
            approvalStatus = "Desktop approvals are unavailable in this build."
        }
    }

    func enableApprovals() async {
        guard !previewMode, !applying else { return }
        applying = true; defer { applying = false }
        do {
            let client = NativeReviewClient(mora: mora)
            let status = try await Task.detached(priority: .utility) { try client.status() }.value
            try await Task.detached(priority: .userInitiated) { try client.enroll(status: status) }.value
            await refreshApprovalStatus()
        } catch { approvalStatus = error.localizedDescription }
    }

    func decide(_ action: String, preview: ReviewPreview) async {
        guard !previewMode, !applying, let status = ownerStatus, status.enrolled, !status.readOnly else { return }
        if action == "keep" && !preview.isKeepEligible { return }
        decisionResult = ""
        indexVisibilityResult = ""
        applying = true; defer { applying = false }
        do {
            let client = NativeReviewClient(mora: mora)
            let outcome: OwnerProofResponse.Outcome
            if preview.kind == "project_rule", let root = selectedProjectRoot, selectedProjectAction == action {
                outcome = try await Task.detached(priority: .userInitiated) { try client.decideProjectRule(action, preview: preview, root: root, status: status) }.value
            } else {
                outcome = try await Task.detached(priority: .userInitiated) { try client.decide(action, preview: preview, status: status) }.value
            }
            indexVisibilityResult = "Index visibility: \(outcome.indexVisibility.replacingOccurrences(of: "_", with: " "))."
            switch outcome.status {
            case "cancelled": decisionResult = "Cancelled. Note unchanged."
            case "retired", "retired_loader_conflict":
                decisionResult = outcome.status == "retired" ? "Retired." : "Retired, but local instructions need attention."
                clearProjectPreview()
                await loadProjectRules(selectedProject?.root)
            case "applied": decisionResult = "Applied."; await load()
            case "stale": decisionResult = "Changed since shown. Not applied."
            default: decisionResult = "Outcome unknown. Refresh before retrying."
            }
        } catch { decisionResult = "Outcome unknown. Refresh before retrying." }
    }

    func show(_ row: ReviewRow) async {
		guard !applying else { return }
        guard let token = state.beginSelection(row) else {
            previewError = .failed(ReviewReceiptError.invalidRow.localizedDescription)
            return
        }
        loadingPreview = true
        previewError = nil
        defer { if token.queue == state.generation && token.selection == state.selectionGeneration { loadingPreview = false } }
        do {
            if previewMode {
                let receipt = try Self.fixture()
                let fixture = try receipt.validated(for: row)
                guard state.acceptsPreview(fixture, row: row, queue: token.queue, selection: token.selection) else { return }
                selected = fixture
                return
            }
            let mora = self.mora
            let result = try await Task.detached(priority: .utility) { try StatusModel.runCollector(mora, arguments: ["review", "--show", row.kind, row.id, "--json"]) }.value
            guard result.status == 0 else { throw ReviewLoadError.failed(Self.commandMessage(result)) }
            let receipt = try JSONDecoder().decode(ReviewPreviewReceipt.self, from: Data(result.output.utf8))
            let preview = try receipt.validated(for: row)
            guard state.acceptsPreview(preview, row: row, queue: token.queue, selection: token.selection) else { return }
            selected = preview
        } catch let error as ReviewLoadError {
            if token.queue == state.generation && token.selection == state.selectionGeneration { previewError = error }
        } catch {
            if token.queue == state.generation && token.selection == state.selectionGeneration { previewError = .failed(Self.short(error.localizedDescription)) }
        }
    }

    func showProjectRule(_ row: ProjectRuleRow, action: String) async {
        guard !applying, let project = selectedProject, let token = projectState.beginPreview(row, root: project.root, action: action) else { return }
        loadingPreview = true; previewError = nil; selected = nil; selectedProjectRoot = nil; selectedProjectAction = nil
        defer { if token.queue == projectState.generation && token.selection == projectState.selectionGeneration { loadingPreview = false } }
        do {
            let preview: ReviewPreview
            if previewMode { preview = try ProjectRuleFixture.preview(for: row, action: action) }
            else {
                let client = NativeReviewClient(mora: mora)
                preview = try await Task.detached(priority: .utility) { try client.projectRulePreview(row: row, root: project.root, action: action) }.value
            }
            guard projectState.accepts(preview, row: row, root: project.root, action: action, queue: token.queue, selection: token.selection) else { return }
            selected = preview; selectedProjectRoot = project.root; selectedProjectAction = action
        } catch {
            if token.queue == projectState.generation && token.selection == projectState.selectionGeneration { previewError = .failed(Self.short(error.localizedDescription)) }
        }
    }

    private static func commandMessage(_ result: (output: String, error: String, status: Int32)) -> String {
        short(result.error.isEmpty ? "Mora exited with status \(result.status)." : result.error)
    }

    private static func short(_ text: String) -> String { String(text.prefix(200)) }

    private static func fixture() throws -> ReviewPreviewReceipt {
        guard let url = Bundle.main.url(forResource: "ReviewPreviewFixture", withExtension: "json") else {
            throw ReviewLoadError.unavailable("The bundled review fixture is missing.")
        }
        let receipt = try JSONDecoder().decode(ReviewPreviewReceipt.self, from: Data(contentsOf: url))
        guard receipt.schema == "mora.review.preview", receipt.schemaVersion == 1 else { throw ReviewLoadError.failed("The bundled review fixture is invalid.") }
        return receipt
    }

    private static func projectFixture(action: String = "retire") throws -> (projects: [ProjectRuleProject], rules: [ProjectRuleRow], preview: ReviewPreview) {
        guard let rule = ProjectRuleFixture.rules.first else { throw ReviewLoadError.failed("Synthetic project rules are missing.") }
        return ([ProjectRuleFixture.project], ProjectRuleFixture.rules, try ProjectRuleFixture.preview(for: rule, action: action))
    }

    private func clearProjectPreview() {
        selected = nil; selectedProjectRoot = nil; selectedProjectAction = nil; loadingPreview = false; previewError = nil
    }
}

struct ReviewView: View {
    @ObservedObject var model: ReviewModel
    @State private var confirmationAction: String?
    @State private var confirmationPreview: ReviewPreview?

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack {
                Text("Agent notes to review").font(.system(size: 15, weight: .semibold))
                Spacer()
                if model.loadingQueue { ProgressView().controlSize(.small) }
                Button("Refresh") { Task { await model.load() } }
                    .disabled(model.loadingQueue || model.loadingPreview || model.applying)
                    .accessibilityLabel("Refresh review queue")
            }
            Text("This window renders Mora’s bounded canonical receipts. Keep and Discard require a fresh native owner proof bound to the preview shown here.")
                .font(.system(size: 11)).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            if !model.decisionResult.isEmpty { Text(verbatim: model.decisionResult).font(.system(size: 11)).fixedSize(horizontal: false, vertical: true) }
            if !model.indexVisibilityResult.isEmpty { Text(verbatim: model.indexVisibilityResult).font(.system(size: 11)).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true) }
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
            projectRulesArea
            Divider()
            previewArea
        }
        .padding(14)
        .frame(minWidth: 400, minHeight: 360)
        .onExitCommand { ReviewWindowController.shared.close() }
        .confirmationDialog("Confirm review decision", isPresented: Binding(get: { confirmationAction != nil }, set: { if !$0 { confirmationAction = nil; confirmationPreview = nil } })) {
            Button(confirmationAction == "keep" ? "Keep" : confirmationAction == "activate" ? "Activate in project" : confirmationAction == "retire" ? "Retire from project" : "Discard", role: confirmationAction == "discard" || confirmationAction == "retire" ? .destructive : nil) {
                if let action = confirmationAction, let preview = confirmationPreview { Task { await model.decide(action, preview: preview) } }
                confirmationAction = nil; confirmationPreview = nil
            }
            Button("Cancel", role: .cancel) { confirmationAction = nil; confirmationPreview = nil }
        } message: { Text("The displayed preview will be bound to a fresh native approval. Cancel leaves the note unchanged.") }
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
		.disabled(model.applying)
        .padding(8)
        .background(RoundedRectangle(cornerRadius: 6).fill(Color.secondary.opacity(0.08)))
        .accessibilityLabel("Show preview for \(row.title)")
    }

    @ViewBuilder private var projectRulesArea: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack {
                Text("Project rules").font(.system(size: 13, weight: .semibold))
                Spacer()
                if model.loadingProjectRules { ProgressView().controlSize(.small) }
            }
            Text("Activate or retire an exact Codex project rule only after reviewing the server-provided preview.")
                .font(.system(size: 11)).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            if model.isPreviewMode { Text("Synthetic project-rule fixture — read-only.").font(.system(size: 11)).foregroundStyle(.secondary) }
            if !model.projects.isEmpty {
                Picker("Bound project", selection: Binding(get: { model.selectedProject?.id ?? "" }, set: { id in
                    if let project = model.projects.first(where: { $0.id == id }) { Task { await model.selectProject(project) } }
                })) {
                    ForEach(model.projects) { project in Text(verbatim: project.root).tag(project.id) }
                }.labelsHidden().disabled(model.applying || model.loadingProjectRules)
            }
            if let error = model.projectRuleError {
                Text(verbatim: error.localizedDescription).font(.system(size: 11)).foregroundStyle(.orange)
            } else if model.selectedProject != nil && model.projectRules.isEmpty && !model.loadingProjectRules {
                Text("No eligible project rules were returned for this bound project.").font(.system(size: 11)).foregroundStyle(.secondary)
            }
            ForEach(model.projectRules) { rule in
                HStack(alignment: .top, spacing: 8) {
                    VStack(alignment: .leading, spacing: 2) {
                        Text(verbatim: rule.title).font(.system(size: 12, weight: .semibold)).lineLimit(1)
                        Text(verbatim: rule.scope).font(.system(size: 10)).foregroundStyle(.secondary)
                    }
                    Spacer()
                    Button("Activate") { Task { await model.showProjectRule(rule, action: "activate") } }
                        .disabled(model.applying || model.loadingPreview)
                    Button("Retire") { Task { await model.showProjectRule(rule, action: "retire") } }
                        .disabled(model.applying || model.loadingPreview)
                }.padding(7).background(RoundedRectangle(cornerRadius: 6).fill(Color.secondary.opacity(0.06)))
            }
        }
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
                if !preview.isKeepEligible {
                    Text(verbatim: "Incomplete preview: \(preview.incompleteness.joined(separator: ", ")). Keep is unavailable.")
                        .font(.system(size: 11)).foregroundStyle(.orange).fixedSize(horizontal: false, vertical: true)
                }
                Text("Index visibility is not reported by this read-only preview. Search may lag; this window does not claim the note is indexed.")
                    .font(.system(size: 11)).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                if preview.kind == "project_rule" {
                    let action = preview.projectRuleAction
                    Text(verbatim: action == "retire" ? "Retire this exact prior activation" : "Activate this exact rule in the selected project")
                        .font(.system(size: 11, weight: .semibold)).fixedSize(horizontal: false, vertical: true)
                    Button(action == "retire" ? "Retire from project" : "Activate in project") { confirmationAction = action; confirmationPreview = preview }
                        .disabled(!preview.projectRuleEligible || !model.canDecide)
                } else {
                    HStack {
                        if preview.isKeepEligible { Button("Keep") { confirmationAction = "keep"; confirmationPreview = preview }.disabled(!model.canDecide) }
                        Button("Discard") { confirmationAction = "discard"; confirmationPreview = preview }.disabled(!model.canDecide)
                    }
                }
                if model.ownerStatus?.enrolled != true {
                    Button("Enable desktop approvals") { Task { await model.enableApprovals() } }.disabled(model.applying || model.isPreviewMode)
                }
                Text(verbatim: model.approvalStatus)
                    .font(.system(size: 11)).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            }
            .padding(10)
            .background(RoundedRectangle(cornerRadius: 8).fill(Color.secondary.opacity(0.08)))
        }
    }
}

extension ReviewModel {
    var canDecide: Bool { ownerStatus?.enrolled == true && ownerStatus?.readOnly == false && !applying && !previewMode }
    var isPreviewMode: Bool { previewMode }
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
