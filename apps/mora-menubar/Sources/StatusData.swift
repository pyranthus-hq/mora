import Foundation

struct StatusSnapshot: Decodable {
    let generatedAt: Date?
    let devices: [DeviceStatus]
    let sources: [SourceStatus]?
    let sourcesError: String?
    let review: ReviewCounts?
    let given: GivenInfo?

    enum CodingKeys: String, CodingKey { case generatedAt = "generated_at", devices, sources, sourcesError = "sources_error", review, given }
    static let iso8601 = ISO8601DateFormatter()
    static let fractionalISO8601: ISO8601DateFormatter = {
        let f = ISO8601DateFormatter(); f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]; return f
    }()
    static func date(_ string: String) -> Date? { iso8601.date(from: string) ?? fractionalISO8601.date(from: string) }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        let value = try c.decode(String.self, forKey: .generatedAt)
        generatedAt = Self.date(value)
        devices = try c.decode([DeviceStatus].self, forKey: .devices)
        do { sources = try c.decodeIfPresent([SourceStatus].self, forKey: .sources) }
        catch { sources = nil }
        review = try? c.decodeIfPresent(ReviewCounts.self, forKey: .review)
        given = try? c.decodeIfPresent(GivenInfo.self, forKey: .given)
        let reportedError = try c.decodeIfPresent(String.self, forKey: .sourcesError)
        sourcesError = reportedError ?? (sources == nil ? "Connector source health is unavailable." : nil)
    }

    static func decode(_ data: Data) throws -> StatusSnapshot {
        let result = try JSONDecoder().decode(StatusSnapshot.self, from: data)
        guard result.generatedAt != nil else { throw StatusError.malformed("Invalid generated_at timestamp") }
        return result
    }

    var deviceAggregate: ChannelState { Self.aggregate(devices: devices) }
    var aggregate: ChannelState { Self.combine(deviceAggregate, sourceAggregate) }

    var sourceAggregate: ChannelState {
        guard sourcesError == nil, let sources, !sources.isEmpty else { return .unknown }
        if sources.contains(where: { $0.normalizedState == "failed" }) { return .failed }
        if sources.contains(where: { $0.normalizedState == "stale" }) { return .stale }
        if sources.contains(where: { !["fresh", "running"].contains($0.normalizedState) }) { return .unknown }
        if sources.contains(where: { $0.normalizedState == "running" }) { return .running }
        return .healthy
    }

    static func combine(_ first: ChannelState, _ second: ChannelState) -> ChannelState {
        for state in [ChannelState.failed, .stale, .unknown, .running] where first == state || second == state {
            return state
        }
        return .healthy
    }

    var menuNeedsAttention: Bool { [.failed, .stale, .unknown].contains(aggregate) }

    static func aggregate(devices: [DeviceStatus]) -> ChannelState {
        let channels = devices.flatMap(\.channels)
        guard !devices.isEmpty, !channels.isEmpty else { return .unknown }
        if channels.contains(where: { $0.state == .failed }) { return .failed }
        if channels.contains(where: { $0.state == .stale }) { return .stale }
        if channels.contains(where: { $0.state == .unknown }) { return .unknown }
        if devices.contains(where: { $0.connection != .connected || $0.channels.isEmpty }) { return .unknown }
        if channels.contains(where: { $0.state == .running }) { return .running }
        return .healthy
    }

    var sourcesSummary: String {
        guard sourcesError == nil, let sources else { return "Unavailable" }
        guard !sources.isEmpty else { return "No enabled sources" }
        let fresh = sources.filter { $0.normalizedState == "fresh" }.count
        let stale = sources.filter { $0.normalizedState == "stale" }.count
        let failed = sources.filter { $0.normalizedState == "failed" }.count
        let never = sources.filter { $0.normalizedState == "never" }.count
        let running = sources.filter { $0.normalizedState == "running" }.count
        var parts = ["\(sources.count) enabled"]
        if fresh > 0 { parts.append("\(fresh) fresh") }
        if stale > 0 { parts.append("\(stale) stale") }
        if failed > 0 { parts.append("\(failed) failed") }
        if never > 0 { parts.append("\(never) never synced") }
        if running > 0 { parts.append("\(running) running") }
        let known = fresh + stale + failed + never + running
        if known < sources.count { parts.append("\(sources.count - known) unknown") }
        return parts.joined(separator: " · ")
    }
}

struct ReviewCounts: Decodable {
    let saved: Int
    let held: Int

    /// One panel row for unreviewed agent notes; nil when there is nothing to review.
    var rowText: String? {
        func notes(_ n: Int) -> String { n == 1 ? "1 agent note" : "\(n) agent notes" }
        guard saved >= 0, held >= 0 else { return nil }
        switch (saved > 0, held > 0) {
        case (true, true): return "\(notes(saved)) to review, \(held) held"
        case (true, false): return "\(notes(saved)) to review"
        case (false, true): return "\(notes(held)) held"
        default: return nil
        }
    }
}

