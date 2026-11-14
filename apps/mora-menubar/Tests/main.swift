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
check(sourceSnapshot.deviceAggregate == .unknown, "device aggregate stays independent of source health")
check(sourceSnapshot.sourceAggregate == .unknown, "unknown source health stays unknown")

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
check(staleSources.aggregate == .stale && staleSources.menuNeedsAttention, "stale source affects overall and menu status")
let failedSource = try StatusSnapshot.decode(Data(#"{"generated_at":"2026-11-08T08:00:00Z","devices":[{"id":"mac","name":"This Mac","connection":"connected","channels":[{"name":"Sync","state":"healthy"}]}],"sources":[{"key":"gmail","state":"failed"}]}"#.utf8))
check(failedSource.sourcesSummary == "1 enabled · 1 failed", "failed source is visible in compact summary")
check(failedSource.aggregate == .failed && failedSource.menuNeedsAttention, "failed source affects overall and menu status")
check(failedSource.deviceAggregate == .healthy, "source failure does not change the device aggregate")
let healthySourcesJSON = #"{"generated_at":"2026-11-08T08:00:00Z","devices":[{"id":"mac","name":"This Mac","connection":"connected","channels":[{"name":"Sync","state":"healthy"}]}],"sources":[{"key":"gmail","state":"fresh","last_success_at":"2026-11-08T07:45:00Z"}]}"#
let healthySources = try StatusSnapshot.decode(Data(healthySourcesJSON.utf8))
check(healthySources.aggregate == .healthy && !healthySources.menuNeedsAttention, "fresh source does not alarm")
check(StatusSnapshot.combine(.stale, .failed) == .failed, "failed state outranks stale")
check(StatusSnapshot.combine(.healthy, .stale) == .stale, "stale source cannot be hidden by healthy device checks")
let mixedSourceJSON = #"{"generated_at":"2026-11-08T08:00:00Z","devices":[],"sources":[{"key":"gmail","state":"running"},{"key":"calendar","state":"provider-paused"}]}"#
let mixedSources = try StatusSnapshot.decode(Data(mixedSourceJSON.utf8))
check(mixedSources.sourceAggregate == .unknown, "unknown source state outranks a running peer")
let malformedFreshJSON = #"{"generated_at":"2026-11-08T08:00:00Z","devices":[{"id":"mac","name":"This Mac","connection":"connected","channels":[{"name":"Sync","state":"healthy"}]}],"sources":[{"key":"gmail","state":"fresh","last_success_at":"not-a-timestamp"}]}"#
let malformedFresh = try StatusSnapshot.decode(Data(malformedFreshJSON.utf8))
check(malformedFresh.sources?.first?.state == "unknown", "fresh source with malformed timestamp is demoted to unknown")
check(malformedFresh.deviceAggregate == .healthy && malformedFresh.aggregate == .unknown, "malformed source timestamp cannot hide healthy device checks")

let reviewJSON = #"{"generated_at":"2026-11-08T08:00:00Z","devices":[],"review":{"saved":3,"held":2}}"#
let reviewed = try StatusSnapshot.decode(Data(reviewJSON.utf8))
check(reviewed.review?.saved == 3 && reviewed.review?.held == 2, "review counts decoded")
check(reviewed.aggregate == .unknown, "review counts do not change the aggregate")
let unreviewed = try StatusSnapshot.decode(Data(#"{"generated_at":"2026-11-08T08:00:00Z","devices":[]}"#.utf8))
check(unreviewed.review == nil, "absent review stays nil, not zero")
check(ReviewCounts(saved: 3, held: 0).rowText == "3 agent notes to review", "saved only row text")
check(ReviewCounts(saved: 0, held: 2).rowText == "2 agent notes held", "held only row text")
check(ReviewCounts(saved: 3, held: 2).rowText == "3 agent notes to review, 2 held", "saved and held row text")
check(ReviewCounts(saved: 1, held: 0).rowText == "1 agent note to review", "singular row text")
check(ReviewCounts(saved: 0, held: 1).rowText == "1 agent note held", "singular held row text")
check(ReviewCounts(saved: 0, held: 0).rowText == nil, "zero counts give no row")
check(ReviewCounts(saved: -1, held: 2).rowText == nil, "negative saved gives no row")
check(ReviewCounts(saved: 3, held: -1).rowText == nil, "negative held gives no row")

let givenJSON = #"{"generated_at":"2026-11-08T08:00:00Z","devices":[],"given":{"at":"2026-11-09T14:02:00Z","surface":"hook-recall","count":3}}"#
let givenSnap = try StatusSnapshot.decode(Data(givenJSON.utf8))
check(givenSnap.given?.count == 3 && givenSnap.given?.surface == "hook-recall", "given decoded")
check(givenSnap.aggregate == .unknown, "given does not change the aggregate")
check(unreviewed.given == nil, "absent given stays nil")
let utc = TimeZone(identifier: "UTC")!
let noon = StatusSnapshot.iso8601.date(from: "2026-11-09T20:00:00Z")!
func given(_ surface: String = "hook-recall", count: Int? = 3, at: String = "2026-11-09T14:02:00Z") -> GivenInfo {
    GivenInfo(at: at, surface: surface, count: count)
}
check(given().rowText(now: noon, timeZone: utc) == "Last hand-over to an agent: 14:02, 3 notes", "today row text")
check(given(at: "2026-11-08T14:02:00Z").rowText(now: noon, timeZone: utc) == "Last hand-over to an agent: Nov 8, 3 notes", "other day row text")
check(given(count: 1).rowText(now: noon, timeZone: utc) == "Last hand-over to an agent: 14:02, 1 note", "singular row text")
check(given(count: nil).rowText(now: noon, timeZone: utc) == "Last hand-over to an agent: 14:02", "absent count row text")
check(given(at: "bad").rowText(now: noon, timeZone: utc) == nil, "bad time gives no row")
check(given("hook-session-start").surfaceText == "At session start.", "session start surface word")
check(given("hook-recall").surfaceText == "On a prompt.", "recall surface word")
check(given("mcp:search_memory").surfaceText == "Through a tool call.", "mcp surface word")
check(given("other").surfaceText == nil, "unknown surface has no word")

let reviewListJSON = #"{"schema":"mora.review","schema_version":1,"notes":[{"id":"mem_1","kind":"saved","scope":"global","type":"insight","title":"A **title**"},{"id":"p_2","kind":"held","scope":"project:x","type":"fact","title":"Held"}]}"#
let reviewList = try JSONDecoder().decode(ReviewListReceipt.self, from: Data(reviewListJSON.utf8))
check(reviewList.isCanonical && reviewList.notes.count == 2, "canonical review queue decoded")
check(reviewList.notes[0].summary == "Saved · global · insight", "queue fields remain plain text")
let unsupportedList = try JSONDecoder().decode(ReviewListReceipt.self, from: Data(#"{"schema":"mora.review","schema_version":2,"notes":[]}"#.utf8))
check(!unsupportedList.isCanonical, "unsupported queue version is rejected")

let previewJSON = #"{"schema":"mora.review.preview","schema_version":1,"preview":{"id":"mem_1","kind":"saved","title":"A **title**","text":"Full text","scope":"global","type":"insight","source":"mcp","provenance":"authored","state":"tentative","details":[{"label":"created at","value":"now"}],"complete":false,"redacted":true,"truncated":false,"escaped":false,"omitted_arguments":true,"canonical_revision":"revision","display_digest":"digest"}}"#
let preview = try JSONDecoder().decode(ReviewPreviewReceipt.self, from: Data(previewJSON.utf8))
check(preview.isCanonical && preview.preview.id == "mem_1", "canonical full preview decoded")
check(preview.preview.incompleteness == ["redacted values", "omitted arguments"], "incomplete preview remains ineligible for Keep")
let unsupportedPreview = try JSONDecoder().decode(ReviewPreviewReceipt.self, from: Data(#"{"schema":"wrong","schema_version":1,"preview":{"id":"mem_1","kind":"saved","title":"x","text":"x","scope":"global","type":"fact","source":"mcp","provenance":"authored","state":"tentative","details":[],"complete":true,"redacted":false,"truncated":false,"escaped":false,"omitted_arguments":false,"canonical_revision":"r","display_digest":"d"}}"#.utf8))
check(!unsupportedPreview.isCanonical, "wrong preview schema is rejected")
print("Review receipt checks passed")
