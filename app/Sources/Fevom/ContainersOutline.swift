import AppKit
import FevomDocker
import FevomStores
import SwiftUI

private final class OutlineStatusCell: NSTableCellView {
    var state = "" {
        didSet {
            textField?.stringValue = StatusAppearance.title(state)
            updateForeground()
            needsDisplay = true
        }
    }

    init(identifier: NSUserInterfaceItemIdentifier) {
        super.init(frame: .zero)
        self.identifier = identifier
        let text = NSTextField(labelWithString: "")
        text.font = .systemFont(ofSize: 11, weight: .medium)
        text.translatesAutoresizingMaskIntoConstraints = false
        text.lineBreakMode = .byTruncatingTail
        addSubview(text)
        textField = text
        NSLayoutConstraint.activate([
            text.leadingAnchor.constraint(equalTo: leadingAnchor, constant: 9),
            text.trailingAnchor.constraint(lessThanOrEqualTo: trailingAnchor, constant: -9),
            text.centerYAnchor.constraint(equalTo: centerYAnchor),
        ])
    }

    required init?(coder: NSCoder) { nil }

    override var backgroundStyle: NSView.BackgroundStyle {
        didSet { updateForeground(); needsDisplay = true }
    }

    private func updateForeground() {
        textField?.textColor = backgroundStyle == .emphasized ? .white : StatusAppearance.color(state)
    }

    override func draw(_ dirtyRect: NSRect) {
        super.draw(dirtyRect)
        let width = min(bounds.width, (textField?.intrinsicContentSize.width ?? 0) + 18)
        let rect = NSRect(x: 0, y: (bounds.height - 23) / 2, width: width, height: 23)
        let color = backgroundStyle == .emphasized ? NSColor.white : StatusAppearance.color(state)
        color.withAlphaComponent(0.09).setFill()
        NSBezierPath(roundedRect: rect, xRadius: 7, yRadius: 7).fill()
    }
}

enum MenuEntry {
    case action(String, () -> Void)
    case divider
}

struct ContainersOutline: NSViewRepresentable {
    final class Rows {
        let rows: [ContainerRow]
        init(_ rows: [ContainerRow]) { self.rows = rows }
    }

    let rows: Rows
    @Binding var selection: Set<ContainerRow.ID>
    var menu: (ContainerRow) -> [MenuEntry]
    var primary: (ContainerRow) -> Void
    var delete: (ContainerRow) -> Void

    enum Column: String, CaseIterable {
        case name = "Name", state = "Status", image = "Image", ports = "Ports"
        var width: CGFloat {
            switch self {
            case .name: 220
            case .image: 180
            case .state: 110
            case .ports: 120
            }
        }
    }

    func makeCoordinator() -> Coordinator { Coordinator() }

    func makeNSView(context: Context) -> NSScrollView {
        let outline = ContainersOutlineView()
        for c in Column.allCases {
            let col = NSTableColumn(identifier: NSUserInterfaceItemIdentifier(c.rawValue))
            col.title = c.rawValue
            col.width = c.width
            col.minWidth = c == .state ? 100 : c == .name ? 140 : c == .image ? 100 : 60
            col.resizingMask = [.userResizingMask, .autoresizingMask]
            outline.addTableColumn(col)
            if c == .name { outline.outlineTableColumn = col }
        }
        outline.style = .inset
        outline.usesAlternatingRowBackgroundColors = false
        outline.rowHeight = 42
        outline.intercellSpacing = NSSize(width: 8, height: 0)
        outline.backgroundColor = .controlBackgroundColor
        outline.allowsMultipleSelection = true
        outline.columnAutoresizingStyle = .uniformColumnAutoresizingStyle
        outline.autosaveExpandedItems = false
        outline.indentationPerLevel = 14
        outline.setAccessibilityLabel("Containers")
        let c = context.coordinator
        c.outline = outline
        outline.dataSource = c
        outline.delegate = c
        outline.target = c
        outline.doubleAction = #selector(Coordinator.doubleClicked)
        let m = NSMenu()
        m.delegate = c
        outline.menu = m
        outline.onDelete = { [weak c] in c?.deleteSelected() }
        let scroll = NSScrollView()
        scroll.documentView = outline
        scroll.hasVerticalScroller = true
        scroll.hasHorizontalScroller = true
        scroll.autohidesScrollers = true
        scroll.backgroundColor = .controlBackgroundColor
        return scroll
    }

    func updateNSView(_ view: NSScrollView, context: Context) {
        let c = context.coordinator
        c.parent = self
        c.apply(rows.rows)
        c.select(selection)
    }

    final class Node: NSObject {
        let id: String
        var row: ContainerRow
        var kids: [Node] = []
        init(_ row: ContainerRow) {
            id = row.id
            self.row = row
        }
    }

    @MainActor
    final class Coordinator: NSObject, NSOutlineViewDataSource, NSOutlineViewDelegate, NSMenuDelegate {
        var parent: ContainersOutline?
        weak var outline: NSOutlineView?
        var top: [Node] = []
        var byID: [String: Node] = [:]
        var expanded: Set<String> = []
        var applying = false