/// One agent note in the review window. All text is untrusted: show it as plain text only.
struct ReviewNote: Decodable, Identifiable, Equatable {
    let id: String
    let kind: String
    let scope: String
    let type: String
    let title: String
    let text: String
    let redacted: Bool
    let truncated: Bool
    let hiddenChars: Int
    let unreadable: Bool

    enum CodingKeys: String, CodingKey { case id, kind, scope, type, title, text, redacted, truncated, hiddenChars = "hidden_chars", unreadable }
    init(id: String, kind: String, scope: String, type: String, title: String, text: String, redacted: Bool = false, truncated: Bool = false, hiddenChars: Int = 0, unreadable: Bool = false) {
        self.id = id; self.kind = kind; self.scope = scope; self.type = type; self.title = title; self.text = text
        self.redacted = redacted; self.truncated = truncated; self.hiddenChars = hiddenChars; self.unreadable = unreadable
    }
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        self.init(id: try c.decode(String.self, forKey: .id), kind: try c.decode(String.self, forKey: .kind),
                  scope: try c.decode(String.self, forKey: .scope), type: try c.decode(String.self, forKey: .type),
                  title: try c.decode(String.self, forKey: .title), text: try c.decode(String.self, forKey: .text),
                  redacted: try c.decodeIfPresent(Bool.self, forKey: .redacted) ?? false,
                  truncated: try c.decodeIfPresent(Bool.self, forKey: .truncated) ?? false,
                  hiddenChars: max(0, try c.decodeIfPresent(Int.self, forKey: .hiddenChars) ?? 0),
                  unreadable: try c.decodeIfPresent(Bool.self, forKey: .unreadable) ?? false)
    }

    /// Keep is allowed only when the window shows the whole text that would be saved. Discard is always allowed.
    var canKeep: Bool { !redacted && !truncated && !unreadable }

    /// Why Keep is missing; nil when Keep is allowed.
    var keepWarning: String? {
        if redacted { return "This note holds a value that looks like a credential. Mora hides it here. Discard it, or look at it with mora review in a terminal." }
        if truncated { return "Text cut: \(hiddenChars) more characters are not shown. Discard it, or look at it with mora review in a terminal." }
        if unreadable { return "The text could not be read. Discard it, or look at it with mora review in a terminal." }
        return nil
    }

    /// "Saved, not reviewed · global · insight"
    var kindLine: String {
        let kindText: String
        switch kind {
        case "saved": kindText = "Saved, not reviewed"
        case "held": kindText = "Held, not saved yet"
        default: kindText = "Unknown kind"
        }
        return [kindText, scope, type].joined(separator: " · ")
    }
}

/// The collector's review-list answer: notes, a count of notes left out, or one error.
struct ReviewList: Decodable {
    let notes: [ReviewNote]
    let more: Int
    let error: String?

    enum CodingKeys: String, CodingKey { case notes, more, error }
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        error = try c.decodeIfPresent(String.self, forKey: .error)
        notes = try c.decodeIfPresent([ReviewNote].self, forKey: .notes) ?? []
        more = max(0, try c.decodeIfPresent(Int.self, forKey: .more) ?? 0)
    }

    static func moreLine(_ more: Int) -> String? { more > 0 ? "\(more) more not shown. Review these first, then refresh." : nil }
    var moreLine: String? { Self.moreLine(more) }
}

/// Decides which fetch results and clicks the window accepts, so a late fetch
/// cannot bring back a card after an action removed it.
struct ReviewGate {
    private(set) var generation = 0
    private(set) var fetching = false
    private(set) var acting = 0

    var canRefresh: Bool { !fetching && acting == 0 }
    var canAct: Bool { !fetching }

    /// The fetch's number, or nil when a fetch or an action is already running.
    mutating func beginFetch() -> Int? {
        guard canRefresh else { return nil }
        fetching = true
        return generation
    }
    /// True when the result is current. False means drop it and fetch again.
    mutating func finishFetch(_ number: Int) -> Bool {
        fetching = false
        return number == generation
    }
    mutating func beginAction() -> Bool {
        guard canAct else { return false }
        acting += 1
        return true
    }
    /// A successful action makes every fetch started before it stale.
    mutating func endAction(succeeded: Bool) {
        acting = max(0, acting - 1)
        if succeeded { generation += 1 }
    }
}

/// The collector's review-act answer.
struct ReviewActResult: Decodable {
    let ok: Bool
    let error: String?
}

