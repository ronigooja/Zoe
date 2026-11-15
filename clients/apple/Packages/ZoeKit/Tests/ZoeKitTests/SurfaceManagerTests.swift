#if os(macOS)
import AppKit
import XCTest
import A2UISwiftCore
import ZoeKit
@testable import ZoeUI

@MainActor
final class SurfaceManagerTests: XCTestCase {
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
        XCTAssertEqual(initial, NSSize(width: 480, height: 400))
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
