#if os(macOS)
import AppKit
import SwiftUI
import XCTest
import A2UISwiftCore
import ZoeKit
@testable import ZoeUI

@MainActor
final class SurfaceManagerTests: XCTestCase {
    func testOnlyEscapeDismissesPopoverByPolicy() {
        XCTAssertTrue(SurfaceClosePolicy.shouldDismissPopover(isEscape: true))
        XCTAssertFalse(SurfaceClosePolicy.shouldDismissPopover(isEscape: false))
    }

    func testFocusDeadlineDisabledPendingAndRegainedFocus() {
        let lost = Date(timeIntervalSince1970: 100)
        XCTAssertEqual(SurfaceClosePolicy.deadline(lostFocusAt: lost, delay: 60, awaitingInput: false), lost.addingTimeInterval(60))
        XCTAssertNil(SurfaceClosePolicy.deadline(lostFocusAt: lost, delay: 0, awaitingInput: false))
        XCTAssertNil(SurfaceClosePolicy.deadline(lostFocusAt: lost, delay: 60, awaitingInput: true))
        XCTAssertNil(SurfaceClosePolicy.deadline(lostFocusAt: nil, delay: 60, awaitingInput: false))
        XCTAssertEqual(SurfaceClosePolicy.deadline(lostFocusAt: lost, delay: 20, awaitingInput: false), lost.addingTimeInterval(20))
    }

    func testRecoveryPolicyOnlyRestoresPendingOrAlreadyPresentedAndAppliesDeletion() {
        XCTAssertFalse(SurfaceClosePolicy.shouldReplay(awaitingInput: false, deleted: false, wasPresented: false))
        XCTAssertTrue(SurfaceClosePolicy.shouldReplay(awaitingInput: true, deleted: false, wasPresented: false))
        XCTAssertTrue(SurfaceClosePolicy.shouldReplay(awaitingInput: false, deleted: false, wasPresented: true))
        XCTAssertTrue(SurfaceClosePolicy.shouldReplay(awaitingInput: false, deleted: true, wasPresented: false))
    }

    func testFixedStyleUsesTintedCardsAndSemanticCaptionColor() {
        let style = SurfaceManager.fixedStyle
        XCTAssertEqual(style.cardStyle.backgroundColor, Color.primary.opacity(0.06))
        XCTAssertEqual(SurfaceManager.cardFillOpacity, 0.06)
        XCTAssertEqual(SurfaceManager.surfacePadding, 8)
        XCTAssertEqual(SurfaceManager.cardPadding, 8)
        XCTAssertEqual(SurfaceManager.leafMargin, 4)
        XCTAssertEqual(style.cardStyle.padding, SurfaceManager.cardPadding)
        XCTAssertEqual(style.leafMargin, SurfaceManager.leafMargin)
        XCTAssertEqual(style.cardStyle.cornerRadius, 12)
        XCTAssertNil(style.cardStyle.shadowRadius)
        XCTAssertNil(style.cardStyle.shadowColor)
        XCTAssertNil(style.cardStyle.shadowY)
        XCTAssertEqual(style.textStyles["h1"]?.font, Font.system(size: 22))
        XCTAssertEqual(style.textStyles["h2"]?.font, Font.system(size: 18))
        XCTAssertEqual(style.textStyles["h3"]?.font, Font.system(size: 15))
        XCTAssertEqual(style.textStyles["h4"]?.font, Font.system(size: 14))
        XCTAssertEqual(style.textStyles["h5"]?.font, Font.system(size: 13))
        XCTAssertEqual(style.textStyles["h4"]?.weight, Font.Weight.semibold)
        XCTAssertEqual(style.textStyles["h5"]?.weight, Font.Weight.semibold)
        XCTAssertEqual(style.textStyles["body"]?.font, Font.system(size: 13))
        XCTAssertEqual(style.textStyles["caption"]?.font, Font.system(size: 11))
        XCTAssertEqual(style.textStyles["caption"]?.color, Color.secondary)
    }

