#if os(macOS)
import AppKit
import XCTest
import A2UISwiftCore
import ZoeKit
@testable import ZoeUI

@MainActor
final class SurfaceManagerTests: XCTestCase {
    private func event(version: Int64, deleted: Bool = false) throws -> UISurfaceEvent {
        let messages = deleted
            ? #"[{"version":"v0.9","deleteSurface":{"surfaceId":"versioned"}}]"#
            : #"[{"version":"v0.9","createSurface":{"surfaceId":"versioned","catalogId":"https://a2ui.org/specification/v0_9/basic_catalog.json"}},{"version":"v0.9","updateComponents":{"surfaceId":"versioned","components":[{"id":"root","component":"Text","text":"Hello"}]}}]"#
        let json = "{\"request_id\":\"r1\",\"surface_id\":\"versioned\",\"surface_version\":\(version),\"deleted\":\(deleted),\"container\":\"window\",\"awaiting_input\":false,\"messages\":\(messages)}"
        let value = try JSONDecoder().decode(JSONValue.self, from: Data(json.utf8))
        return try XCTUnwrap(UISurfaceEvent.decode(value))
    }

    func testClosedRevisionDoesNotReopenAndDeletionRendersSuccessfully() throws {
        _ = NSApplication.shared
        let manager = SurfaceManager(popoverAnchor: { nil })
        defer { manager.reset() }
        XCTAssertTrue(manager.display(try event(version: 1)))
        manager.close(id: "versioned")
        XCTAssertFalse(manager.display(try event(version: 1)))
        XCTAssertNil(manager.viewModel(for: "versioned"))
        XCTAssertTrue(manager.display(try event(version: 2)))
        XCTAssertTrue(manager.display(try event(version: 3, deleted: true)))
        XCTAssertNil(manager.viewModel(for: "versioned"))
        XCTAssertFalse(manager.display(try event(version: 2)))
        manager.reset()
        XCTAssertTrue(manager.display(try event(version: 1)))
    }

    func testActualRendererActionClosureKeepsCapturedRevisionAfterReplacement() async throws {
        _ = NSApplication.shared
        let manager = SurfaceManager(popoverAnchor: { nil })
        defer { manager.reset() }
        XCTAssertTrue(manager.display(try event(version: 1)))
        let oldRendererHandler = manager.capturedActionHandler(id: "versioned")
        XCTAssertTrue(manager.display(try event(version: 2)))
        let received = expectation(description: "Old renderer dispatches original identity")
        manager.onBoundAction = { binding, action in
            XCTAssertEqual(binding.surfaceVersion, 1)
            XCTAssertEqual(binding.surfaceID, "versioned")
            XCTAssertEqual(action.name, "answer")
            received.fulfill()
        }
        oldRendererHandler(ResolvedAction(name: "answer", sourceComponentId: "submit", context: [:]))
        await fulfillment(of: [received], timeout: 2)
    }

    func testDisplayReportsRendererFailureInsteadOfSuccess() throws {
        _ = NSApplication.shared
        let manager = SurfaceManager(popoverAnchor: { nil })
        defer { manager.reset() }
        let invalid = try messages("""
        [{"version":"v0.9","updateDataModel":{"surfaceId":"invalid","path":"/value","value":"primitive"}},
         {"version":"v0.9","updateDataModel":{"surfaceId":"invalid","path":"/value/child","value":"invalid"}}]
        """)
        XCTAssertFalse(manager.display(SurfaceSpec(id: "invalid", kind: .window, messages: invalid)))
        XCTAssertNil(manager.viewModel(for: "invalid"))
    }

    private func messages(_ json: String) throws -> [A2uiMessage] {
        try JSONDecoder().decode([A2uiMessage].self, from: Data(json.utf8))
    }

    func testWindowOpensAtUsableSizeAndPreservesResizedContentOnUpdate() throws {
        _ = NSApplication.shared
        let manager = SurfaceManager(popoverAnchor: { nil })
        let id = "surface-window-size-test"
        defer { manager.close(id: id) }
        let batch = try messages("""
        [{"version":"v0.9","createSurface":{"surfaceId":"\(id)","catalogId":"https://a2ui.org/specification/v0_9/basic_catalog.json"}},
         {"version":"v0.9","updateComponents":{"surfaceId":"\(id)","components":[
           {"id":"root","component":"Column","children":["text"]},
           {"id":"text","component":"Text","text":"Hello"}]}}]
        """)
        var renderErrors: [Error] = []
        manager.onRenderError = { _, errors in renderErrors.append(contentsOf: errors) }
        manager.open(SurfaceSpec(id: id, kind: .window, messages: batch))
        let window = try XCTUnwrap(NSApp.windows.first { $0.identifier?.rawValue == id })
        let initial = window.contentRect(forFrameRect: window.frame).size
        XCTAssertGreaterThanOrEqual(initial.width, 300)
        XCTAssertGreaterThanOrEqual(initial.height, 200)
        let visible = try XCTUnwrap(NSScreen.main).visibleFrame.size
        XCTAssertEqual(initial, NSSize(width: min(520, visible.width - 20), height: min(640, visible.height - 20)))
        XCTAssertGreaterThanOrEqual(window.minSize.width, 320)
        XCTAssertGreaterThanOrEqual(window.minSize.height, 240)
        window.setContentSize(NSSize(width: 620, height: 460))
        let resized = window.contentRect(forFrameRect: window.frame).size
        manager.update(id: id, messages: Array(batch.suffix(1)))
        window.contentView?.layoutSubtreeIfNeeded()
        XCTAssertEqual(window.contentRect(forFrameRect: window.frame).size, resized)
        XCTAssertTrue(renderErrors.isEmpty)
    }

