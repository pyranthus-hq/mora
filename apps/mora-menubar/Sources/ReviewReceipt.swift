import Foundation

struct ReviewListReceipt: Decodable {
    let schema: String
    let schemaVersion: Int
    let notes: [ReviewRow]
    enum CodingKeys: String, CodingKey { case schema, schemaVersion = "schema_version", notes }
    var isCanonical: Bool { schema == "mora.review" && schemaVersion == 1 }
}

struct ReviewRow: Decodable, Identifiable {
    let id: String
    let kind: String
    let scope: String
    let type: String
    let title: String
    var summary: String { "\(kind.capitalized) · \(scope) · \(type)" }
}

struct ReviewPreviewReceipt: Decodable {
    let schema: String
    let schemaVersion: Int
    let preview: ReviewPreview
    enum CodingKeys: String, CodingKey { case schema, schemaVersion = "schema_version", preview }
    var isCanonical: Bool { schema == "mora.review.preview" && schemaVersion == 1 }
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
    var incompleteness: [String] {
        [redacted ? "redacted values" : nil, truncated ? "cut text" : nil, escaped ? "escaped control characters" : nil, omittedArguments ? "omitted arguments" : nil].compactMap { $0 }
    }
}
