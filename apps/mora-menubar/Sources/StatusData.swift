import Foundation

struct StatusSnapshot: Decodable {
    let generatedAt: Date?
    let devices: [DeviceStatus]
    let sources: [SourceStatus]?
    let sourcesError: String?
    let review: ReviewCounts?

    enum CodingKeys: String, CodingKey { case generatedAt = "generated_at", devices, sources, sourcesError = "sources_error", review }
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
