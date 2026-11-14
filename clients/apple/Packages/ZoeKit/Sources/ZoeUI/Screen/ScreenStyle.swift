#if os(macOS)
import SwiftUI

struct MaterialView: NSViewRepresentable {
    let material: String
    let opacity: Double

    func makeNSView(context: Context) -> NSVisualEffectView {
        let view = NSVisualEffectView()
        view.blendingMode = .behindWindow
        view.state = .active
        return view
    }

    func updateNSView(_ view: NSVisualEffectView, context: Context) {
        switch material.lowercased() {
        case "sidebar": view.material = .sidebar
        case "menu": view.material = .menu
        case "popover": view.material = .popover
        case "hud": view.material = .hudWindow
        case "content": view.material = .contentBackground
        default: view.material = .underWindowBackground
        }
        view.alphaValue = max(0, min(1, opacity))
    }
}
#endif
