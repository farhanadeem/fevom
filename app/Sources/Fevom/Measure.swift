import AppKit
import Foundation

@MainActor
enum Measure {
    private static var observer: CFRunLoopObserver?
    private static var path: String?
    nonisolated(unsafe) private static var spans: [(Double, Double)] = []
    nonisolated(unsafe) private static var woke: Double = 0
    private nonisolated static let origin = clock_gettime_nsec_np(CLOCK_UPTIME_RAW)

    nonisolated static func now() -> Double {
        let o = origin
        return Double(clock_gettime_nsec_np(CLOCK_UPTIME_RAW) - o) / 1e6
    }

    static func startRunLoopLog(_ file: String) {
        path = file
        _ = now()
        spans.reserveCapacity(1 << 16)
        let activities = CFRunLoopActivity.afterWaiting.rawValue | CFRunLoopActivity.beforeWaiting.rawValue
        let obs = CFRunLoopObserverCreateWithHandler(nil, activities, true, 0) { _, activity in
            let t = Measure.now()
            if activity == .afterWaiting {
                Measure.woke = t
            } else if Measure.woke > 0 {
                Measure.spans.append((Measure.woke, t - Measure.woke))
                Measure.woke = 0
            }
        }
        CFRunLoopAddObserver(CFRunLoopGetMain(), obs, .commonModes)
        observer = obs
    }

    static func writeRunLoopLog() {
        guard let path else { return }
        var text = ""
        text.reserveCapacity(spans.count * 20)
        for (s, d) in spans { text += String(format: "%.3f %.3f\n", s, d) }
        try? text.write(toFile: path, atomically: true, encoding: .utf8)
    }

    static func scroll(_ window: NSWindow?, at: Double, for secs: Double) {
        DispatchQueue.main.asyncAfter(deadline: .now() + at) {
            guard let scroller = findTable(window?.contentView) else {
                NSLog("FEV_APP_SCROLL: no table to scroll")
                return
            }
            let end = Date().addingTimeInterval(secs)
            let state = ScrollState(scroller)
            Timer.scheduledTimer(withTimeInterval: 1.0 / 60, repeats: true) { t in
                let more = Date() < end && MainActor.assumeIsolated { state.step() }
                if !more { t.invalidate() }
            }
        }
    }

    @MainActor final class ScrollState: @unchecked Sendable {
        let scroller: NSScrollView
        var down = true
        init(_ s: NSScrollView) { scroller = s }
        func step() -> Bool {
            guard let doc = scroller.documentView else { return false }
            let clip = scroller.contentView
            let maxY = max(0, doc.frame.height - clip.bounds.height)
            var y = clip.bounds.origin.y + (down ? 40 : -40)
            if y >= maxY { y = maxY; down = false }
            if y <= 0 { y = 0; down = true }
            clip.scroll(to: NSPoint(x: clip.bounds.origin.x, y: y))
            scroller.reflectScrolledClipView(clip)
            return true
        }
    }

    private static func findTable(_ view: NSView?) -> NSScrollView? {
        var all: [NSScrollView] = []
        func walk(_ v: NSView) {
            if let s = v as? NSScrollView, s.documentView is NSTableView { all.append(s) }
            v.subviews.forEach(walk)
        }
        if let view { walk(view) }
        return all.max { ($0.documentView?.frame.height ?? 0) < ($1.documentView?.frame.height ?? 0) }
    }
}
