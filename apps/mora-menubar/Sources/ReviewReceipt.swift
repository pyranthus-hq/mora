import Foundation

enum ReviewReceiptError: LocalizedError {
    case unsupportedQueue
    case unsupportedPreview
    case invalidRow
    case invalidProjectRule

    var errorDescription: String? {
        switch self {
        case .unsupportedQueue: return "Mora returned an unsupported review queue."
        case .unsupportedPreview: return "Mora returned an unsupported review preview."
        case .invalidRow: return "Mora returned an invalid review item."
        case .invalidProjectRule: return "Mora returned an invalid project rule receipt."
        }
    }
}

struct ProjectRuleProject: Decodable, Identifiable, Equatable {
    let root: String
    let scope: String
    var id: String { root }
    var isSafe: Bool { !root.isEmpty && !scope.isEmpty && !root.contains("\u{0}") && !scope.contains("\u{0}") }
}

struct ProjectRuleRow: Decodable, Identifiable, Equatable {
    let id: String
    let kind: String
    let scope: String
    let type: String?
    let title: String
    var isSafe: Bool {
        kind == "project_rule" && !id.hasPrefix("-") && !id.isEmpty &&
            id.unicodeScalars.allSatisfy { CharacterSet.alphanumerics.union(CharacterSet(charactersIn: "._-")).contains($0) }
    }
}

struct ProjectRuleListReceipt: Decodable {
    let schema: String
    let schemaVersion: Int
    let projects: [ProjectRuleProject]
    let rules: [ProjectRuleRow]
    enum CodingKeys: String, CodingKey { case schema, projects, rules; case schemaVersion = "schema_version" }

    func validated() throws -> ProjectRuleListReceipt {
        guard schema == "mora.review.desktop", schemaVersion == 1,
              projects.allSatisfy(\.isSafe), rules.allSatisfy(\.isSafe) else { throw ReviewReceiptError.invalidProjectRule }
        return self
    }
}

struct ReviewListReceipt: Decodable {
    let schema: String
    let schemaVersion: Int
    let notes: [ReviewRow]
    enum CodingKeys: String, CodingKey { case schema, schemaVersion = "schema_version", notes }

    func validated() throws -> [ReviewRow] {
        guard schema == "mora.review", schemaVersion == 1 else { throw ReviewReceiptError.unsupportedQueue }
        guard notes.allSatisfy(\.isSafe) else { throw ReviewReceiptError.invalidRow }
        return notes
    }
}

struct ReviewRow: Decodable, Identifiable, Equatable {
    let id: String
    let kind: String
    let scope: String
    let type: String
    let title: String
    var summary: String { "\(kind.capitalized) · \(scope) · \(type)" }
    var isSafe: Bool {
        (kind == "saved" || kind == "held") && !id.hasPrefix("-") &&
            !id.isEmpty && id.unicodeScalars.allSatisfy { CharacterSet.alphanumerics.union(CharacterSet(charactersIn: "._-")).contains($0) }
    }
}

struct ReviewPreviewReceipt: Decodable {
    let schema: String
    let schemaVersion: Int
    let preview: ReviewPreview
    enum CodingKeys: String, CodingKey { case schema, schemaVersion = "schema_version", preview }

    func validated(for row: ReviewRow) throws -> ReviewPreview {
        guard schema == "mora.review.preview", schemaVersion == 1, row.isSafe,
              preview.id == row.id, preview.kind == row.kind else { throw ReviewReceiptError.unsupportedPreview }
        return preview
    }
}

struct ReviewPreview: Decodable, Identifiable {
    struct Detail: Decodable, Identifiable {
        let label: String
        let value: String
        var id: String { label + "\u{0}" + value }
    }
    let id: String
    let kind: String
    let title: String
    let text: String
    let scope: String
    let type: String
    let source: String
    let provenance: String
    let state: String
    let details: [Detail]
    let complete: Bool
    let redacted: Bool
    let truncated: Bool
    let escaped: Bool
    let omittedArguments: Bool
    let canonicalRevision: String
    let displayDigest: String

