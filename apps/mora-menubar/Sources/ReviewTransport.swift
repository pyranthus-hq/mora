import Foundation
import Darwin

enum ReviewTransportError: Error { case malformed, oversized, timedOut, unavailable }

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

    static func run(executable: URL, arguments: [String], request: Data, timeout: TimeInterval = 30, temporaryDirectory: URL = FileManager.default.temporaryDirectory) throws -> Data {
        guard request.count <= limit else { throw ReviewTransportError.oversized }
        let directory = temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        defer { try? FileManager.default.removeItem(at: directory) }
        let inputURL = directory.appendingPathComponent("request")
        let outputURL = directory.appendingPathComponent("response")
        var length = UInt32(request.count).bigEndian
        try (Data(bytes: &length, count: 4) + request).write(to: inputURL, options: .atomic)
        FileManager.default.createFile(atPath: outputURL.path, contents: nil, attributes: [.posixPermissions: 0o600])
        let input = try FileHandle(forReadingFrom: inputURL)
        let output = try FileHandle(forWritingTo: outputURL)
        defer { try? input.close(); try? output.close() }

        let process = Process()
        process.executableURL = executable
        process.arguments = arguments
        process.standardInput = input
        process.standardOutput = output
        process.standardError = FileHandle.nullDevice
        try process.run()
        let deadline = Date().addingTimeInterval(timeout)
        func stop() {
            if process.isRunning {
                process.terminate()
                let grace = Date().addingTimeInterval(1)
                while process.isRunning && Date() < grace { Thread.sleep(forTimeInterval: 0.01) }
                if process.isRunning { _ = kill(process.processIdentifier, SIGKILL) }
                while process.isRunning { Thread.sleep(forTimeInterval: 0.01) }
            }
        }
        while process.isRunning {
            let attributes = try? FileManager.default.attributesOfItem(atPath: outputURL.path)
            let size = (attributes?[.size] as? NSNumber)?.intValue ?? 0
            if size > limit + 4 { stop(); throw ReviewTransportError.oversized }
            if Date() >= deadline { stop(); throw ReviewTransportError.timedOut }
            Thread.sleep(forTimeInterval: 0.01)
        }
        try? output.close()
        let bytes = try Data(contentsOf: outputURL, options: .mappedIfSafe)
        guard bytes.count <= limit + 4 else { throw ReviewTransportError.oversized }
        guard process.terminationStatus == 0 else { throw ReviewTransportError.unavailable }
        return try response(from: [bytes])
    }
}
