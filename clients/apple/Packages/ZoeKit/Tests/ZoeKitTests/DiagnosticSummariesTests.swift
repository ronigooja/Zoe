import XCTest
@testable import ZoeKit
@testable import ZoeUI

private final class CensusClock {
    var date = Date(timeIntervalSince1970: 0)
    func now() -> Date { date }
    func advance(_ seconds: Double) { date = date.addingTimeInterval(seconds) }
}

final class DiagnosticSummariesTests: XCTestCase {
    func testCompleteTypeReportsAreBoundedSortedAndNeverSplitEntries() {
        var census = EventCensusDiagnostics(completeTypes: true)
        XCTAssertEqual(census.typeReports(), [])
        var expected: [String] = []
        for index in 0..<110 {
            let type = String(format: "event_%03d_", index) + String(repeating: "x", count: 54)
            census.note(type: type, activity: index == 0)
            expected.append(type + ":1:" + (index == 0 ? "act=1" : "ignored"))
        }
        census.note(type: "session.output_audio.delta")
        census.note(type: "session.output_audio.delta")
        expected.insert("session.output_audio.delta:2:ignored", at: 0)
        let reports = census.typeReports()
        XCTAssertGreaterThan(reports.count, 10)
        var actual: [String] = []
        for (index, report) in reports.enumerated() {
            XCTAssertLessThanOrEqual(report.count, 200)
            let prefix = "part=\(index + 1)/\(reports.count);types="
            XCTAssertTrue(report.hasPrefix(prefix))
            actual += report.dropFirst(prefix.count).components(separatedBy: "|")
        }
        XCTAssertEqual(actual, expected)
        var long = EventCensusDiagnostics(completeTypes: true)
        long.note(type: String(repeating: "a", count: 80))
        XCTAssertEqual(long.typeReports(), ["part=1/1;types=" + String(repeating: "a", count: 64) + ":1:ignored"])
    }

    @MainActor
    func testVoiceTypeReportsRepeatEveryMinuteWithCumulativeCounts() async throws {
        let clock = CensusClock()
        let name = "DiagnosticSummariesTests-\(UUID().uuidString)"
        let defaults = UserDefaults(suiteName: name)!
        defer { defaults.removePersistentDomain(forName: name) }
        let model = ZoeAppModel(defaults: defaults, diagnosticNow: clock.now)
        let api = try APIClient(serverURL: URL(string: "https://census.example")!)
        let session = try voiceDialFixture("census-test", generation: 1)
        var epoch = model.installVoiceSessionForTesting(session, api: api)
        var traces: [(String, String)] = []
        model.voiceDiagnosticObserverForTesting = { traces.append(($0, $1)) }
        func receive(_ type: String) async throws {
            let inner = try JSONSerialization.data(withJSONObject: ["type": type])
            let wrapped = try JSONSerialization.data(withJSONObject: ["type": "data_message", "data": String(decoding: inner, as: UTF8.self)])
            await model.receiveVoiceDataForTesting(wrapped, mediaEpoch: epoch)
        }
        try await receive("session.output_audio.delta")
        clock.advance(19)
        model.reportVoiceCensusForTesting()
        XCTAssertTrue(traces.isEmpty)
        clock.advance(1)
        model.reportVoiceCensusForTesting()
        XCTAssertEqual(traces.filter { $0.0 == "voice_event_census_early" }.count, 1)
        XCTAssertEqual(traces.filter { $0.0 == "voice_event_types" }.map(\.1), ["part=1/1;types=session.output_audio.delta:1:ignored"])
        traces.removeAll()
        try await receive("session.output_audio.delta")
        try await receive("turn.delta")
        clock.advance(39)
        model.reportVoiceCensusForTesting()
        XCTAssertTrue(traces.isEmpty)
        clock.advance(1)
        model.reportVoiceCensusForTesting()
        XCTAssertEqual(traces.map(\.0), ["voice_event_types"])
        XCTAssertEqual(traces.map(\.1), ["part=1/1;types=session.output_audio.delta:2:ignored|turn.delta:1:ignored"])
        traces.removeAll()
        model.reportVoiceCensusForTesting()
        clock.advance(59)
        model.reportVoiceCensusForTesting()
        XCTAssertTrue(traces.isEmpty)
        try await receive("turn.delta")
        clock.advance(1)
        model.reportVoiceCensusForTesting()
        XCTAssertEqual(traces.map(\.0), ["voice_event_types"])
        XCTAssertEqual(traces.map(\.1), ["part=1/1;types=session.output_audio.delta:2:ignored|turn.delta:2:ignored"])
        XCTAssertTrue(traces.allSatisfy { $0.1.count <= 200 })
        epoch = model.installVoiceSessionForTesting(session, api: api)
        traces.removeAll()
        try await receive("turn.delta")
        clock.advance(60)
        model.reportVoiceCensusForTesting()
        XCTAssertEqual(traces.filter { $0.0 == "voice_event_types" }.map(\.1), ["part=1/1;types=turn.delta:1:ignored"])
    }

    func testCensusOrderingTruncationOmittedAndOverflow() {
        var census = EventCensusDiagnostics()
        census.note(type: "z", activity: true)
        census.note(type: "z", activity: true)
        census.note(type: "a")
        XCTAssertEqual(census.summary(), "types=z:2:act=2|a:1:ignored")
        for n in 0..<50 { census.note(type: "long_event_type_\(n)") }
        let summary = census.summary()
        XCTAssertLessThanOrEqual(summary.count, 200)
        XCTAssertTrue(summary.hasPrefix("types=other:12:ignored"))
        XCTAssertTrue(summary.contains("|+"))
        let omitted = Int(summary.components(separatedBy: "|+").last!)!
        XCTAssertEqual(summary.components(separatedBy: "|").count - 1 + omitted, 41)
    }

    func testEventsThrottleFlushAndSurfacePriority() {
        let start = Date(timeIntervalSince1970: 0)
        var events = EventsReceivedDiagnostics(now: start)
        for n in 0..<39 { events.note(type: "task.event_\(n)") }
        events.note(type: "ui.surface")
        XCTAssertNil(events.report(now: start.addingTimeInterval(29)))
        let summary = events.report(now: start.addingTimeInterval(30))!
        XCTAssertTrue(summary.hasPrefix("n=40;types=ui.surface:1"))
        XCTAssertLessThanOrEqual(summary.count, 200)
        events.note(type: "task.created")
        XCTAssertEqual(events.report(now: start.addingTimeInterval(31), force: true), "n=1;types=task.created:1")
        XCTAssertNil(events.report(now: start.addingTimeInterval(60), force: true))
    }
}
