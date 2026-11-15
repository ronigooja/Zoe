import XCTest
@testable import ZoeKit

final class DiagnosticSummariesTests: XCTestCase {
    func testSettingsAndTimeoutFormats() {
        XCTAssertEqual(DiagnosticSummaries.settingsSuffix(enabled: true, seconds: 60), ";sil=on,60")
        XCTAssertEqual(DiagnosticSummaries.settingsSuffix(enabled: false, seconds: 60), ";sil=off")
        XCTAssertEqual(DiagnosticSummaries.settings(enabled: true, seconds: 60.5), "on,60.5")
        XCTAssertEqual(DiagnosticSummaries.settings(enabled: true, seconds: 60) + "->" + DiagnosticSummaries.settings(enabled: false, seconds: 60), "on,60->off,60")
        XCTAssertEqual(DiagnosticSummaries.timeout(age: 60.25, timeout: 60, enabled: true, interaction: false, elapsed: 103), "since_last_activity=60.2;timeout=60;enabled=1;interaction=0;t=103")
        XCTAssertLessThanOrEqual(DiagnosticSummaries.timeout(age: .infinity, timeout: .greatestFiniteMagnitude, enabled: false, interaction: true, elapsed: .infinity).count, 200)
    }

    func testSilenceResetThrottleAndAggregation() {
        var trace = SilenceResetDiagnostics()
        let start = Date(timeIntervalSince1970: 0)
        XCTAssertEqual(trace.note(type: "connect", now: start, elapsed: 0), "n=1;last=connect;t=0")
        XCTAssertNil(trace.note(type: "assistant.output", now: start.addingTimeInterval(9), elapsed: 9))
        XCTAssertEqual(trace.note(type: "input_audio_transcription.completed", now: start.addingTimeInterval(10), elapsed: 10), "n=2;last=input_audio_transcription.completed;t=10")
        XCTAssertNil(trace.note(type: "ignored", now: start.addingTimeInterval(19.9), elapsed: 19.9))
        XCTAssertEqual(DiagnosticSummaries.eventType(String(repeating: "a", count: 100)).count, 48)
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
