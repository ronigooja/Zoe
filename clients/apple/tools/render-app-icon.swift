import AppKit

let icons: [(name: String, pixels: Int)] = [
    ("icon_16x16.png", 16),
    ("icon_16x16@2x.png", 32),
    ("icon_32x32.png", 32),
    ("icon_32x32@2x.png", 64),
    ("icon_128x128.png", 128),
    ("icon_128x128@2x.png", 256),
    ("icon_256x256.png", 256),
    ("icon_256x256@2x.png", 512),
    ("icon_512x512.png", 512),
    ("icon_512x512@2x.png", 1024),
]

func fail(_ message: String) -> Never {
    FileHandle.standardError.write(Data((message + "\n").utf8))
    exit(1)
}

let args = CommandLine.arguments
guard args.count == 3 else { fail("usage: render-app-icon.swift <svg-path> <output-dir>") }
guard let source = NSImage(contentsOf: URL(fileURLWithPath: args[1])), source.isValid else { fail("load failed: \(args[1])") }
let outputDirectory = URL(fileURLWithPath: args[2], isDirectory: true)
do {
    try FileManager.default.createDirectory(at: outputDirectory, withIntermediateDirectories: true)
} catch {
    fail("cannot create output directory: \(error)")
}

func renderPNG(pixels: Int) -> Data {
    guard let rep = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: pixels, pixelsHigh: pixels, bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false, colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0) else { fail("bitmap failed at \(pixels) px") }
    rep.size = NSSize(width: pixels, height: pixels)
    guard let context = NSGraphicsContext(bitmapImageRep: rep) else { fail("graphics context failed at \(pixels) px") }
    NSGraphicsContext.saveGraphicsState()
    NSGraphicsContext.current = context
    context.imageInterpolation = .high
    NSColor.clear.setFill()
    NSRect(x: 0, y: 0, width: pixels, height: pixels).fill(using: .copy)
    source.draw(in: NSRect(x: 0, y: 0, width: pixels, height: pixels), from: .zero, operation: .sourceOver, fraction: 1)
    NSGraphicsContext.restoreGraphicsState()
    guard let png = rep.representation(using: .png, properties: [:]) else { fail("PNG encoding failed at \(pixels) px") }
    return png
}

for icon in icons {
    let url = outputDirectory.appendingPathComponent(icon.name)
    do {
        try renderPNG(pixels: icon.pixels).write(to: url)
    } catch {
        fail("write failed: \(icon.name): \(error)")
    }
}

for icon in icons {
    let url = outputDirectory.appendingPathComponent(icon.name)
    guard let data = try? Data(contentsOf: url), let rep = NSBitmapImageRep(data: data),
          rep.pixelsWide == icon.pixels, rep.pixelsHigh == icon.pixels else {
        fail("verification failed: \(icon.name) is missing or not \(icon.pixels) px")
    }
}

print("rendered \(icons.count) icons to \(outputDirectory.path)")
