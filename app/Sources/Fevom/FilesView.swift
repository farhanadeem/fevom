import AppKit
import FevomAPI
import FevomStores
import QuickLook
import SwiftUI

struct FilesView: View {
    @Environment(AppModel.self) private var model
    @Environment(Navigation.self) private var nav
    @State private var browser: FileBrowser?
    @State private var selection: Set<FileBrowser.Entry.ID> = []
    @State private var preview: URL?
    @State private var filter = ""
    @State private var inSidebar: Bool?

    var rootPath: String? {
        if let p = model.status?.view?.path, !p.isEmpty { return p }
        return model.place.home == DaemonPlace.defaultHome ? DaemonPlace.defaultView : nil
    }

    var body: some View {
        Group {
            if let b = browser {
                listing(b)
            } else {
                WorkspaceEmptyState(symbol: "folder", title: "No files", message: model.status?.state == DaemonAPI.Const.stateRunning
                                        ? "This fev was started without a folder for its files (fevd --view-dir)."
                                        : "Start Fevom to browse its machines, containers, images and volumes.")
            }
        }
        .searchable(text: $filter, prompt: "Filter files")
        .findFocus()
        .workspacePage(summary: browser.map { "\($0.entries.count) items" } ?? "")
        .navigationTitle("Files")
        .onAppear { attach() }
        .onChange(of: rootPath) { attach() }
        .onChange(of: nav.filesPath) { if let p = nav.filesPath { browser?.go(p); nav.filesPath = nil } }
    }

    private func attach() {
        guard let p = rootPath else { browser = nil; return }
        if p == DaemonPlace.defaultView { inSidebar = FinderSidebar.contains(p) }
        if browser?.root.path != URL(fileURLWithPath: p).standardizedFileURL.path {
            browser = FileBrowser(root: URL(fileURLWithPath: p))
        }
        if let parts = nav.filesPath {
            nav.filesPath = nil
            browser?.go(parts)
        } else {
            browser?.reload()
        }
    }

    @ViewBuilder private func listing(_ b: FileBrowser) -> some View {
        let rows = b.entries.filter { filter.isEmpty || $0.name.localizedCaseInsensitiveContains(filter) }
        VStack(spacing: 0) {
            HStack(spacing: 8) {
                Button("Up", systemImage: "chevron.up") { b.up() }
                    .labelStyle(.iconOnly).disabled(b.atRoot).help("Enclosing folder")
                Button(b.root.lastPathComponent) { b.go([]) }.buttonStyle(.link)
                ForEach(Array(b.parts.enumerated()), id: \.offset) { i, part in
                    Text("›").foregroundStyle(.secondary)
                    Button(part) { b.go(Array(b.parts.prefix(i + 1))) }.buttonStyle(.link)
                }
                Spacer()
                if let v = model.status?.view, !v.state.isEmpty, v.state != "mounted" {
                    Text(v.error.isEmpty ? "~/Fevom is \(v.state)" : "~/Fevom is \(v.state): \(v.error)")
                        .font(.caption).foregroundStyle(.secondary)
                }
                Toggle("Hidden Files", isOn: Binding(get: { b.showHidden }, set: { b.showHidden = $0 })).toggleStyle(.checkbox)
                Button("Refresh", systemImage: "arrow.clockwise") { b.reload() }.labelStyle(.iconOnly).help("Reload")
                Button("Show in Finder", systemImage: "folder") { NSWorkspace.shared.activateFileViewerSelecting([b.directory]) }
                    .labelStyle(.iconOnly).help("Show this folder in Finder")
                if b.root.path == URL(fileURLWithPath: DaemonPlace.defaultView).standardizedFileURL.path, let on = inSidebar {
                    Button(on ? "Remove from Finder Sidebar" : "Add to Finder Sidebar", systemImage: on ? "sidebar.squares.left" : "sidebar.left") {
                        do {
                            if on { try FinderSidebar.remove(b.root.path) } else { try FinderSidebar.add(b.root.path) }
                        } catch {
                            model.lastError = String(describing: error)
                        }
                        inSidebar = FinderSidebar.contains(b.root.path)
                    }
                    .help(on ? "Remove ~/Fevom from Finder's Favorites" : "Add ~/Fevom to Finder's Favorites")
                }
            }
            .controlSize(.small)
            .padding(.horizontal, 20).padding(.vertical, 12)
            Divider()
            Table(rows, selection: $selection) {
                TableColumn("Name") { e in
                    HStack(spacing: 10) {
                        Image(systemName: e.isDirectory ? "folder.fill" : e.isLink ? "arrow.turn.up.right" : "doc")
                            .foregroundStyle(e.isDirectory ? Color.accentColor : .secondary)
                        Text(e.name)
                    }
                    .padding(.vertical, 7)
                        .accessibilityLabel("\(e.isDirectory ? "Folder" : "File") \(e.name)")
                }
                TableColumn("Size") { e in Text(e.isDirectory ? "" : ByteCountFormatter.string(fromByteCount: e.size, countStyle: .file)).monospacedDigit() }
                    .width(ideal: 90)
                TableColumn("Modified") { e in Text(e.modified.map { $0.formatted(date: .abbreviated, time: .shortened) } ?? "") }
                    .width(ideal: 160)
            }
            .alternatingRowBackgrounds(.disabled)
            .contextMenu(forSelectionType: FileBrowser.Entry.ID.self) { ids in
                if let id = ids.first, let e = b.entries.first(where: { $0.id == id }) {
                    if e.isDirectory { Button("Open") { b.open(e) } } else { Button("Quick Look") { look(b, e) } }
                    Button("Open with Default App") {
                        if let u = b.openable(e) { NSWorkspace.shared.open(u) } else { refuse(b, e) }
                    }
                    Button("Show in Finder") { NSWorkspace.shared.activateFileViewerSelecting([b.url(e)]) }
                    Divider()
                    Button("Copy Path") { copyToPasteboard(b.url(e).path) }
                }
            } primaryAction: { ids in
                if let id = ids.first, let e = b.entries.first(where: { $0.id == id }) {
                    if e.isDirectory { b.open(e) } else { look(b, e) }
                }
            }
            .onKeyPress(.space) {
                guard let id = selection.first, let e = b.entries.first(where: { $0.id == id }), !e.isDirectory else { return .ignored }
                look(b, e)
                return .handled
            }
            .quickLookPreview($preview)
            .overlay {
                if let error = b.error {
                    ContentUnavailableView("Cannot Read the Folder", systemImage: "exclamationmark.triangle", description: Text(error))
                } else if b.entries.isEmpty, !b.loading {
                    WorkspaceEmptyState(symbol: "folder", title: "This folder is empty")
                } else if rows.isEmpty, !b.loading {
                    WorkspaceEmptyState(symbol: "magnifyingglass", title: "No matching files")
                }
            }
        }
    }

    private func look(_ b: FileBrowser, _ e: FileBrowser.Entry) {
        if let u = b.openable(e) { preview = u } else { refuse(b, e) }
    }

    private func refuse(_ b: FileBrowser, _ e: FileBrowser.Entry) {
        model.lastError = "\(e.name) is a link to \(b.linkTarget(e)), which is outside Fevom's files: it is not opened on this Mac"
    }
}