    private func event(version: Int64, deleted: Bool = false, id: String = "versioned", awaitingInput: Bool = false) throws -> UISurfaceEvent {
        let messages = deleted
            ? #"[{"version":"v0.9","deleteSurface":{"surfaceId":"versioned"}}]"#
            : #"[{"version":"v0.9","createSurface":{"surfaceId":"versioned","catalogId":"https://a2ui.org/specification/v0_9/basic_catalog.json"}},{"version":"v0.9","updateComponents":{"surfaceId":"versioned","components":[{"id":"root","component":"Text","text":"Hello"}]}}]"#
        let json = "{\"request_id\":\"r1\",\"surface_id\":\"versioned\",\"surface_version\":\(version),\"deleted\":\(deleted),\"container\":\"window\",\"awaiting_input\":false,\"messages\":\(messages)}"
        var input = json.replacingOccurrences(of: "versioned", with: id)
            .replacingOccurrences(of: "\"awaiting_input\":false", with: "\"awaiting_input\":\(awaitingInput)")
        if awaitingInput {
            input = input.replacingOccurrences(of: "\"request_id\":\"r1\"",
                with: "\"request_id\":\"r1\",\"task_id\":\"task\",\"question_id\":\"question\"")
        }
        let value = try JSONDecoder().decode(JSONValue.self, from: Data(input.utf8))
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

    func testNewSurfaceReplacementUsesClosePathAndInvalidatesOldRenderEvidence() throws {
        _ = NSApplication.shared
        let manager = SurfaceManager(popoverAnchor: { nil })
        manager.newUISurfaceBehavior = .closeOldWindows
        defer { manager.reset() }
        let old = try event(version: 1, awaitingInput: true)
        XCTAssertTrue(manager.display(old))
        var closes = 0
        manager.onUserClosed = { closes += 1 }
        let next = try event(version: 1, id: "next")
        XCTAssertTrue(manager.display(next))
        XCTAssertEqual(closes, 1)
        XCTAssertNil(manager.viewModel(for: old.surfaceID))
        XCTAssertFalse(manager.isCurrentPresentation(old))
        XCTAssertTrue(manager.isCurrentPresentation(next))
        XCTAssertFalse(manager.display(old), "Closing must preserve the revision fence")
        XCTAssertNotNil(manager.viewModel(for: next.surfaceID))
    }

    func testDefaultAndStaleOrDeletedArrivalsDoNotCloseOtherSurfaces() throws {
        _ = NSApplication.shared
        let manager = SurfaceManager(popoverAnchor: { nil })
        defer { manager.reset() }
        let first = try event(version: 1)
        let second = try event(version: 1, id: "second")
        XCTAssertTrue(manager.display(first))
        XCTAssertTrue(manager.display(second))
        XCTAssertTrue(manager.isCurrentPresentation(first))
        manager.newUISurfaceBehavior = .closeOldWindows
        XCTAssertFalse(manager.display(first))
        XCTAssertTrue(manager.isCurrentPresentation(second))
        XCTAssertTrue(manager.display(try event(version: 2, deleted: true)))
        XCTAssertTrue(manager.isCurrentPresentation(second))
    }

    func testActualNativePresentationFailureRetriesExactRevisionAndKeepsTombstone() throws {
        _ = NSApplication.shared
        let manager = SurfaceManager(popoverAnchor: { nil })
        defer { manager.reset() }
        var attempts = 0
        manager.presentationAcceptedForTesting = { attempts += 1; return attempts > 1 }
        XCTAssertFalse(manager.display(try event(version: 1)))
        XCTAssertNil(manager.presentationVersionForTesting("versioned"))
        XCTAssertNil(manager.viewModel(for: "versioned"))
        XCTAssertTrue(manager.display(try event(version: 1)))
        XCTAssertEqual(manager.presentationVersionForTesting("versioned"), 1)
        XCTAssertNotNil(manager.viewModel(for: "versioned"))
        XCTAssertTrue(manager.display(try event(version: 2, deleted: true)))
        XCTAssertEqual(manager.presentationVersionForTesting("versioned"), 2)
        XCTAssertFalse(manager.display(try event(version: 1)))
        XCTAssertNil(manager.viewModel(for: "versioned"))
    }

    func testPhysicalEvidenceInvalidatedByFailedAndSuccessfulReplacements() throws {
        _ = NSApplication.shared
        let manager = SurfaceManager(popoverAnchor: { nil })
        defer { manager.reset() }
        let old = try event(version: 1)
        XCTAssertTrue(manager.display(old))
        XCTAssertTrue(manager.isCurrentPresentation(old))
        manager.presentationAcceptedForTesting = { false }
        XCTAssertFalse(manager.display(try event(version: 2)))
        XCTAssertFalse(manager.isCurrentPresentation(old), "Partial failed replacement cannot justify old ACK")
        manager.presentationAcceptedForTesting = { true }
        let newer = try event(version: 2)
        XCTAssertTrue(manager.display(newer))
        XCTAssertTrue(manager.isCurrentPresentation(newer))
        XCTAssertFalse(manager.isCurrentPresentation(old))
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
        XCTAssertTrue(window.isOpaque, "Transparent cards must not make standalone windows translucent")
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
        XCTAssertEqual(manager.viewModel(for: id)?.a2uiStyle.cardStyle.backgroundColor, SurfaceManager.fixedStyle.cardStyle.backgroundColor)
        let before = window.contentRect(forFrameRect: window.frame).size
        XCTAssertTrue(manager.display(SurfaceSpec(id: id, kind: .window, messages: batch)))
        XCTAssertEqual(window.contentRect(forFrameRect: window.frame).size, before)
        XCTAssertEqual(manager.viewModel(for: id)?.a2uiStyle.primaryColor, SurfaceManager.fixedStyle.primaryColor)
        XCTAssertEqual(manager.viewModel(for: id)?.a2uiStyle.cardStyle.backgroundColor, SurfaceManager.fixedStyle.cardStyle.backgroundColor)
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
