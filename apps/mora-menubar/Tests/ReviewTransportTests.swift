import Foundation

func checkTransport(_ value: @autoclosure () throws -> Bool, _ message: String) throws {
    guard try value() else { throw NSError(domain: "ReviewTransportTests", code: 1, userInfo: [NSLocalizedDescriptionKey: message]) }
}

@main enum ReviewTransportTests {
    static func main() throws {
        let body = Data(#"{"schema":"mora.review.desktop","schema_version":1}"#.utf8)
        var header = UInt32(body.count).bigEndian
        let framed = Data(bytes: &header, count: 4) + body
        try checkTransport(ReviewTransport.response(from: [framed.prefix(2), framed.dropFirst(2).prefix(3), framed.dropFirst(5)]) == body, "chunked frame changed bytes")
        do { _ = try ReviewTransport.response(from: [framed.prefix(3)]); throw NSError(domain: "ReviewTransportTests", code: 2) } catch ReviewTransportError.malformed { }
        do { _ = try ReviewTransport.response(from: [Data(repeating: 0, count: ReviewTransport.limit + 5)]); throw NSError(domain: "ReviewTransportTests", code: 3) } catch ReviewTransportError.oversized { }
        print("Review transport checks passed")
    }
}