    enum CodingKeys: String, CodingKey {
        case id, kind, title, text, scope, type, source, provenance, state, details, complete, redacted, truncated, escaped
        case omittedArguments = "omitted_arguments"
        case canonicalRevision = "canonical_revision"
        case displayDigest = "display_digest"
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id); kind = try c.decode(String.self, forKey: .kind)
        title = try c.decode(String.self, forKey: .title); text = try c.decode(String.self, forKey: .text)
        scope = try c.decode(String.self, forKey: .scope); type = try c.decode(String.self, forKey: .type)
        source = try c.decode(String.self, forKey: .source); provenance = try c.decode(String.self, forKey: .provenance)
        state = try c.decode(String.self, forKey: .state); details = try c.decodeIfPresent([Detail].self, forKey: .details) ?? []
        complete = try c.decode(Bool.self, forKey: .complete); redacted = try c.decode(Bool.self, forKey: .redacted)
        truncated = try c.decode(Bool.self, forKey: .truncated); escaped = try c.decode(Bool.self, forKey: .escaped)
        omittedArguments = try c.decode(Bool.self, forKey: .omittedArguments)
        canonicalRevision = try c.decode(String.self, forKey: .canonicalRevision); displayDigest = try c.decode(String.self, forKey: .displayDigest)
    }

    var isKeepEligible: Bool { complete && !redacted && !truncated && !escaped && !omittedArguments }
    var incompleteness: [String] {
        var result = [redacted ? "redacted values" : nil, truncated ? "cut text" : nil, escaped ? "escaped control characters" : nil, omittedArguments ? "omitted arguments" : nil].compactMap { $0 }
        if !complete && result.isEmpty { result.append("Mora marked this preview incomplete") }
        return result
    }
}

extension ReviewPreview {
    var projectRuleAction: String? {
        let actions = details.filter { $0.label == "action" }.map(\.value)
        guard actions.count == 1, actions[0] == "activate" || actions[0] == "retire" else { return nil }
        return actions[0]
    }

    var projectRuleEligible: Bool { kind == "project_rule" && isKeepEligible && projectRuleAction != nil }
}

struct ReviewSelectionState {
    private(set) var generation = 0
    private(set) var selectionGeneration = 0
    private(set) var rows: [ReviewRow] = []
    private(set) var selectedID: String?

    mutating func beginRefresh() -> Int {
        generation += 1; selectionGeneration += 1; rows = []; selectedID = nil; return generation
    }
    mutating func acceptQueue(_ rows: [ReviewRow], generation: Int) -> Bool {
        guard generation == self.generation else { return false }
        self.rows = rows; return true
    }
    mutating func beginSelection(_ row: ReviewRow) -> (queue: Int, selection: Int)? {
        guard rows.contains(row) else { return nil }
        selectionGeneration += 1; selectedID = row.id; return (generation, selectionGeneration)
    }
    func acceptsPreview(_ preview: ReviewPreview, row: ReviewRow, queue: Int, selection: Int) -> Bool {
        queue == generation && selection == selectionGeneration && selectedID == row.id && rows.contains(row) && preview.id == row.id && preview.kind == row.kind
    }
}

struct ProjectRuleSelectionState {
    private(set) var generation = 0
    private(set) var selectionGeneration = 0
    private(set) var root: String?
    private(set) var rows: [ProjectRuleRow] = []
    private(set) var selectedID: String?
    private(set) var action: String?

    mutating func beginRefresh(root: String?) -> Int {
        generation += 1; selectionGeneration += 1; self.root = root; rows = []; selectedID = nil; action = nil; return generation
    }
    mutating func accept(_ rows: [ProjectRuleRow], root: String?, generation: Int) -> Bool {
        guard generation == self.generation, root == self.root else { return false }
        self.rows = rows; return true
    }
    mutating func beginPreview(_ row: ProjectRuleRow, root: String, action: String) -> (queue: Int, selection: Int)? {
        guard self.root == root, rows.contains(row), action == "activate" || action == "retire" else { return nil }
        selectionGeneration += 1; selectedID = row.id; self.action = action; return (generation, selectionGeneration)
    }
    func accepts(_ preview: ReviewPreview, row: ProjectRuleRow, root: String, action: String, queue: Int, selection: Int) -> Bool {
        queue == generation && selection == selectionGeneration && self.root == root && selectedID == row.id && self.action == action && rows.contains(row) && preview.id == row.id && preview.kind == "project_rule" && preview.projectRuleAction == action
    }
}