        private(set) var fullReloads = 0
        private(set) var rowReloads = 0
        static weak var current: Coordinator?
        var expandAll = DevHooks.value("FEV_APP_EXPAND_ALL") == "1"

        var state: [String: Any] {
            ["rows": outline?.numberOfRows ?? 0, "full_reloads": fullReloads, "row_reloads": rowReloads,
             "expanded": expanded.sorted(), "selected": selectedIDs().sorted()]
        }

        func apply(_ rows: [ContainerRow]) {
            guard let outline else { return }
            Self.current = self
            let shape = { (rs: [ContainerRow]) in rs.map { r in [r.id] + (r.children ?? []).map(\.id) } }
            let sameShape = shape(rows) == top.map { n in [n.id] + n.kids.map(\.id) }
            var changed: [Node] = []
            var next: [String: Node] = [:]
            func node(_ r: ContainerRow) -> Node {
                let n: Node
                if let old = byID[r.id] {
                    n = old
                    if old.row != r { changed.append(old) }
                    old.row = r
                } else {
                    n = Node(r)
                }
                n.kids = (r.children ?? []).map(node)
                next[r.id] = n
                return n
            }
            let newTop = rows.map(node)
            byID = next
            top = newTop
            applying = true
            defer { applying = false }
            if sameShape {
                if changed.isEmpty { return }
                var idx = IndexSet()
                for n in changed {
                    let r = outline.row(forItem: n)
                    if r >= 0 { idx.insert(r) }
                }
                if !idx.isEmpty {
                    outline.reloadData(forRowIndexes: idx, columnIndexes: IndexSet(0..<outline.numberOfColumns))
                    rowReloads += idx.count
                }
                return
            }
            let selected = selectedIDs()
            outline.reloadData()
            fullReloads += 1
            expanded = expanded.filter { byID[$0] != nil }
            if expandAll && !top.isEmpty {
                expandAll = false
                for n in top where !n.kids.isEmpty { expanded.insert(n.id) }
            }
            for id in expanded { if let n = byID[id] { outline.expandItem(n) } }
            selectRows(selected)
        }

        func selectedIDs() -> Set<String> {
            guard let outline else { return [] }
            return Set(outline.selectedRowIndexes.compactMap { (outline.item(atRow: $0) as? Node)?.id })
        }

        func select(_ ids: Set<String>) {
            guard ids != selectedIDs() else { return }
            applying = true
            defer { applying = false }
            selectRows(ids)
        }

        private func selectRows(_ ids: Set<String>) {
            guard let outline else { return }
            var idx = IndexSet()
            for id in ids {
                if let n = byID[id] {
                    let r = outline.row(forItem: n)
                    if r >= 0 { idx.insert(r) }
                }
            }
            outline.selectRowIndexes(idx, byExtendingSelection: false)
        }

        func outlineView(_ outlineView: NSOutlineView, numberOfChildrenOfItem item: Any?) -> Int {
            (item as? Node)?.kids.count ?? top.count
        }

        func outlineView(_ outlineView: NSOutlineView, child index: Int, ofItem item: Any?) -> Any {
            (item as? Node)?.kids[index] ?? top[index]
        }

        func outlineView(_ outlineView: NSOutlineView, isItemExpandable item: Any) -> Bool {
            !((item as? Node)?.kids.isEmpty ?? true)
        }

        func outlineViewItemDidExpand(_ notification: Notification) {
            if let n = notification.userInfo?["NSObject"] as? Node { expanded.insert(n.id) }
        }

        func outlineViewItemDidCollapse(_ notification: Notification) {
            if !applying, let n = notification.userInfo?["NSObject"] as? Node { expanded.remove(n.id) }
        }

        func outlineViewSelectionDidChange(_ notification: Notification) {
            guard !applying, let parent else { return }
            let ids = selectedIDs()
            if parent.selection != ids { parent.selection = ids }
        }

