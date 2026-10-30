import AppKit
import FevomDocker
import FevomStores
import SwiftUI

struct NetworksView: View {
    @Environment(AppModel.self) private var model
    @State private var selection: Set<DockerNetwork.ID> = []
    @State private var filter = ""
    @State private var creating = false
    @State private var newName = ""
    @State private var pendingDelete: DockerNetwork?

    var rows: [DockerNetwork] {
        let f = filter.trimmingCharacters(in: .whitespaces)
        guard !f.isEmpty else { return model.networks }
        return model.networks.filter { $0.name.localizedCaseInsensitiveContains(f) || $0.subnet.contains(f) || $0.driver.localizedCaseInsensitiveContains(f) }
    }

    var detailNetwork: DockerNetwork? {
        guard selection.count == 1, let id = selection.first else { return nil }
        return model.networks.first { $0.id == id }
    }

    var body: some View {
        let rows = rows
        ListDetail(idealDetail: 360, shown: detailNetwork != nil, widthKey: "detailWidth.networks") {
            Table(rows, selection: $selection) {
                TableColumn("Name") { n in
                    HStack(spacing: 10) {
                        EntityIcon(symbol: "point.3.connected.trianglepath.dotted", color: .teal, size: 26)
                        Text(n.name).fontWeight(.medium).lineLimit(1)
                    }
                    .padding(.vertical, 5)
                }
                .width(min: 110, ideal: 260)
                TableColumn("Subnet") { n in Text(n.subnet).font(.callout.monospacedDigit()).foregroundStyle(.secondary) }.width(min: 80, ideal: 150)
                TableColumn("Driver") { n in Text(n.driver) }.width(min: 50, ideal: 80)
                TableColumn("Scope") { n in Text(n.scope).foregroundStyle(.secondary) }.width(min: 40, ideal: 70)
            }
            .alternatingRowBackgrounds(.disabled)
            .contextMenu(forSelectionType: DockerNetwork.ID.self) { ids in
                if let id = ids.first, let n = model.networks.first(where: { $0.id == id }) {
                    Button("Copy Name") { copyToPasteboard(n.name) }
                    if !n.subnet.isEmpty { Button("Copy Subnet") { copyToPasteboard(n.subnet) } }
                    Button("Copy Command") { copyToPasteboard("docker network inspect \(n.name)") }
                    Divider()
                    Button("Delete…", role: .destructive) { pendingDelete = n }
                        .disabled(n.predefined)
                }
            }
        } detail: {
            if let n = detailNetwork {
                NetworkDetailView(network: n).id(n.id)
            }
        }
        .searchable(text: $filter, prompt: "Filter networks")
        .findFocus()
        .toolbar {
            Button("New Network", systemImage: "plus") { newName = ""; creating = true }
                .labelStyle(.titleAndIcon)
        }
        .sheet(isPresented: $creating) {
            NamePrompt(title: "New Network", label: "Name", name: $newName, actionTitle: "Create Network") {
                let n = newName
                Task { await model.createNetwork(n) }
            }
        }
        .onDeleteCommand {
            if let id = selection.first, let n = model.networks.first(where: { $0.id == id }), !n.predefined { pendingDelete = n }
        }
        .confirmationDialog("Delete the network \(pendingDelete?.name ?? "")?",
                            isPresented: Binding(get: { pendingDelete != nil }, set: { if !$0 { pendingDelete = nil } }), presenting: pendingDelete) { n in
            Button("Delete Network", role: .destructive) { Task { await model.removeNetwork(n.id) } }
            Button("Cancel", role: .cancel) {}
        } message: { n in
            Text("Docker refuses if a container is still connected to it.")
        }
        .overlay {
            if model.networks.isEmpty {
                WorkspaceEmptyState(symbol: "point.3.connected.trianglepath.dotted", title: "No networks")
            } else if rows.isEmpty {
                WorkspaceEmptyState(symbol: "magnifyingglass", title: "No matching networks")
            }
        }
        .workspacePage(summary: "\(model.networks.count) networks")
        .navigationTitle("Networks")
        .onAppear { model.watchList(.networks) }
        .onDisappear { model.unwatchList(.networks) }
        .onChange(of: model.networks.count) {
            if selection.isEmpty, DevHooks.value("FEV_APP_SELECT_FIRST") != nil, let f = model.networks.first { selection = [f.id] }
        }
    }
}

struct NetworkDetailView: View {
    @Environment(AppModel.self) private var model
    let network: DockerNetwork
    @State private var detail: DockerNetwork?

    var body: some View {
        let n = detail ?? network
        ScrollView {
            VStack(alignment: .leading, spacing: 14) {
                HStack(spacing: 12) {
                    EntityIcon(symbol: "point.3.connected.trianglepath.dotted", color: .teal, size: 40)
                    VStack(alignment: .leading, spacing: 3) {
                        Text(n.name).font(.headline).textSelection(.enabled)
                        Text(n.driver).font(.caption).foregroundStyle(.secondary)
                    }
                }
                .padding(.vertical, 4)
                InspectorSection(title: "Overview") {
                    value("ID", String(n.id.prefix(12)))
                    value("Created", created(n.created))
                    if !n.subnet.isEmpty { value("Subnet", n.subnet) }
                    if !n.gateway.isEmpty { value("Gateway", n.gateway) }
                    if !n.subnet6.isEmpty { value("IPv6 subnet", n.subnet6) }
                }
                InspectorSection(title: "Driver") {
                    value("Driver", n.driver)
                    value("Scope", n.scope)
                    value("Internal", n.isInternal ? "Yes" : "No")
                    value("Attachable", n.attachable ? "Yes" : "No")
                }
                if detail != nil {
                    InspectorSection(title: "Containers") {
                        if n.containers.isEmpty {
                            Text("None connected.").font(.callout).foregroundStyle(.secondary)
                        }
                        ForEach(n.containers, id: \.name) { m in value(m.name, m.ipv4.isEmpty ? m.ipv6 : m.ipv4) }
                    }
                }
                if !n.options.isEmpty {
                    InspectorSection(title: "Options") {
                        ForEach(n.options.sorted(by: { $0.key < $1.key }), id: \.key) { k, v in
                            VStack(alignment: .leading, spacing: 2) {
                                Text(k).font(.caption.monospaced()).foregroundStyle(.secondary).textSelection(.enabled)
                                Text(v).font(.callout.monospaced()).textSelection(.enabled)
                            }
                        }
                    }
                }
                if !n.labels.isEmpty {
                    InspectorSection(title: "Labels") {
                        ForEach(n.labels.sorted(by: { $0.key < $1.key }), id: \.key) { k, v in value(k, v) }
                    }
                }
            }
            .padding(18)
        }
        .background(WorkspaceStyle.canvas)
        .task(id: "\(network.id) \(model.networkEvents[network.id] ?? 0)") { detail = await model.networkDetail(network.id) }
    }

    private func created(_ s: String) -> String {
        guard let d = BuildTime.parse(s), d.timeIntervalSince1970 > 0 else { return "Unknown" }
        return d.formatted(date: .abbreviated, time: .shortened)
    }

    private func value(_ label: String, _ v: String) -> some View {
        HStack(alignment: .firstTextBaseline) {
            Text(label).foregroundStyle(.secondary).lineLimit(1)
            Spacer(minLength: 12)
            Text(v).font(.callout.monospacedDigit()).multilineTextAlignment(.trailing).textSelection(.enabled)
        }
        .font(.callout)
    }
}
