import AppKit
import SwiftUI

enum Brand {
    static let menuBar: NSImage = {
        let image = NSImage(size: NSSize(width: 18, height: 18), flipped: false) { _ in
            func slab(_ r: NSRect, _ alpha: CGFloat) {
                NSColor.black.withAlphaComponent(alpha).setFill()
                NSBezierPath(roundedRect: r, xRadius: 1.2, yRadius: 1.2).fill()
            }
            let s: CGFloat = 16 / 520, x0 = (18 - 380 * s) / 2, y0: CGFloat = 1
            slab(NSRect(x: x0, y: y0, width: 120 * s, height: 520 * s), 1)
            slab(NSRect(x: x0, y: y0 + 400 * s, width: 380 * s, height: 120 * s), 1)
            slab(NSRect(x: x0, y: y0 + 220 * s, width: 290 * s, height: 120 * s), 0.65)
            return true
        }
        image.isTemplate = true
        image.accessibilityDescription = "Fevom"
        return image
    }()
}

struct BrandIcon: View {
    var size: CGFloat

    var body: some View {
        Image(nsImage: NSApp.applicationIconImage)
            .resizable()
            .interpolation(.high)
            .frame(width: size, height: size)
            .accessibilityHidden(true)
    }
}
