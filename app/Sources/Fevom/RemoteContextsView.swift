import AppKit
import FevomAPI
import FevomStores
import SwiftUI

struct RemoteContextsView: View {
    @Environment(AppModel.self) private var model
    @Environment(\.shell) private var shell
    @State private var selected: String?
    @State private var filter = ""

    var body: some View {
        Group {
            if let contexts = model.contexts, contexts.isEmpty {
                WorkspaceEmptyState(symbol: "network.slash", title: "No other Docker contexts",
                                    command: "docker context create NAME --docker host=ssh://HOST")
            } else if let contexts = model.contexts {
                content(contexts)
            } else {
                ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
            }
        }
        .workspacePage(summary: selected.map { "Context: \($0)" } ?? model.contexts.map { "\($0.count) contexts" } ?? "")
        .searchable(text: $filter, prompt: "Filter containers and images")
        .findFocus()
        .navigationTitle(SidebarItem.contexts.title)
        .toolbar {
            Button("Refresh", systemImage: "arrow.clockwise") {
                if let c = selected { Task { await model.loadContext(c) } }
            }
            .help("Reload")
            .disabled(selected == nil)
        }
        .task { await model.loadContexts(); pickDefault() }
        .task(id: selected) { if let c = selected { await model.loadContext(c) } }
    }

    private func pickDefault() {
        guard selected == nil, let contexts = model.contexts, !contexts.isEmpty else { return }
        selected = contexts.first(where: { $0.current })?.name ?? contexts.first?.name
    }

    @ViewBuilder
    private func content(_ contexts: [DaemonAPI.RemoteContext]) -> some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack(spacing: 10) {
                Picker("Context", selection: Binding(get: { selected ?? contexts.first?.name ?? "" }, set: { selected = $0 })) {
                    ForEach(contexts, id: \.name) { c in
                        Text(c.current ? "\(c.name) (current)" : c.name).tag(c.name)
                    }
                }
                .pickerStyle(.menu).fixedSize()
                if let c = contexts.first(where: { $0.name == selected }), !c.host.isEmpty {
                    Text(c.host).font(.caption.monospaced()).foregroundStyle(.secondary).textSelection(.enabled)
                }
                Spacer()
            }
            .padding(.horizontal, WorkspaceStyle.inset).padding(.vertical, 12)

            if let name = selected, let err = model.contextErrors[name] {
                Banner(text: err, color: .red) { model.dismissContextError(name) }
            }

            List {
                if let name = selected {
                    Section("Containers") {
                        let cs = (model.contextContainers[name] ?? []).filter {
                            filter.isEmpty || $0.name.localizedCaseInsensitiveContains(filter) || $0.image.localizedCaseInsensitiveContains(filter)
                        }
                        if cs.isEmpty {
                            Text(filter.isEmpty ? "No containers" : "No matching containers").foregroundStyle(.secondary)
                        } else {
                            ForEach(cs, id: \.id) { c in containerRow(name, c) }
                        }
                    }
                    Section("Images") {
                        let ims = (model.contextImages[name] ?? []).filter {
                            filter.isEmpty || $0.repoTags.contains { $0.localizedCaseInsensitiveContains(filter) }
                        }
                        if ims.isEmpty {
                            Text(filter.isEmpty ? "No images" : "No matching images").foregroundStyle(.secondary)
                        } else {
                            ForEach(ims, id: \.id) { i in imageRow(name, i) }
                        }
                    }
                }
            }
            .listStyle(.inset)
        }
    }

    @ViewBuilder
    private func containerRow(_ context: String, _ c: DaemonAPI.RemoteContainer) -> some View {
        HStack(spacing: 10) {
            EntityIcon(symbol: "shippingbox", size: 24)
            VStack(alignment: .leading, spacing: 1) {
                Text(c.name.isEmpty ? String(c.id.prefix(12)) : c.name).fontWeight(.medium)
                Text("\(c.image) · \(c.status.isEmpty ? c.state : c.status)").font(.caption).foregroundStyle(.secondary)
            }
            Spacer()
            debugButton(target: c.id, kind: "container", context: context, name: c.name.isEmpty ? c.id : c.name)
        }
        .padding(.vertical, 3)
        .contextMenu {
            debugMenuItems(target: c.id, kind: "container", context: context, name: c.name.isEmpty ? c.id : c.name)
        }
    }

    @ViewBuilder
    private func imageRow(_ context: String, _ i: DaemonAPI.RemoteImage) -> some View {
        let tag = i.repoTags.first ?? String(i.id.replacingOccurrences(of: "sha256:", with: "").prefix(12))
        HStack(spacing: 10) {
            EntityIcon(symbol: "square.stack.3d.up", size: 24)
            VStack(alignment: .leading, spacing: 1) {
                Text(tag).fontWeight(.medium)
                if i.size > 0 {
                    Text(ByteCountFormatter.string(fromByteCount: i.size, countStyle: .file)).font(.caption).foregroundStyle(.secondary)
                }
            }
            Spacer()
            debugButton(target: tag, kind: "image", context: context, name: tag)
        }
        .padding(.vertical, 3)
        .contextMenu {
            debugMenuItems(target: tag, kind: "image", context: context, name: tag)
        }
    }

    private func debugButton(target: String, kind: String, context: String, name: String) -> some View {
        Button("Debug Shell", systemImage: "ladybug") {
            shell.openDebugShell(target, kind, false, context, name)
        }
        .buttonStyle(.borderless)
        .help("Open a shell with debugging tools inside this \(kind) on \(context), without changing it")
    }

    @ViewBuilder
    private func debugMenuItems(target: String, kind: String, context: String, name: String) -> some View {
        Button("Debug Shell") { shell.openDebugShell(target, kind, false, context, name) }
        Button("Open Debug Shell in Terminal App") { try? ExternalTerminal.debug(target: target, context: context) }
        Divider()
        Button("Copy Name") { copyToPasteboard(name) }
    }
}
