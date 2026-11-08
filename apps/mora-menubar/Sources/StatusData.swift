import Foundation

struct StatusSnapshot: Decodable {
    let generatedAt: Date?
    let devices: [DeviceStatus]
    let sources: [SourceStatus]?
    let sourcesError: String?

    enum CodingKeys: String, CodingKey { case generatedAt = "generated_at", devices, sources, sourcesError = "sources_error" }
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
        sources = try c.decodeIfPresent([SourceStatus].self, forKey: .sources)
        let error = try c.decodeIfPresent(String.self, forKey: .sourcesError)
        sourcesError = error?.isEmpty == true ? nil : error
    }

    static func decode(_ data: Data) throws -> StatusSnapshot {
        let result = try JSONDecoder().decode(StatusSnapshot.self, from: data)
        guard result.generatedAt != nil else { throw StatusError.malformed("Invalid generated_at timestamp") }
        return result
    }

    var aggregate: ChannelState {
        var states = devices.flatMap(\.channels).map(\.state) + (sources ?? []).map(\.state)
        if devices.isEmpty || devices.flatMap(\.channels).isEmpty || sources == nil || sourcesError != nil {
            states.append(.unknown)
        }
        guard !states.isEmpty else { return .unknown }
        if states.contains(.failed) { return .failed }
        if states.contains(.stale) { return .stale }
        if states.contains(.unknown) { return .unknown }
        if states.contains(.running) { return .running }
        return .healthy
    }
}

struct SourceStatus: Decodable, Identifiable {
    let key: String
    let rawState: String
    let detail: String?
    let lastSuccessAt: String?
    var id: String { key }
    var state: ChannelState {
        switch rawState {
        case "fresh": .healthy
        case "failed": .failed
        case "stale": .stale
        case "running": .running
        default: .unknown
        }
    }
    var lastSuccess: Date? { lastSuccessAt.flatMap(StatusSnapshot.date) }
    enum CodingKeys: String, CodingKey {
        case key, rawState = "state", detail, lastSuccessAt = "last_success_at"
    }
}

struct DeviceStatus: Decodable, Identifiable {
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
