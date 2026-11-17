#if os(macOS)
import AppKit
import SwiftUI
import XCTest
import A2UISwiftCore
import A2UISwiftUI
@testable import ZoeUI

/// Offline render harness: draws a messages JSON with the production style and container.
/// Inert unless ZOE_RENDER_INPUT is set. See docs/development/verification.md.
///   ZOE_RENDER_INPUT   path to a JSON array of A2UI messages
///   ZOE_RENDER_OUTPUT  output prefix; writes <prefix>-light.png and <prefix>-dark.png
///   ZOE_RENDER_SIZE    optional "WxH" in points (default 400x480, the popover fallback)
@MainActor
final class RenderHarnessTests: XCTestCase {
    func testRenderMessagesToPNG() throws {
        let env = ProcessInfo.processInfo.environment
        guard let input = env["ZOE_RENDER_INPUT"], let prefix = env["ZOE_RENDER_OUTPUT"] else {
            throw XCTSkip("ZOE_RENDER_INPUT / ZOE_RENDER_OUTPUT not set")
        }
        _ = NSApplication.shared
        var size = NSSize(width: 400, height: 480)
        if let raw = env["ZOE_RENDER_SIZE"] {
            let parts = raw.split(separator: "x").compactMap { Double($0) }
            if parts.count == 2 { size = NSSize(width: parts[0], height: parts[1]) }
        }
        let data = try Data(contentsOf: URL(fileURLWithPath: input))
        let messages = try JSONDecoder().decode([A2uiMessage].self, from: data)
        let model = SurfaceViewModel(catalog: basicCatalog)
        let errors = model.processMessages(messages)
        model.a2uiStyle = SurfaceManager.fixedStyle
        if !errors.isEmpty {
            try Data("\(errors)".utf8).write(to: URL(fileURLWithPath: prefix + "-errors.txt"))
        }
        for (name, appearance) in [("light", NSAppearance.Name.aqua), ("dark", NSAppearance.Name.darkAqua)] {
            // The window container has an opaque system background; the capture would otherwise be transparent.
            let host = NSHostingView(rootView: SurfaceManager.surfaceRootView(model, onAction: { _ in })
                .background(Color(nsColor: .windowBackgroundColor)))
            let window = NSWindow(contentRect: NSRect(origin: .zero, size: size), styleMask: [.titled], backing: .buffered, defer: false)
            window.appearance = NSAppearance(named: appearance)
            window.contentView = host
            window.setContentSize(size)
            host.frame = NSRect(origin: .zero, size: size)
            host.layoutSubtreeIfNeeded()
            // Let AsyncImage and layout settle before capture.
            RunLoop.current.run(until: Date().addingTimeInterval(1.5))
            host.layoutSubtreeIfNeeded()
            let rep = try XCTUnwrap(host.bitmapImageRepForCachingDisplay(in: host.bounds))
            host.cacheDisplay(in: host.bounds, to: rep)
            let png = try XCTUnwrap(rep.representation(using: .png, properties: [:]))
            try png.write(to: URL(fileURLWithPath: "\(prefix)-\(name).png"))
        }
    }
}
#endif
