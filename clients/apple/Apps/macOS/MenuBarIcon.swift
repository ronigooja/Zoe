import AppKit

enum MenuBarIcon {
    static let size: CGFloat = 18
    static let discDiameter: CGFloat = 8
    static let ringOuterRadius: CGFloat = 8.2
    static let ringStrokeWidth: CGFloat = 1.6

    static func image() -> NSImage {
        let image = NSImage(size: NSSize(width: size, height: size), flipped: false) { rect in
            let center = NSPoint(x: rect.midX, y: rect.midY)
            NSColor.black.setFill()
            NSBezierPath(ovalIn: circleRect(center: center, radius: discDiameter / 2)).fill()
            let ring = NSBezierPath(ovalIn: circleRect(center: center, radius: ringOuterRadius - ringStrokeWidth / 2))
            ring.lineWidth = ringStrokeWidth
            NSColor.black.setStroke()
            ring.stroke()
            return true
        }
        image.isTemplate = true
        image.accessibilityDescription = "Zoe"
        return image
    }

    private static func circleRect(center: NSPoint, radius: CGFloat) -> NSRect {
        NSRect(x: center.x - radius, y: center.y - radius, width: radius * 2, height: radius * 2)
    }
}
