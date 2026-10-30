import FevomAPI
import FevomDocker
import FevomStores
import SwiftUI

struct ActivityView: View {
    @Environment(AppModel.self) private var model
    @State private var filter = ""
    @State private var selection: Set<Activity.Row.ID> = []

    var body: some View {
        let a = model.activity
        let rows = filtered(a.rows(containers: model.containers))
        VStack(spacing: 0) {
            if model.status?.state == DaemonAPI.Const.stateRunning || !a.history.isEmpty {
                Graphs(activity: a).padding(20)
                Table(rows, children: \.children, selection: $selection) {
                    TableColumn("Name") { r in
                        Label(r.name, systemImage: Self.symbol(r.kind))
                            .accessibilityLabel("\(r.kind.rawValue) \(r.name)")
                    }
                    .width(min: 160, ideal: 220)
                    TableColumn("CPU") { r in Text(Fmt.percent(r.cpuPercent)).monospacedDigit() }
                        .width(65)
                    TableColumn("Memory") { r in Text(Fmt.bytes(r.memoryBytes)).monospacedDigit() }
                        .width(90)
                    TableColumn("Network in / out") { r in Text("\(Fmt.rate(r.netRx)) / \(Fmt.rate(r.netTx))").monospacedDigit() }
                    TableColumn("Disk read / write") { r in Text("\(Fmt.rate(r.diskRead)) / \(Fmt.rate(r.diskWrite))").monospacedDigit() }
                    TableColumn("Processes") { r in Text("\(r.pids)").monospacedDigit() }
                        .width(70)
                }
                .alternatingRowBackgrounds(.disabled)
                .contextMenu(forSelectionType: Activity.Row.ID.self) { _ in
                    Button("Copy Command") { copyToPasteboard("docker stats") }
                }
                .transaction { $0.disablesAnimations = true; $0.animation = nil }
            } else {
                WorkspaceEmptyState(symbol: "chart.xyaxis.line", title: "No activity",
                                    message: "Start Fevom to see its CPU, memory, network and disk use.")
            }
        }
        .workspacePage(summary: "Last five minutes")
        .searchable(text: $filter, prompt: "Filter activity")
        .navigationTitle("Activity")
        .onAppear { model.watchStats() }
        .onDisappear { model.unwatchStats() }
    }

    func filtered(_ rows: [Activity.Row]) -> [Activity.Row] {
        guard !filter.isEmpty else { return rows }
        return rows.compactMap { r in
            if r.name.localizedCaseInsensitiveContains(filter) { return r }
            guard let kids = r.children?.filter({ $0.name.localizedCaseInsensitiveContains(filter) }), !kids.isEmpty else { return nil }
            var p = r
            p.children = kids
            return p
        }
    }

    static func symbol(_ k: Activity.Row.Kind) -> String {
        switch k {
        case .system: "cpu"
        case .service: "gearshape"
        case .project: "square.stack"
        case .container: "shippingbox"
        case .machine: "desktopcomputer"
        }
    }
}

struct Graphs: View {
    let activity: Activity

    var body: some View {
        let h = activity.history
        Grid(horizontalSpacing: 12, verticalSpacing: 8) {
            GridRow {
                LineGraph(title: "CPU", now: Fmt.percent(h.last?.cpuPercent), unit: "%",
                          series: [.init(name: "CPU", color: .systemBlue, values: h.map(\.cpuPercent))])
                LineGraph(title: "Memory", now: Fmt.bytes(h.last?.memoryBytes ?? 0) + (activity.footprintBytes.map { ", on the Mac \(Fmt.bytes($0))" } ?? ""), unit: "B",
                          series: [.init(name: "Linux", color: .systemBlue, values: h.map { Double($0.memoryBytes) }),
                                   .init(name: "On the Mac", color: .systemGreen, values: h.map { Double($0.footprintBytes) })])
            }
            GridRow {
                LineGraph(title: "Network", now: "\(Fmt.rate(h.last?.netRx)) in, \(Fmt.rate(h.last?.netTx)) out", unit: "B/s",
                          series: [.init(name: "In", color: .systemBlue, values: h.map(\.netRx)), .init(name: "Out", color: .systemGreen, values: h.map(\.netTx))])
                LineGraph(title: "Disk", now: "\(Fmt.rate(h.last?.diskRead)) read, \(Fmt.rate(h.last?.diskWrite)) written", unit: "B/s",
                          series: [.init(name: "Read", color: .systemBlue, values: h.map(\.diskRead)), .init(name: "Write", color: .systemGreen, values: h.map(\.diskWrite))])
            }
        }
        .frame(height: 280)
    }
}

