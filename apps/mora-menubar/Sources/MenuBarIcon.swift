import AppKit

enum MenuBarIcon {
    static let normal = make(attention: false)
    static let attention = make(attention: true)

    private static func make(attention: Bool) -> NSImage {
        // MenuBarExtra uses the native image size, not a SwiftUI frame.
        let image = NSImage(size: NSSize(width: 20, height: 18), flipped: false) { _ in
            let eye = NSImage(systemSymbolName: "eye", accessibilityDescription: nil)!
            eye.draw(in: NSRect(x: 1, y: 3, width: 18, height: 12))
            if attention {
                NSColor.black.setFill()
                NSBezierPath(ovalIn: NSRect(x: 16, y: 14, width: 3, height: 3)).fill()
            }
            return true
        }
        image.isTemplate = true
        return image
    }
}