struct GivenInfo: Decodable {
    let at: String
    let surface: String
    let count: Int?

    /// "Last hand-over to an agent: 14:02, 3 notes"; nil when the time cannot be read.
    func rowText(now: Date, timeZone: TimeZone) -> String? {
        guard let date = StatusSnapshot.iso8601.date(from: at) ?? StatusSnapshot.fractionalISO8601.date(from: at) else { return nil }
        var calendar = Calendar(identifier: .gregorian); calendar.timeZone = timeZone
        let f = DateFormatter()
        f.locale = Locale(identifier: "en_US_POSIX"); f.timeZone = timeZone
        f.dateFormat = calendar.isDate(date, inSameDayAs: now) ? "HH:mm" : "MMM d"
        var text = "Last hand-over to an agent: \(f.string(from: date))"
        if let n = count, n >= 0 { text += n == 1 ? ", 1 note" : ", \(n) notes" }
        return text
    }

    /// Plain words for the surface; nil for an unknown surface.
    var surfaceText: String? {
        switch surface {
        case "hook-session-start": return "At session start."
        case "hook-recall": return "On a prompt."
        default: return surface.hasPrefix("mcp:") ? "Through a tool call." : nil
        }
    }
}

struct SourceStatus: Decodable, Identifiable {
    let key: String
    let state: String
    let lastSuccessAt: Date?
    let detail: String?
    var id: String { key }

    enum CodingKeys: String, CodingKey { case key, state, lastSuccessAt = "last_success_at", detail }
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        key = try c.decode(String.self, forKey: .key)
        let rawState = try c.decode(String.self, forKey: .state)
        if let text = try c.decodeIfPresent(String.self, forKey: .lastSuccessAt) {
            lastSuccessAt = StatusSnapshot.date(text)
        } else { lastSuccessAt = nil }
        state = rawState.lowercased() == "fresh" && lastSuccessAt == nil ? "unknown" : rawState
        detail = try c.decodeIfPresent(String.self, forKey: .detail)
    }

    var normalizedState: String { state.lowercased() }
    var label: String {
        switch normalizedState {
        case "fresh": "Fresh"
        case "stale": "Stale"
        case "failed": "Failed"
        case "never": "Never synced"
        default: state.replacingOccurrences(of: "_", with: " ").capitalized
        }
    }
    var symbol: String {
        switch normalizedState {
        case "fresh": "checkmark.circle.fill"
        case "stale": "clock.fill"
        case "failed": "xmark.circle.fill"
        default: "questionmark.circle.fill"
        }
    }
}

struct DeviceStatus: Decodable, Identifiable {
    var state: ChannelState { StatusSnapshot.aggregate(devices: [self]) }
    let id: String
    let name: String
    let connection: ConnectionState
    let detail: String?
    let channels: [ChannelStatus]
}

struct ChannelStatus: Decodable, Identifiable {
    let name: String
    let state: ChannelState
    let detail: String?
    let lastSuccess: Date?
    var id: String { name }

    enum CodingKeys: String, CodingKey { case name, state, detail, lastSuccess = "last_success" }
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        name = try c.decode(String.self, forKey: .name)
        state = ChannelState(rawValue: try c.decode(String.self, forKey: .state)) ?? .unknown
        detail = try c.decodeIfPresent(String.self, forKey: .detail)
        if let text = try c.decodeIfPresent(String.self, forKey: .lastSuccess), text != "null" {
            lastSuccess = StatusSnapshot.date(text)
        } else { lastSuccess = nil }
    }
}

enum ConnectionState: String, Decodable {
    case connected, offline, unknown
    init(from decoder: Decoder) throws {
        let value = try decoder.singleValueContainer().decode(String.self)
        self = ConnectionState(rawValue: value) ?? .unknown
    }
}

enum ChannelState: String, Decodable {
    case healthy, failed, stale, unknown, running
    var summary: String {
        switch self {
        case .healthy: "Checks passed"
        case .failed: "Needs attention"
        case .stale: "Not fully current"
        case .unknown: "Checks incomplete"
        case .running: "In progress"
        }
    }
    var label: String {
        switch self {
        case .healthy: "Healthy"
        case .failed: "Failed"
        case .stale: "Stale"
        case .unknown: "Unknown"
        case .running: "Running"
        }
    }
    var symbol: String {
        switch self {
        case .healthy: "checkmark.circle.fill"
        case .failed: "xmark.circle.fill"
        case .stale: "clock.fill"
        case .unknown: "questionmark.circle.fill"
        case .running: "arrow.triangle.2.circlepath"
        }
    }
}

enum StatusError: LocalizedError {
    case malformed(String)
    case collector(String)
    var errorDescription: String? {
        switch self { case .malformed(let message), .collector(let message): message }
    }
}