    func testRenderErrorsAreReportedForOpenAndUpdate() throws {
        _ = NSApplication.shared
        let manager = SurfaceManager(popoverAnchor: { nil })
        let id = "surface-render-error-test"
        defer { manager.close(id: id) }
        let invalid = try messages("""
        [{"version":"v0.9","updateDataModel":{"surfaceId":"\(id)","path":"/value","value":"primitive"}},
         {"version":"v0.9","updateDataModel":{"surfaceId":"\(id)","path":"/value/child","value":"invalid"}}]
        """)
        var reports: [(String, [Error])] = []
        manager.onRenderError = { reports.append(($0, $1)) }
        manager.open(SurfaceSpec(id: id, kind: .popover, messages: invalid))
        XCTAssertEqual(reports.count, 1)
        XCTAssertEqual(reports.first?.0, id)
        XCTAssertEqual(reports.first?.1.count, 1)
        manager.update(id: id, messages: invalid)
        XCTAssertEqual(reports.count, 2)
        XCTAssertEqual(reports.last?.0, id)
        XCTAssertEqual(reports.last?.1.count, 1)
    }

    func testRequestedWindowSizeUpdatesClampsAndIgnoresTheme() throws {
        _ = NSApplication.shared
        let manager = SurfaceManager(popoverAnchor: { nil })
        let id = "surface-explicit-size-test"
        defer { manager.close(id: id) }
        let batch = try messages("""
        [{"version":"v0.9","createSurface":{"surfaceId":"\(id)","catalogId":"https://a2ui.org/specification/v0_9/basic_catalog.json","theme":{"primaryColor":"#FF0000"}}},
         {"version":"v0.9","updateComponents":{"surfaceId":"\(id)","components":[{"id":"root","component":"Text","text":"Hello"}]}}]
        """)
        let visible = try XCTUnwrap(NSScreen.main).visibleFrame.size
        manager.open(SurfaceSpec(id: id, kind: .window, messages: batch, size: .init(width: 700, height: 500)))
        let window = try XCTUnwrap(NSApp.windows.first { $0.identifier?.rawValue == id })
        XCTAssertEqual(window.contentRect(forFrameRect: window.frame).size, NSSize(width: min(700, visible.width - 20), height: min(500, visible.height - 20)))
        XCTAssertEqual(manager.viewModel(for: id)?.a2uiStyle, SurfaceManager.fixedStyle)
        let before = window.contentRect(forFrameRect: window.frame).size
        XCTAssertTrue(manager.display(SurfaceSpec(id: id, kind: .window, messages: batch)))
        XCTAssertEqual(window.contentRect(forFrameRect: window.frame).size, before)
        XCTAssertEqual(manager.viewModel(for: id)?.a2uiStyle.primaryColor, SurfaceManager.fixedStyle.primaryColor)
        XCTAssertTrue(manager.display(SurfaceSpec(id: id, kind: .window, messages: batch, size: .init(width: 600, height: 400))))
        XCTAssertEqual(window.contentRect(forFrameRect: window.frame).size, NSSize(width: min(600, visible.width - 20), height: min(400, visible.height - 20)))
        XCTAssertTrue(manager.display(SurfaceSpec(id: id, kind: .window, messages: batch, size: .init(width: 5000, height: 5000))))
        let oversized = window.contentRect(forFrameRect: window.frame).size
        XCTAssertLessThanOrEqual(oversized.width, visible.width - 20)
        XCTAssertLessThanOrEqual(oversized.height, visible.height - 20)
        XCTAssertGreaterThanOrEqual(oversized.width, 320)
        XCTAssertGreaterThanOrEqual(oversized.height, 240)
    }

    func testPopoverPreservesSizeWithoutRequestAndAppliesRequest() throws {
        _ = NSApplication.shared
        let manager = SurfaceManager(popoverAnchor: { nil })
        let id = "surface-popover-size-test"
        defer { manager.close(id: id) }
        manager.open(SurfaceSpec(id: id, kind: .popover, messages: [], size: .init(width: 360, height: 320)))
        XCTAssertEqual(manager.popoverContentSize(for: id), NSSize(width: 360, height: 320))
        manager.update(id: id, messages: [])
        XCTAssertEqual(manager.popoverContentSize(for: id), NSSize(width: 360, height: 320))
        _ = manager.display(SurfaceSpec(id: id, kind: .popover, messages: [], size: .init(width: 380, height: 340)))
        XCTAssertEqual(manager.popoverContentSize(for: id), NSSize(width: 380, height: 340))
    }

    func testMissingRootBatchDoesNotCrash() throws {
        _ = NSApplication.shared
        let manager = SurfaceManager(popoverAnchor: { nil })
        let id = "surface-missing-root-test"
        defer { manager.close(id: id) }
        let batch = try messages("""
        [{"version":"v0.9","updateComponents":{"surfaceId":"\(id)","components":[{"id":"text","component":"Text","text":"Hello"}]}}]
        """)
        manager.open(SurfaceSpec(id: id, kind: .popover, messages: batch))
        manager.update(id: id, messages: batch)
    }
}
#endif