        func outlineView(_ outlineView: NSOutlineView, viewFor tableColumn: NSTableColumn?, item: Any) -> NSView? {
            guard let n = item as? Node, let tableColumn, let col = Column(rawValue: tableColumn.identifier.rawValue) else { return nil }
            let r = n.row
            let c = r.container
            if col == .name {
                let cell = outlineView.makeView(withIdentifier: tableColumn.identifier, owner: nil) as? NSTableCellView ?? Self.nameCell(tableColumn.identifier)
                let symbol = r.project != nil ? "square.stack.3d.up" : "shippingbox"
                cell.imageView?.image = NSImage(systemSymbolName: symbol, accessibilityDescription: nil)
                cell.textField?.stringValue = r.name
                let live = r.state == "running" || r.project != nil
                cell.textField?.textColor = live ? .labelColor : .secondaryLabelColor
                cell.imageView?.contentTintColor = r.project != nil ? .controlAccentColor : .secondaryLabelColor
                cell.setAccessibilityLabel("\(r.project != nil ? "Project" : "Container") \(r.name), \(r.state)")
                return cell
            }
            if col == .state {
                let cell = outlineView.makeView(withIdentifier: tableColumn.identifier, owner: nil) as? OutlineStatusCell ?? OutlineStatusCell(identifier: tableColumn.identifier)
                cell.state = r.state
                cell.toolTip = c?.status
                return cell
            }
            let text: String
            switch col {
            case .image: text = c?.image ?? ""
            case .state: text = c.map { $0.status.isEmpty ? $0.state : $0.status } ?? r.state
            case .ports: text = c.map(Self.ports) ?? ""
            case .name: text = ""
            }
            let cell = outlineView.makeView(withIdentifier: tableColumn.identifier, owner: nil) as? NSTableCellView ?? Self.textCell(tableColumn.identifier, col)
            cell.textField?.stringValue = text
            return cell
        }

        static func nameCell(_ id: NSUserInterfaceItemIdentifier) -> NSTableCellView {
            let cell = NSTableCellView()
            cell.identifier = id
            let image = NSImageView()
            image.translatesAutoresizingMaskIntoConstraints = false
            let text = NSTextField(labelWithString: "")
            text.translatesAutoresizingMaskIntoConstraints = false
            text.font = .systemFont(ofSize: 13, weight: .medium)
            text.lineBreakMode = .byTruncatingTail
            cell.addSubview(image)
            cell.addSubview(text)
            cell.imageView = image
            cell.textField = text
            NSLayoutConstraint.activate([
                image.leadingAnchor.constraint(equalTo: cell.leadingAnchor, constant: 2),
                image.centerYAnchor.constraint(equalTo: cell.centerYAnchor),
                image.widthAnchor.constraint(equalToConstant: 16),
                text.leadingAnchor.constraint(equalTo: image.trailingAnchor, constant: 5),
                text.trailingAnchor.constraint(lessThanOrEqualTo: cell.trailingAnchor, constant: -2),
                text.centerYAnchor.constraint(equalTo: cell.centerYAnchor),
            ])
            return cell
        }

        static func textCell(_ id: NSUserInterfaceItemIdentifier, _ col: Column) -> NSTableCellView {
            let cell = NSTableCellView()
            cell.identifier = id
            let f = NSTextField(labelWithString: "")
            f.translatesAutoresizingMaskIntoConstraints = false
            f.lineBreakMode = .byTruncatingTail
            f.font = col == .ports ? .monospacedDigitSystemFont(ofSize: 12, weight: .regular) : .systemFont(ofSize: 12)
            f.textColor = .secondaryLabelColor
            cell.addSubview(f)
            cell.textField = f
            NSLayoutConstraint.activate([
                f.leadingAnchor.constraint(equalTo: cell.leadingAnchor, constant: 2),
                f.trailingAnchor.constraint(lessThanOrEqualTo: cell.trailingAnchor, constant: -2),
                f.centerYAnchor.constraint(equalTo: cell.centerYAnchor),
            ])
            return cell
        }

        static func ports(_ c: Container) -> String {
            Set(c.ports.compactMap { p in p.publicPort.map { "\($0)→\(p.privatePort)/\(p.type)" } }).sorted().joined(separator: ", ")
        }

        func targetRow() -> ContainerRow? {
            guard let outline else { return nil }
            let r = outline.clickedRow >= 0 ? outline.clickedRow : outline.selectedRow
            return (outline.item(atRow: r) as? Node)?.row
        }

        @objc func doubleClicked() {
            guard let outline, outline.clickedRow >= 0, let row = targetRow() else { return }
            parent?.primary(row)
        }

        func deleteSelected() {
            guard let outline, let row = (outline.item(atRow: outline.selectedRow) as? Node)?.row else { return }
            parent?.delete(row)
        }

        func menuNeedsUpdate(_ menu: NSMenu) {
            menu.removeAllItems()
            guard let row = targetRow(), let parent else { return }
            for e in parent.menu(row) {
                switch e {
                case .divider: menu.addItem(.separator())
                case .action(let title, let run): menu.addItem(ClosureMenuItem(title, run))
                }
            }
        }
    }
}

final class ContainersOutlineView: NSOutlineView {
    var onDelete: () -> Void = {}
    override func keyDown(with event: NSEvent) {
        if event.charactersIgnoringModifiers == "\u{7f}" || event.specialKey == .deleteForward { onDelete() } else { super.keyDown(with: event) }
    }
    @objc func delete(_ sender: Any?) { onDelete() }
}

final class ClosureMenuItem: NSMenuItem {
    let run: () -> Void
    init(_ title: String, _ run: @escaping () -> Void) {
        self.run = run
        super.init(title: title, action: #selector(fire), keyEquivalent: "")
        target = self
    }
    @available(*, unavailable) required init(coder: NSCoder) { fatalError() }
    @objc func fire() { run() }
}
