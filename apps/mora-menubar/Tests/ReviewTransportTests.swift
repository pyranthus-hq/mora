import Foundation
import Darwin

func checkTransport(_ value: @autoclosure () throws -> Bool, _ message: String) throws {
    guard try value() else { throw NSError(domain: "ReviewTransportTests", code: 1, userInfo: [NSLocalizedDescriptionKey: message]) }
}

@main enum ReviewTransportTests {
    static func stub(_ root: URL, _ name: String, _ body: String) throws -> URL {
        let url = root.appendingPathComponent(name)
        try ("#!/bin/sh\n" + body).write(to: url, atomically: true, encoding: .utf8)
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: url.path)
        return url
    }

    static func main() throws {
        let body = Data(#"{"schema":"mora.review.desktop","schema_version":1}"#.utf8)
        var header = UInt32(body.count).bigEndian
        let framed = Data(bytes: &header, count: 4) + body
        try checkTransport(ReviewTransport.response(from: [framed.prefix(2), framed.dropFirst(2).prefix(3), framed.dropFirst(5)]) == body, "chunked frame changed bytes")
        do { _ = try ReviewTransport.response(from: [framed.prefix(3)]); throw NSError(domain: "ReviewTransportTests", code: 2) } catch ReviewTransportError.malformed { }
        do { _ = try ReviewTransport.response(from: [Data(repeating: 0, count: ReviewTransport.limit + 5)]); throw NSError(domain: "ReviewTransportTests", code: 3) } catch ReviewTransportError.oversized { }
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: root) }
        let request = Data("{}".utf8)
        let split = try stub(root, "split", "dd bs=1 count=4 of=/dev/null 2>/dev/null\nprintf '\\000\\000\\000\\002'\nsleep 0.02\nprintf '{}'")
        try checkTransport(try ReviewTransport.run(executable: split, arguments: [], request: request, temporaryDirectory: root) == request, "fragmented process response changed bytes")
        let truncated = try stub(root, "truncated", "cat >/dev/null\nprintf '\\000\\000\\000\\002{'" )
        do { _ = try ReviewTransport.run(executable: truncated, arguments: [], request: request, temporaryDirectory: root); throw NSError(domain: "ReviewTransportTests", code: 4) } catch ReviewTransportError.malformed { }
        let oversized = try stub(root, "oversized", "cat >/dev/null\nhead -c 1048581 /dev/zero\nsleep 2")
        do { _ = try ReviewTransport.run(executable: oversized, arguments: [], request: request, timeout: 2, temporaryDirectory: root); throw NSError(domain: "ReviewTransportTests", code: 5) } catch ReviewTransportError.oversized { }
        let pid = root.appendingPathComponent("hung.pid")
        let hung = try stub(root, "hung", "echo $$ > \"$1\"\ntrap '' TERM\nwhile :; do :; done")
        do { _ = try ReviewTransport.run(executable: hung, arguments: [pid.path], request: request, timeout: 0.2, temporaryDirectory: root); throw NSError(domain: "ReviewTransportTests", code: 6) } catch ReviewTransportError.timedOut { }
        guard let processID = Int32(String(decoding: try Data(contentsOf: pid), as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)) else {
            throw NSError(domain: "ReviewTransportTests", code: 7)
        }
        try checkTransport(kill(processID, 0) == -1, "hung child was not reaped")
        let remaining = try FileManager.default.contentsOfDirectory(atPath: root.path)
        try checkTransport(remaining.allSatisfy { $0 == "split" || $0 == "truncated" || $0 == "oversized" || $0 == "hung" || $0 == "hung.pid" }, "transport temporary directory was not cleaned")
        print("Review transport checks passed")
    }
}
