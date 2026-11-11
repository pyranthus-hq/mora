import AppKit

@main
struct MenuBarIconTests {
    static func main() {
        for image in [MenuBarIcon.normal, MenuBarIcon.attention] {
            precondition(image.size == NSSize(width: 20, height: 18))
            precondition(image.isTemplate)
            precondition(image.tiffRepresentation != nil)
        }
        precondition(MenuBarIcon.normal.tiffRepresentation != MenuBarIcon.attention.tiffRepresentation)
        print("Menu bar icon size and template checks passed")
    }
}
