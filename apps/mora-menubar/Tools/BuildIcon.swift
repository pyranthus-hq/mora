import AppKit

@main
struct BuildIcon {
    static func main() throws {
        let directory = URL(fileURLWithPath: CommandLine.arguments[1])
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        for size in [16, 32, 128, 256, 512] {
            for scale in [1, 2] {
                let pixels = size * scale
                let bitmap = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: pixels, pixelsHigh: pixels,
                    bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false,
                    colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0)!
                NSGraphicsContext.saveGraphicsState()
                NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: bitmap)
                let side = CGFloat(pixels)
                NSColor.black.setFill()
                NSBezierPath(roundedRect: NSRect(x: side * 0.04, y: side * 0.04, width: side * 0.92, height: side * 0.92),
                    xRadius: side * 0.2, yRadius: side * 0.2).fill()
                let transform = NSAffineTransform()
                transform.translateX(by: side * 0.16, yBy: side * 0.194)
                transform.scale(by: side * 0.034)
                transform.concat()
                MenuBarIcon.drawMark(color: .white)
                NSGraphicsContext.restoreGraphicsState()
                let suffix = scale == 2 ? "@2x" : ""
                let name = "icon_\(size)x\(size)\(suffix).png"
                try bitmap.representation(using: .png, properties: [:])!.write(to: directory.appendingPathComponent(name))
            }
        }
    }
}
