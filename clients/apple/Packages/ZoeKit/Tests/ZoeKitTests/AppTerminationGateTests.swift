import XCTest
import ZoeUI

@MainActor
final class AppTerminationGateTests: XCTestCase {
    func testInactiveTerminatesImmediately() async {
        let gate = AppTerminationGate()
        let unexpected = expectation(description: "no shutdown or deferred reply")
        unexpected.isInverted = true
        XCTAssertEqual(gate.request(active: false, shutdown: { unexpected.fulfill() }, reply: { unexpected.fulfill() }), .terminateNow)
        await fulfillment(of: [unexpected], timeout: 0.05)
    }

    func testActiveRepliesAfterShutdownAndIgnoresSecondRequest() async {
        let gate = AppTerminationGate()
        let started = expectation(description: "shutdown started")
        let replied = expectation(description: "replied after shutdown")
        let unexpected = expectation(description: "no second shutdown or reply")
        unexpected.isInverted = true
        let (shutdownRelease, release) = AsyncStream<Void>.makeStream()
        var didShutdown = false
        var replies = 0
        XCTAssertEqual(gate.request(active: true, shutdown: {
            started.fulfill()
            for await _ in shutdownRelease {}
            didShutdown = true
        }, reply: {
            XCTAssertTrue(didShutdown)
            replies += 1
            if replies == 1 { replied.fulfill() } else { unexpected.fulfill() }
        }), .terminateLater)
        await fulfillment(of: [started], timeout: 1)
        XCTAssertEqual(replies, 0)
        // The first shutdown may already have reset the active flag.
        XCTAssertEqual(gate.request(active: false, shutdown: { unexpected.fulfill() }, reply: { unexpected.fulfill() }), .terminateLater)
        release.finish()
        await fulfillment(of: [replied], timeout: 1)
        await fulfillment(of: [unexpected], timeout: 0.05)
        XCTAssertEqual(replies, 1)
    }

    func testTimeoutRepliesWhileShutdownIsStillBlockedAndNeverRepliesAgain() async {
        let gate = AppTerminationGate(timeout: .milliseconds(50))
        let replied = expectation(description: "replied on timeout")
        let finished = expectation(description: "shutdown eventually finished")
        let unexpected = expectation(description: "no late second reply")
        unexpected.isInverted = true
        let (shutdownRelease, release) = AsyncStream<Void>.makeStream()
        var didShutdown = false
        var replies = 0
        XCTAssertEqual(gate.request(active: true, shutdown: {
            for await _ in shutdownRelease {}
            didShutdown = true
            finished.fulfill()
        }, reply: {
            replies += 1
            if replies == 1 { replied.fulfill() } else { unexpected.fulfill() }
        }), .terminateLater)
        await fulfillment(of: [replied], timeout: 1)
        XCTAssertFalse(didShutdown)
        release.finish()
        await fulfillment(of: [finished], timeout: 1)
        await fulfillment(of: [unexpected], timeout: 0.05)
        XCTAssertEqual(replies, 1)
    }
}
