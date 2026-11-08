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

check(disconnected.devices[0].state == .unknown, "offline device cannot have a healthy summary")
check(fixture(channelState: "stale").devices[0].state == .stale, "collapsed device preserves overdue sync")
check(fixture(channelState: "failed").devices[0].state == .failed, "collapsed device preserves failed sync")
check(fixture(channelState: "unknown").devices[0].state == .unknown, "missing receipts stay incomplete")

let sourceFixtureJSON = #"{"generated_at":"2026-11-08T08:00:00Z","devices":[{"id":"mac","name":"This Mac","connection":"connected","channels":[]}],"sources":[{"key":"gmail:work","state":"fresh","last_success_at":"2026-11-08T07:45:00Z","detail":"last read succeeded"},{"key":"calendar","state":"provider-paused"}]}"#
let sourceSnapshot = try StatusSnapshot.decode(Data(sourceFixtureJSON.utf8))
check(sourceSnapshot.sources?.count == 2, "enabled source list decoded")
check(sourceSnapshot.sources?[0].lastSuccessAt != nil, "source timestamp decoded")
check(sourceSnapshot.sources?[1].state == "provider-paused", "unknown source state is preserved")
check(sourceSnapshot.sourcesSummary == "2 enabled · 1 fresh · 1 unknown", "source summary stays compact and honest")
check(sourceSnapshot.aggregate == .unknown, "source health does not override device health")

let failedSourcesJSON = #"{"generated_at":"2026-11-08T08:00:00Z","devices":[{"id":"mac","name":"This Mac","connection":"connected","channels":[]}],"sources":[],"sources_error":"Mora doctor is unavailable."}"#
let failedSources = try StatusSnapshot.decode(Data(failedSourcesJSON.utf8))
check(failedSources.sourcesSummary == "Unavailable", "probe error outranks an empty source array")
let emptySourcesJSON = #"{"generated_at":"2026-11-08T08:00:00Z","devices":[{"id":"mac","name":"This Mac","connection":"connected","channels":[]}],"sources":[]}"#
let emptySources = try StatusSnapshot.decode(Data(emptySourcesJSON.utf8))
check(emptySources.sourcesSummary == "No enabled sources", "empty enabled-source list is not success")
let malformedSources = try StatusSnapshot.decode(Data(#"{"generated_at":"2026-11-08T08:00:00Z","devices":[{"id":"mac","name":"This Mac","connection":"connected","channels":[]}],"sources":[{"key":"gmail"}]}"#.utf8))
check(malformedSources.devices[0].connection == .connected, "malformed source health does not discard device status")
check(malformedSources.sourcesSummary == "Unavailable", "malformed source health is shown unavailable")
let staleSources = try StatusSnapshot.decode(Data(#"{"generated_at":"2026-11-08T08:00:00Z","devices":[],"sources":[{"key":"gmail","state":"stale"}]}"#.utf8))
check(staleSources.sourcesSummary == "1 enabled · 1 stale", "stale source is visible in compact summary")
let failedSource = try StatusSnapshot.decode(Data(#"{"generated_at":"2026-11-08T08:00:00Z","devices":[],"sources":[{"key":"gmail","state":"failed"}]}"#.utf8))
check(failedSource.sourcesSummary == "1 enabled · 1 failed", "failed source is visible in compact summary")
