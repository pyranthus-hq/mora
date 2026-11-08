import AppKit

enum MenuBarIcon {
    static let normal = make(attention: false)
    static let attention = make(attention: true)

    // Shared vector artwork: a watchful eye over three curling tentacles.
    static func drawMark(color: NSColor) {
        color.setStroke()
        color.setFill()
        let eye = NSBezierPath()
        eye.move(to: NSPoint(x: 2, y: 11))
        eye.curve(to: NSPoint(x: 18, y: 11), controlPoint1: NSPoint(x: 6, y: 18), controlPoint2: NSPoint(x: 14, y: 18))
        eye.curve(to: NSPoint(x: 2, y: 11), controlPoint1: NSPoint(x: 14, y: 5), controlPoint2: NSPoint(x: 6, y: 5))
        eye.close()
        eye.lineWidth = 1.4
        eye.stroke()
        NSBezierPath(roundedRect: NSRect(x: 9, y: 9, width: 2, height: 5), xRadius: 1, yRadius: 1).fill()

        let tendrils = NSBezierPath()
        tendrils.move(to: NSPoint(x: 5, y: 8))
        tendrils.curve(to: NSPoint(x: 2, y: 3), controlPoint1: NSPoint(x: 6, y: 3), controlPoint2: NSPoint(x: 0, y: 6))
        tendrils.curve(to: NSPoint(x: 5, y: 3), controlPoint1: NSPoint(x: 3, y: 1), controlPoint2: NSPoint(x: 5, y: 1))
        tendrils.move(to: NSPoint(x: 10, y: 7))
        tendrils.curve(to: NSPoint(x: 11, y: 1.5), controlPoint1: NSPoint(x: 7, y: 1), controlPoint2: NSPoint(x: 12, y: 5))
        tendrils.move(to: NSPoint(x: 15, y: 8))
        tendrils.curve(to: NSPoint(x: 18, y: 3), controlPoint1: NSPoint(x: 14, y: 3), controlPoint2: NSPoint(x: 20, y: 6))
        tendrils.curve(to: NSPoint(x: 15, y: 3), controlPoint1: NSPoint(x: 17, y: 1), controlPoint2: NSPoint(x: 15, y: 1))
        tendrils.lineWidth = 1.4
        tendrils.lineCapStyle = .round
        tendrils.stroke()
    }

    private static func make(attention: Bool) -> NSImage {
        // MenuBarExtra uses the native image size, not a SwiftUI frame.
        let image = NSImage(size: NSSize(width: 20, height: 18), flipped: false) { _ in
            drawMark(color: .black)
            if attention {
                NSColor.black.setFill()
                NSBezierPath(ovalIn: NSRect(x: 17, y: 15, width: 2.5, height: 2.5)).fill()
            }
            return true
        }
        image.isTemplate = true
        return image
    }
}
