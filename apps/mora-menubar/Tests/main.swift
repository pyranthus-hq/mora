import Foundation

func check(_ condition: @autoclosure () -> Bool, _ message: String) {
    guard condition() else { fputs("FAIL: \(message)\n", stderr); exit(1) }
}

func fixture(channelState: String, connection: String = "connected") -> StatusSnapshot {
    let json = """
    {"generated_at":"2026-11-08T08:00:00Z","devices":[{"id":"mac","name":"This Mac","connection":"\(connection)","channels":[{"name":"Work shares","state":"\(channelState)","last_success":"2026-11-08T07:45:00Z"}]}]}
    """
    return try! StatusSnapshot.decode(Data(json.utf8))
}

check(fixture(channelState: "failed").aggregate == .failed, "failed channel takes priority")
check(fixture(channelState: "stale").aggregate == .stale, "stale channel remains stale")
check(fixture(channelState: "future-state").aggregate == .unknown, "unknown status is not success")
check(fixture(channelState: "healthy", connection: "future-connection").devices[0].connection == .unknown, "unknown connection is preserved as unknown")
do {
    _ = try StatusSnapshot.decode(Data("not json".utf8))
    check(false, "malformed input must throw")
} catch { }

let disconnected = try StatusSnapshot.decode(Data(#"{"generated_at":"2026-11-08T08:00:00Z","devices":[{"id":"off","name":"Offline","connection":"offline","channels":[{"name":"cached","state":"healthy"}]}]}"#.utf8))
check(disconnected.aggregate == .unknown, "offline peer cannot make the overall state healthy")

print("StatusData checks passed")