struct LineGraph: View {
    struct Series: Equatable {
        var name: String
        var color: NSColor
        var values: [Double]
    }

    let title: String
    let now: String
    let unit: String
    let series: [Series]

    var body: some View {
        let top = Self.top(series.flatMap(\.values).max() ?? 0, unit: unit)
        VStack(alignment: .leading, spacing: 9) {
            Text(title).font(.caption.weight(.medium)).foregroundStyle(.secondary)
            Text(now).font(.system(size: 14, weight: .semibold)).monospacedDigit().lineLimit(1).minimumScaleFactor(0.8)
            ZStack(alignment: .topTrailing) {
                GraphView(series: series, top: top)
                Text(Fmt.axis(top, unit: unit)).font(.caption2).foregroundStyle(.secondary).padding(.trailing, 2)
            }
            if series.count > 1 {
                HStack(spacing: 10) {
                    ForEach(series, id: \.name) { s in
                        HStack(spacing: 4) {
                            Circle().fill(Color(nsColor: s.color)).frame(width: 7, height: 7)
                            Text(s.name).font(.caption).foregroundStyle(.secondary)
                        }
                    }
                }
            }
        }
        .padding(14)
        .background(WorkspaceStyle.surface, in: RoundedRectangle(cornerRadius: 10))
        .overlay(RoundedRectangle(cornerRadius: 10).strokeBorder(WorkspaceStyle.border, lineWidth: 0.5))
        .accessibilityElement(children: .ignore)
        .accessibilityLabel("\(title) over the last five minutes")
        .accessibilityValue(now)
    }

    static func top(_ m: Double, unit: String) -> Double {
        if unit == "%" { return max(10, (m / 10).rounded(.up) * 10) }
        guard m > 0 else { return 1024 }
        return max(1024, pow(2, log2(m).rounded(.up)))
    }
}

struct GraphView: NSViewRepresentable {
    let series: [LineGraph.Series]
    let top: Double

    func makeNSView(context: Context) -> Plot { Plot() }

    func updateNSView(_ v: Plot, context: Context) {
        guard v.series != series || v.top != top else { return }
        v.series = series
        v.top = top
        v.needsDisplay = true
    }

    final class Plot: NSView {
        var series: [LineGraph.Series] = []
        var top: Double = 1

        override var isFlipped: Bool { true }

        override func draw(_ dirtyRect: NSRect) {
            let b = bounds
            let grid = NSBezierPath()
            for i in 0...2 {
                let y = b.height * CGFloat(i) / 2
                grid.move(to: NSPoint(x: 0, y: y))
                grid.line(to: NSPoint(x: b.width, y: y))
            }
            NSColor.secondaryLabelColor.withAlphaComponent(0.25).setStroke()
            grid.lineWidth = 0.5
            grid.stroke()
            let n = Activity.historyLimit
            let step = b.width / CGFloat(n - 1)
            for s in series where s.values.count > 1 {
                let x0 = b.width - step * CGFloat(s.values.count - 1)
                let p = NSBezierPath()
                for (i, v) in s.values.enumerated() {
                    let pt = NSPoint(x: x0 + step * CGFloat(i), y: b.height * (1 - CGFloat(min(max(v, 0) / top, 1))))
                    if i == 0 { p.move(to: pt) } else { p.line(to: pt) }
                }
                p.lineWidth = 1.5
                p.lineJoinStyle = .round
                s.color.setStroke()
                p.stroke()
            }
        }
    }
}

enum Fmt {
    static func percent(_ v: Double?) -> String {
        guard let v else { return "—" }
        return v < 10 ? String(format: "%.1f%%", v) : String(format: "%.0f%%", v)
    }

    static func bytes(_ v: UInt64) -> String { size(Double(v), .memory) }

    static func rate(_ v: Double?) -> String {
        guard let v else { return "—" }
        return size(v, .binary) + "/s"
    }

    static func axis(_ v: Double, unit: String) -> String {
        switch unit {
        case "%": return String(format: "%.0f%%", v)
        case "B": return size(v, .memory)
        default: return size(v, .binary)
        }
    }

    static func size(_ v: Double, _ style: ByteCountFormatter.CountStyle) -> String {
        let f = ByteCountFormatter()
        f.countStyle = style
        f.allowsNonnumericFormatting = false
        return f.string(fromByteCount: Int64(max(0, v)))
    }
}
