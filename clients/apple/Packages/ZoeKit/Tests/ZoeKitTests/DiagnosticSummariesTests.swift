import XCTest
@testable import ZoeKit
@testable import ZoeUI

private final class CensusClock: SilenceClock {
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
        let model = ZoeAppModel(defaults: defaults, silenceClock: clock)
        model.setSilenceHangupEnabled(false)
        let api = try APIClient(serverURL: URL(string: "https://census.example")!)
        let session = VoiceSessionResponse(voiceSessionId: "census-test", generation: 1, state: "active", answerSdp: "")
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
