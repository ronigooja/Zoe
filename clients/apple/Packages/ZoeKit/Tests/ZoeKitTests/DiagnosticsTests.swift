import XCTest
@testable import ZoeKit

private struct DiagnosticsCredential: BearerTokenProviding {
    func bearerToken() throws -> String { "diagnostics-test-token" }
}

private final class DiagnosticsFailureURLProtocol: URLProtocol {
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        let response = HTTPURLResponse(url: request.url!, statusCode: 503, httpVersion: nil, headerFields: nil)!
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        client?.urlProtocolDidFinishLoading(self)
    }
    override func stopLoading() {}
}

final class DiagnosticsTests: XCTestCase {
    func testDiagnosticLevelParsingAndEventEncoding() throws {
        XCTAssertEqual(DiagnosticLevel.parse("debug"), .debug)
        XCTAssertEqual(DiagnosticLevel.parse("unknown"), .error)
        let event = DiagnosticEvent(clientRequestID: "r", phase: "p", requestPath: "/v1/p", installationID: "i", level: .info)
        let data = try JSONEncoder().encode(event)
        let object = try XCTUnwrap(try JSONSerialization.jsonObject(with: data) as? [String: Any])
        XCTAssertEqual(object["level"] as? String, "info")
    }

    func testDiagnosticPayloadIncludesBuildIdentityAndSuccessIsInfo() async throws {
        let server = URL(string: "https://identity-\(UUID().uuidString).example")!
        let installation = "identity-\(UUID().uuidString)"
        let api = try APIClient(serverURL: server, credentials: nil, installationID: installation)
        await api.applyDiagnosticLevel(.info)
        await api.recordDiagnosticAndWait(phase: "request", requestPath: "/v1/p", httpStatus: 200)
        let queue = try XCTUnwrap(EncryptedDiagnosticQueue(serverURL: api.serverURL, installationID: installation))
        for _ in 0..<20 { if !(await queue.take(limit: 10)).isEmpty { break }; await Task.yield() }
        let events = await queue.take(limit: 10)
        let event = try XCTUnwrap(events.first)
        XCTAssertFalse(event.buildVersion.isEmpty)
        XCTAssertFalse(event.sourceRevision.isEmpty)
        XCTAssertEqual(event.level, DiagnosticLevel.info.rawValue)
    }

    func testDiagnosticUploadFailureIsCountedAndRetainsEncryptedEvent() async throws {
        let server = URL(string: "https://failure-\(UUID().uuidString).example")!
        let installation = "failure-\(UUID().uuidString)"
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [DiagnosticsFailureURLProtocol.self]
        let api = try APIClient(serverURL: server, credentials: DiagnosticsCredential(), session: URLSession(configuration: configuration), installationID: installation)
        api.recordDiagnostic(phase: "request", requestPath: "/v1/p", errorType: "transport_error", errorSummary: "offline")
        let queue = try XCTUnwrap(EncryptedDiagnosticQueue(serverURL: api.serverURL, installationID: installation))
        for _ in 0..<20 { if !(await queue.take(limit: 10)).isEmpty { break }; await Task.yield() }
        let before = await queue.take(limit: 10).count
        await api.uploadDiagnosticsIfPossible()
        let after = await queue.take(limit: 10).count
        XCTAssertEqual(after, before)
        XCTAssertGreaterThan(UserDefaults.standard.integer(forKey: "zoe.diagnostics.delivery_failures.\(api.serverURL.absoluteString).upload"), 0)
    }

    func testDiagnosticLevelFiltersAtRecordTimeAndPersists() async throws {
        let server = URL(string: "https://level-\(UUID().uuidString).example")!
        let installation = "level-test-\(UUID().uuidString)"
        let queue = try XCTUnwrap(EncryptedDiagnosticQueue(serverURL: server, installationID: installation))
        await queue.setLevel(.info)
        let debug = DiagnosticEvent(clientRequestID: "debug", phase: "p", requestPath: "/v1/p", installationID: installation, level: .debug)
        let info = DiagnosticEvent(clientRequestID: "info", phase: "p", requestPath: "/v1/p", installationID: installation, level: .info)
        await queue.append(debug)
        await queue.append(info)
        let recorded = await queue.take(limit: 10)
        XCTAssertEqual(recorded.map(\.clientRequestID), ["info"])
        let restored = try XCTUnwrap(EncryptedDiagnosticQueue(serverURL: server, installationID: installation))
        let restoredLevel = await restored.currentLevel()
        XCTAssertEqual(restoredLevel, .info)
    }

    func testClientPolicyClampsIntervalAndUnknownLevel() throws {
        let decoder = JSONDecoder()
        let low = try decoder.decode(ClientPolicy.self, from: Data(#"{"log_level":"verbose","poll_interval_seconds":1,"revision":3}"#.utf8))
        XCTAssertEqual(low.logLevel, .error)
        XCTAssertEqual(low.pollIntervalSeconds, 30)
        let high = try decoder.decode(ClientPolicy.self, from: Data(#"{"log_level":"debug","poll_interval_seconds":999,"revision":4}"#.utf8))
        XCTAssertEqual(high.logLevel, .debug)
        XCTAssertEqual(high.pollIntervalSeconds, 600)
    }

    func testDiagnosticEventHashesInstallationAndQueueBoundsOldestEntries() async throws {
        let server = URL(string: "https://zoe-\(UUID().uuidString).example")!
        let installation = "diagnostics-test-installation"
        let queue = try XCTUnwrap(EncryptedDiagnosticQueue(serverURL: server, installationID: installation))
        for index in 0..<101 {
            let event = DiagnosticEvent(clientRequestID: "r-\(index)", phase: "config_request", requestPath: "/v1/voice/config", installationID: installation)
            await queue.append(event)
        }
        let events = await queue.take(limit: 200)
        XCTAssertEqual(events.count, 100)
        XCTAssertEqual(events.first?.clientRequestID, "r-1")
        XCTAssertEqual(events.first?.deviceIDHash.count, 64)
        XCTAssertFalse(events.first?.errorSummary.contains("Authorization") == true)
    }
}
