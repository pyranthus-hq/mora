import Foundation

func check(_ condition: @autoclosure () -> Bool, _ message: String) {
    guard condition() else { fputs("FAIL: \(message)\n", stderr); exit(1) }
}

func fixture(channelState: String, connection: String = "connected", sourceFields: String = "\"sources\":[]") -> StatusSnapshot {
    let json = """
    {"generated_at":"2026-11-08T08:00:00Z","devices":[{"id":"mac","name":"This Mac","connection":"\(connection)","channels":[{"name":"Work shares","state":"\(channelState)","last_success":"2026-11-08T07:45:00Z"}]}],\(sourceFields)}
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
check(fixture(channelState: "healthy").aggregate == .healthy, "empty connector list is valid")
for (raw, expected) in [("fresh", ChannelState.healthy), ("failed", .failed), ("stale", .stale), ("future-state", .unknown)] {
    let snapshot = fixture(channelState: "healthy", sourceFields: "\"sources\":[{\"key\":\"gmail:pyranthus\",\"state\":\"\(raw)\",\"last_success_at\":\"2026-11-08T07:45:00.123Z\",\"detail\":\"Account check\"}]")
    check(snapshot.aggregate == expected, "connector \(raw) affects aggregate")
    check(snapshot.sources?.first?.lastSuccess != nil, "fractional source timestamp parses")
    check(snapshot.sources?.first?.detail == "Account check", "connector detail preserved")
}
check(fixture(channelState: "healthy", sourceFields: "\"sources_error\":\"Doctor unavailable\"").aggregate == .unknown, "missing source data is unknown")
check(fixture(channelState: "healthy", sourceFields: "\"sources\":[],\"sources_error\":\"Doctor unavailable\"").aggregate == .unknown, "source collection failure is unknown")
check(fixture(channelState: "failed", sourceFields: "\"sources_error\":\"Doctor unavailable\"").aggregate == .failed, "known failure outranks source collection error")
check(fixture(channelState: "healthy", sourceFields: "\"sources\":[],\"sources_error\":\"\"").aggregate == .healthy, "empty source error is ignored")
do {
    _ = try StatusSnapshot.decode(Data(#"{"generated_at":"2026-11-08T08:00:00Z","devices":[],"sources":[{"key":"gmail"}]}"#.utf8))
    check(false, "malformed source schema must fail explicitly")
} catch { }
print("StatusData checks passed")
