import AppKit
import FevomAPI
import FevomStores
import SwiftUI

@MainActor
final class LogWindowController: NSObject, NSWindowDelegate {
    let feed: LogFeed
    let window: NSWindow
    var onClose: () -> Void = {}

    init(feed: LogFeed) {
        self.feed = feed
        window = NSWindow(contentViewController: NSHostingController(rootView: LogPane(feed: feed).frame(minWidth: 520, minHeight: 300)))
        super.init()
        window.title = "Logs: \(feed.title)"
        window.setContentSize(NSSize(width: 900, height: 520))
        window.isReleasedWhenClosed = false
        window.isRestorable = false
        window.delegate = self
    }

    func show() {
        feed.start()
        DevHooks.show(window, regular: false)
    }

    func windowWillClose(_ notification: Notification) {
        feed.stop()
        onClose()
    }
}

struct LogPane: View {
    @Bindable var feed: LogFeed
    @State private var wrap = false
    @State private var timestamps = false
    @State private var follow = true

    var body: some View {
        VStack(spacing: 0) {
            HStack(spacing: 12) {
                Image(systemName: "text.alignleft").foregroundStyle(.secondary)
                TextField("Filter log lines", text: $feed.filterText)
                    .textFieldStyle(.roundedBorder)
                    .frame(maxWidth: 280)
                    .accessibilityLabel("Filter the log")
                Toggle("Wrap", isOn: $wrap).toggleStyle(.checkbox)
                Toggle("Timestamps", isOn: $timestamps).toggleStyle(.checkbox)
                Toggle("Follow", isOn: $follow).toggleStyle(.checkbox)
                    .help("Keep the newest line in view")
                Spacer()
                Button("Clear") { feed.clear() }
                    .help("Clear the view")
            }
            .controlSize(.small)
            .padding(.horizontal, 16).padding(.vertical, 12)
            Divider()
            LogTable(feed: feed, version: feed.version, wrap: wrap, timestamps: timestamps, follow: follow)
            Divider()
            ListSummary(text: statusLine)
            if let why = feed.ended {
                Text(why).font(.caption).foregroundStyle(.red).padding(6).frame(maxWidth: .infinity, alignment: .leading)
            }
        }
    }

    var statusLine: String {
        let rows = feed.rowCount
        let held = feed.buffer.count
        return feed.filterText.isEmpty ? "\(held) lines" : "\(rows) of \(held) lines"
    }
}

struct LogTable: NSViewRepresentable {
    let feed: LogFeed
    let version: Int
    let wrap: Bool
    let timestamps: Bool
    let follow: Bool

    func makeCoordinator() -> Coordinator { Coordinator(feed: feed) }

    func makeNSView(context: Context) -> NSScrollView {
        let table = CopyTableView()
        let column = NSTableColumn(identifier: .init("line"))
        column.resizingMask = .autoresizingMask
        table.addTableColumn(column)
        table.headerView = nil
        table.usesAlternatingRowBackgroundColors = false
        table.allowsMultipleSelection = true
        table.style = .plain
        table.intercellSpacing = NSSize(width: 0, height: 0)
        table.columnAutoresizingStyle = .uniformColumnAutoresizingStyle
        table.rowHeight = Coordinator.lineHeight
        table.dataSource = context.coordinator
        table.delegate = context.coordinator
        table.copyLines = { [weak coordinator = context.coordinator] in coordinator?.copySelection() }
        table.setAccessibilityLabel("Log lines")
        context.coordinator.table = table
        let scroll = NSScrollView()
        scroll.documentView = table
        scroll.hasVerticalScroller = true
        scroll.hasHorizontalScroller = false
        scroll.autohidesScrollers = true
        context.coordinator.scroll = scroll
        NotificationCenter.default.addObserver(context.coordinator, selector: #selector(Coordinator.resized), name: NSView.frameDidChangeNotification, object: scroll.contentView)
        scroll.contentView.postsFrameChangedNotifications = true
        return scroll
    }

    func updateNSView(_ view: NSScrollView, context: Context) {
        let c = context.coordinator
        let settings = c.wrap != wrap || c.timestamps != timestamps
        c.wrap = wrap
        c.timestamps = timestamps
        c.follow = follow
        c.reload(all: settings || c.lastVersion > version)
        c.lastVersion = version
    }

    @MainActor
    final class Coordinator: NSObject, NSTableViewDataSource, NSTableViewDelegate {
        let feed: LogFeed
        weak var table: NSTableView?
        weak var scroll: NSScrollView?
        var wrap = false
        var timestamps = false
        var follow = true
        var lastVersion = 0
        var shownRows = 0

        static let font = NSFont.monospacedSystemFont(ofSize: 11, weight: .regular)
        static let lineHeight: CGFloat = ceil(font.ascender - font.descender + font.leading) + 2
        static let cellWidth: CGFloat = ("M" as NSString).size(withAttributes: [.font: font]).width

        init(feed: LogFeed) { self.feed = feed }

        var lastFirst = 0
        var lastFilter = ""

        func reload(all: Bool) {
            guard let table else { return }
            let n = feed.rowCount
            let first = feed.buffer.firstSeq
            if all || first != lastFirst || feed.filterText != lastFilter || n < shownRows {
                table.reloadData()
            } else if n > shownRows {
                table.insertRows(at: IndexSet(shownRows..<n), withAnimation: [])
            }
            shownRows = n
            lastFirst = first
            lastFilter = feed.filterText
            if follow && n > 0 { table.scrollRowToVisible(n - 1) }
        }

        @objc func resized() {
            guard wrap, let table else { return }
            table.noteHeightOfRows(withIndexesChanged: IndexSet(0..<table.numberOfRows))
        }

        func numberOfRows(in tableView: NSTableView) -> Int { feed.rowCount }

        func tableView(_ tableView: NSTableView, heightOfRow row: Int) -> CGFloat {
            guard wrap else { return Self.lineHeight }
            let cols = max(1, Int((tableView.bounds.width - 8) / Self.cellWidth))
            let cells = feed.cells(row: row, timestamps: timestamps)
            return Self.lineHeight * CGFloat(max(1, (max(cells, 1) + cols - 1) / cols))
        }

        func tableView(_ tableView: NSTableView, viewFor tableColumn: NSTableColumn?, row: Int) -> NSView? {
            let id = NSUserInterfaceItemIdentifier("cell")
            let cell = (tableView.makeView(withIdentifier: id, owner: nil) as? NSTextField) ?? {
                let f = NSTextField(labelWithString: "")
                f.identifier = id
                f.font = Self.font
                f.isSelectable = false
                f.lineBreakMode = .byClipping
                return f
            }()
            cell.maximumNumberOfLines = wrap ? 0 : 1
            cell.cell?.wraps = wrap
            cell.lineBreakMode = wrap ? .byCharWrapping : .byClipping
            let d = feed.display(row: row, timestamps: timestamps)
            cell.stringValue = d.more > 0 ? d.text + " … (\(d.more) more bytes; copy takes the whole line)" : d.text
            cell.setAccessibilityLabel(d.text)
            return cell
        }

        func copySelection() {
            guard let table else { return }
            let lines = table.selectedRowIndexes.map { feed.line(row: $0, timestamps: timestamps) }
            guard !lines.isEmpty else { return }
            NSPasteboard.general.clearContents()
            NSPasteboard.general.setString(lines.joined(separator: "\n"), forType: .string)
        }
    }
}

final class CopyTableView: NSTableView {
    var copyLines: () -> Void = {}
    @objc func copy(_ sender: Any?) { copyLines() }
}
