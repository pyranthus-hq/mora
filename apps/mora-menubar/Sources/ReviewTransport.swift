import Foundation

enum ReviewTransportError: Error { case malformed, oversized }

enum ReviewTransport {
    static let limit = 1_048_576

    static func response(from chunks: [Data]) throws -> Data {
        var bytes = Data()
        for chunk in chunks {
            guard bytes.count + chunk.count <= limit + 4 else { throw ReviewTransportError.oversized }
            bytes.append(chunk)
        }
        guard bytes.count >= 4 else { throw ReviewTransportError.malformed }
        let length = bytes.prefix(4).reduce(UInt32(0)) { ($0 << 8) | UInt32($1) }
        guard length > 0, length <= limit, bytes.count == Int(length) + 4 else { throw ReviewTransportError.malformed }
        return bytes.dropFirst(4)
    }
}
