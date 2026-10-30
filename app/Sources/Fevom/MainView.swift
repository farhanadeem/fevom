import AppKit
import FevomAPI
import FevomDocker
import FevomStores
import SwiftUI

enum SidebarItem: String, CaseIterable, Identifiable {
    case containers = "Containers"
    case images = "Images"
    case volumes = "Volumes"
    case machines = "Machines"
    case files = "Files"
    case contexts = "Remote"
    case activity = "Activity"
    case kubernetes = "Kubernetes"
    case builds = "Builds"
    case networks = "Networks"
    var id: String { rawValue }
    var title: String { self == .contexts ? "Remote Contexts" : rawValue }
    var symbol: String {
        switch self {
        case .containers: "shippingbox"
        case .images: "square.stack.3d.up"
        case .volumes: "externaldrive"
        case .machines: "desktopcomputer"
        case .files: "folder"
        case .contexts: "network"
        case .activity: "chart.xyaxis.line"
        case .kubernetes: "helm"
        case .builds: "hammer"
        case .networks: "point.3.connected.trianglepath.dotted"
        }
    }
    var shortcut: Int? {
        let i = (Self.allCases.firstIndex(of: self) ?? 0) + 1
        return i <= 9 ? i : nil
    }
}

struct Shell {
    var openLogs: @MainActor (LogFeed) -> Void = { _ in }
    var openTerminal: @MainActor (String) -> Void = { _ in }
    var openContainerTerminal: @MainActor (String, String) -> Void = { _, _ in }
    var openDebugShell: @MainActor (_ target: String, _ kind: String, _ readOnly: Bool, _ context: String, _ name: String) -> Void = { _, _, _, _, _ in }
    var openK8sTerminal: @MainActor (_ namespace: String, _ pod: String, _ container: String) -> Void = { _, _, _ in }
}

private struct ShellKey: EnvironmentKey {
    static var defaultValue: Shell { Shell() }
}

extension EnvironmentValues {
    var shell: Shell {
        get { self[ShellKey.self] }
        set { self[ShellKey.self] = newValue }
    }
}

struct MainView: View {
    @Environment(AppModel.self) private var model
    @Environment(Navigation.self) private var nav

    var body: some View {
        NavigationSplitView {
            List(selection: Binding<SidebarItem?>(get: { nav.section }, set: { if let s = $0 { nav.section = s } })) {
                Section("Workspace") {
                    sidebarRow(.containers, count: model.containers.count)
                    sidebarRow(.machines, count: model.machines.count)
                    sidebarRow(.kubernetes)
                }
                Section("Library") {
                    sidebarRow(.images)
                    sidebarRow(.builds)
                    sidebarRow(.volumes)
                    sidebarRow(.networks)
                    sidebarRow(.files)
                }
                Section("Remote") { sidebarRow(.contexts) }
                Section("Monitor") { sidebarRow(.activity) }
            }
            .listStyle(.sidebar)
            .navigationSplitViewColumnWidth(min: 190, ideal: 210, max: 260)
            .safeAreaInset(edge: .top, spacing: 0) {
                HStack(spacing: 10) {
                    BrandIcon(size: 36)
                    Text("Fevom").font(.system(size: 18, weight: .semibold))
                    Spacer()
                }
                .padding(.horizontal, 17).padding(.top, 15).padding(.bottom, 20)
            }
            .safeAreaInset(edge: .bottom) { ServiceFooter().padding(14) }
        } detail: {
            VStack(spacing: 0) {
                if let e = model.lastError {
                    Banner(text: e, color: .red) { model.lastError = nil }
                }
                if let i = model.lastInfo {
                    Banner(text: i, color: .secondary) { model.lastInfo = nil }
                }
                switch nav.section {
                case .containers: ContainersView()
                case .images: ImagesView()
                case .volumes: VolumesView()
                case .machines: MachinesView()
                case .files: FilesView()
                case .contexts: RemoteContextsView()
                case .activity: ActivityView()
                case .kubernetes: KubernetesView()
                case .builds: BuildsView()
                case .networks: NetworksView()
                }
            }
            .background(WorkspaceStyle.canvas)
        }
    }

    func sidebarRow(_ item: SidebarItem, count: Int? = nil) -> some View {
        HStack(spacing: 10) {
            Label(item.rawValue, systemImage: item.symbol)
            Spacer(minLength: 4)
            if let count { Text("\(count)").font(.caption.monospacedDigit()).foregroundStyle(.secondary) }
        }
        .padding(.vertical, 5)
        .tag(item)
        .help(item.shortcut.map { "\(item.rawValue) (⌘\($0))" } ?? item.rawValue)
    }
}

struct Banner: View {
    let text: String
    let color: Color
    let dismiss: () -> Void

    var body: some View {
        HStack(alignment: .top, spacing: 10) {
            Image(systemName: color == .red ? "exclamationmark.circle.fill" : "info.circle.fill").foregroundStyle(color)
            Text(text).font(.callout).foregroundStyle(.primary).textSelection(.enabled)
            Spacer()
            Button("Dismiss", systemImage: "xmark", action: dismiss).labelStyle(.iconOnly).buttonStyle(.borderless)
        }
        .padding(13)
        .background(color.opacity(0.08), in: RoundedRectangle(cornerRadius: 9))
        .padding(.horizontal, WorkspaceStyle.inset).padding(.top, 12)
        .accessibilityElement(children: .combine)
    }
}

struct ServiceFooter: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack(spacing: 7) {
                Circle().fill(stateColor).frame(width: 8, height: 8)
                Text(ServiceLine.describe(link: model.link, status: model.status)).font(.system(size: 12, weight: .medium)).lineLimit(2)
                Spacer(minLength: 4)
                if !canStart {
                    Menu {
                        Button("Restart Fevom") { Task { await model.restartService() } }
                        Button("Stop Fevom") { Task { await model.stopService() } }
                    } label: { Image(systemName: "ellipsis.circle") }
                        .menuStyle(.borderlessButton)
                        .fixedSize()
                        .menuIndicator(.hidden)
                        .help("Manage Fevom")
                        .disabled(model.link == .connecting || model.link == .startingDaemon)
                }
            }
            .accessibilityElement(children: .combine)
            if canStart {
                Button("Start Fevom", systemImage: "play.fill") { Task { await model.startService() } }
                    .buttonStyle(.borderedProminent).controlSize(.small)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(12)
        .background(.primary.opacity(0.035), in: RoundedRectangle(cornerRadius: 11))
    }

    var canStart: Bool {
        switch model.link {
        case .stoppedByUser, .failed: true
        case .connected: model.status?.state == DaemonAPI.Const.stateStopped
        default: false
        }
    }

    var stateColor: Color {
        switch model.status?.state {
        case DaemonAPI.Const.stateRunning: model.status?.engine == DaemonAPI.Const.engineReady ? .green : .yellow
        case DaemonAPI.Const.stateStarting, DaemonAPI.Const.stateStopping, DaemonAPI.Const.statePaused: .yellow
        default: model.link == .connected ? .gray : .red
        }
    }
}

struct ContainersView: View {
    @Environment(AppModel.self) private var model
    @Environment(Navigation.self) private var nav
    @Environment(\.shell) private var shell
    @State private var selection: Set<ContainerRow.ID> = []
    @State private var filter = ""
    @State private var pendingDelete: DeleteRequest?

    var body: some View {
        let rows = ContainerRow.rows(model.containers, filter: filter)
        let detail = selection.count == 1 ? selection.first.flatMap { find($0, in: rows)?.container } : nil
        ListDetail(idealDetail: 320, shown: detail != nil, widthKey: "detailWidth.containers") {
            ContainersOutline(rows: .init(rows), selection: $selection, menu: menu, primary: { r in
                if let c = r.container { openLogs(c) }
            }, delete: requestDelete)
        } detail: {
            if let c = detail {
                ContainerDetailView(container: c)
            }
        }
        .searchable(text: $filter, prompt: "Filter containers")
        .findFocus()
        .onDeleteCommand {
            if let id = selection.first, let row = find(id, in: rows) { requestDelete(row) }
        }
        .confirmDelete($pendingDelete, model: model)
        .overlay {
            if model.containers.isEmpty {
                WorkspaceEmptyState(symbol: "shippingbox", title: "No containers",
                                    command: "docker run -d -p 8080:80 nginx")
            } else if rows.isEmpty {
                WorkspaceEmptyState(symbol: "magnifyingglass", title: "No matching containers")
            }
        }
        .workspacePage(summary: "\(model.containers.count) containers, \(model.containers.filter { $0.state == "running" }.count) running")
        .toolbar {
            if selection.count == 1, let id = selection.first, let row = find(id, in: rows) {
                ToolbarItemGroup(placement: .primaryAction) {
                    if let c = row.container {
                        Button(c.state == "running" ? "Stop" : c.state == "paused" ? "Resume" : "Start",
                               systemImage: c.state == "running" ? "stop.fill" : "play.fill") {
                            Task { await model.container(c.id, c.state == "running" ? "stop" : c.state == "paused" ? "unpause" : "start") }
                        }
                        Button("Terminal", systemImage: "terminal") { shell.openContainerTerminal(c.id, c.name) }
                            .disabled(c.state != "running")
                        Button("Debug Shell", systemImage: "ladybug") { shell.openDebugShell(c.id, "container", false, "", c.name) }
                            .disabled(c.state == "paused")
                            .help("Open a shell with debugging tools inside this container, without changing it")
                        Button("Logs", systemImage: "text.alignleft") { openLogs(c) }
                    } else if let project = row.project {
                        Button(row.state == "running" ? "Stop Project" : "Start Project",
                               systemImage: row.state == "running" ? "stop.fill" : "play.fill") {
                            Task { await model.project(project, row.state == "running" ? "stop" : "start") }
                        }
                        Button("Logs", systemImage: "text.alignleft") { openProjectLogs(row) }
                    }
                }
            }
        }
        .navigationTitle("Containers")
    }

    func find(_ id: String, in rows: [ContainerRow]) -> ContainerRow? {
        for r in rows {
            if r.id == id { return r }
            if let c = r.children?.first(where: { $0.id == id }) { return c }
        }
        return nil
    }

    func requestDelete(_ row: ContainerRow) {
        if let p = row.project { pendingDelete = .project(named: p, in: model.containers) } else if let c = row.container { pendingDelete = .container(c) }
    }

    func menu(_ row: ContainerRow) -> [MenuEntry] {
        var m: [MenuEntry] = []
        func add(_ title: String, _ run: @escaping () -> Void) { m.append(.action(title, run)) }
        if let p = row.project {
            add("Start") { Task { await model.project(p, "start") } }
            add("Stop") { Task { await model.project(p, "stop") } }
            add("Restart") { Task { await model.project(p, "restart") } }
            m.append(.divider)
            add("Logs") { openProjectLogs(row) }
            add("Copy Command") { copyToPasteboard("docker compose -p \(p) logs -f") }
            m.append(.divider)
            add("Delete…") { pendingDelete = .project(named: p, in: model.containers) }
        } else if let c = row.container {
            if c.state == "running" {
                add("Stop") { Task { await model.container(c.id, "stop") } }
                add("Restart") { Task { await model.container(c.id, "restart") } }
                add("Pause") { Task { await model.container(c.id, "pause") } }
                add("Kill") { Task { await model.container(c.id, "kill") } }
            } else if c.state == "paused" {
                add("Resume") { Task { await model.container(c.id, "unpause") } }
            } else {
                add("Start") { Task { await model.container(c.id, "start") } }
            }
            m.append(.divider)
            add("Logs") { openLogs(c) }
            if c.state == "running" {
                add("Open Terminal") { shell.openContainerTerminal(c.id, c.name) }
                add("Open Files") { nav.showFiles(FileBrowser.container(c)) }
                add("Copy Terminal Command") { copyToPasteboard("docker exec -it \(c.name) sh") }
            }
            if c.state == "running" || c.state == "exited" || c.state == "created" || c.state == "dead" {
                add("Debug Shell") { shell.openDebugShell(c.id, "container", false, "", c.name) }
                add("Open Debug Shell in Terminal App") { try? ExternalTerminal.debug(target: c.name) }
            }
            if let port = c.ports.compactMap(\.publicPort).sorted().first {
                add("Open in Browser") { NSWorkspace.shared.open(URL(string: "http://localhost:\(port)")!) }
            }
            add("Open Domain in Browser") { NSWorkspace.shared.open(URL(string: "http://\(c.domains[0])")!) }
            m.append(.divider)
            add("Copy ID") { copyToPasteboard(c.id) }
            add("Copy Image") { copyToPasteboard(c.image) }
            add("Copy Domain") { copyToPasteboard(c.domains[0]) }
            add("Copy Address") { Task { if let a = try? await model.docker.inspect(c.id).address { copyToPasteboard(a) } } }
            add("Copy Command") { copyToPasteboard("docker logs -f \(c.name)") }
            m.append(.divider)
            add("Delete…") { pendingDelete = .container(c) }
        }
        return m
    }

    func openLogs(_ c: Container) {
        shell.openLogs(LogFeed.bounded(title: c.name, socketPath: DaemonAPI.dockerSocketPath(home: model.place.home), sources: [.init(id: c.id)]))
    }

    func openProjectLogs(_ row: ContainerRow) {
        let members = (row.children ?? []).compactMap(\.container)
        let width = members.map { ($0.composeService ?? $0.name).count }.max() ?? 0
        let sources = members.map { c -> LogFeed.Source in
            let n = c.composeService ?? c.name
            return .init(id: c.id, label: n.padding(toLength: width, withPad: " ", startingAt: 0) + " | ")
        }
        shell.openLogs(LogFeed.bounded(title: row.name, socketPath: DaemonAPI.dockerSocketPath(home: model.place.home), sources: sources))
    }
}

struct ContainerDetailView: View {
    @Environment(AppModel.self) private var model
    let container: Container
    @State private var detail: ContainerDetail?
    @State private var error: String?

    var usage: Activity.Figures? {
        model.activity.groups.first { $0.kind == DaemonAPI.Const.statsContainer && $0.id == container.id }
    }

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 14) {
                HStack(spacing: 12) {
                    EntityIcon(symbol: "shippingbox", size: 40)
                    VStack(alignment: .leading, spacing: 5) {
                        Text(container.name).font(.headline).textSelection(.enabled)
                        StatusPill(state: container.state)
                    }
                }
                .padding(.vertical, 4)
                if let d = detail {
                    InspectorSection(title: "Overview") {
                        detailValue("Image", d.Config?.Image ?? container.image)
                        if let a = d.address { detailValue("Address", a) }
                        let command = (d.Config?.Cmd ?? []).joined(separator: " ")
                        if !command.isEmpty { detailValue("Command", command) }
                        if let choice = model.status?.emulation?.workloads.first(where: {
                            $0.id == container.id || $0.id.isEmpty && $0.name == container.name
                        }) {
                            detailValue("x86 engine", choice.engine == "fex" ? "FEX" : "Rosetta")
                            Text(choice.reason).font(.caption).foregroundStyle(.secondary)
                        }
                    }
                    InspectorSection(title: "Resources") {
                        if let r = usage {
                            HStack {
                                metric("CPU", Fmt.percent(r.cpuPercent))
                                Spacer()
                                metric("Memory", Fmt.bytes(r.memoryBytes))
                            }
                            Divider()
                            detailValue("Network in / out", "\(Fmt.rate(r.netRx)) / \(Fmt.rate(r.netTx))")
                            detailValue("Disk read / write", "\(Fmt.rate(r.diskRead)) / \(Fmt.rate(r.diskWrite))")
                            detailValue("Processes", "\(r.pids)")
                        } else {
                            Text(container.state == "running" ? "Measuring…" : "Start the container to see its resource usage.")
                                .font(.callout).foregroundStyle(.secondary)
                        }
                    }
                    InspectorSection(title: "Network") {
                        ForEach(container.domains, id: \.self) { Text($0).font(.callout).textSelection(.enabled) }
                        ForEach((d.NetworkSettings?.Ports ?? [:]).keys.sorted(), id: \.self) { k in
                            let b = (d.NetworkSettings?.Ports?[k] ?? nil)?.compactMap { "\($0.HostIp ?? ""):\($0.HostPort ?? "")" } ?? []
                            detailValue(k, b.isEmpty ? "Not published" : b.joined(separator: ", "))
                        }
                    }
                    if !(d.Mounts ?? []).isEmpty {
                        InspectorSection(title: "Mounts") {
                            ForEach(Array((d.Mounts ?? []).enumerated()), id: \.offset) { _, m in
                                detailValue(m.Destination ?? "", "\(m.Type ?? "") \(m.Name ?? m.Source ?? "")")
                            }
                        }
                    }
                    InspectorSection(title: "Configuration") {
                        DisclosureGroup("Environment (\((d.Config?.Env ?? []).count))") {
                            VStack(alignment: .leading, spacing: 8) {
                                ForEach(d.Config?.Env ?? [], id: \.self) { Text($0).font(.caption.monospaced()).textSelection(.enabled) }
                            }.padding(.top, 8).frame(maxWidth: .infinity, alignment: .leading)
                        }
                        DisclosureGroup("Labels (\((d.Config?.Labels ?? [:]).count))") {
                            VStack(alignment: .leading, spacing: 10) {
                                ForEach((d.Config?.Labels ?? [:]).sorted(by: { $0.key < $1.key }), id: \.key) { k, v in detailValue(k, v) }
                            }.padding(.top, 8)
                        }
                    }
                } else if let error {
                    Label(error, systemImage: "exclamationmark.triangle").foregroundStyle(.red)
                } else {
                    ProgressView().frame(maxWidth: .infinity).padding()
                }
            }
            .padding(18)
        }
        .background(WorkspaceStyle.canvas)
        .onAppear { model.watchStats() }
        .onDisappear { model.unwatchStats() }
        .task(id: container) {
            detail = nil
            error = nil
            do {
                let result = try await model.docker.inspect(container.id)
                guard !Task.isCancelled else { return }
                detail = result
            } catch {
                if !Task.isCancelled { self.error = String(describing: error) }
            }
        }
    }

    private func detailValue(_ label: String, _ value: String) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            Text(label).font(.caption).foregroundStyle(.secondary)
            Text(value).font(.callout).textSelection(.enabled).fixedSize(horizontal: false, vertical: true)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private func metric(_ label: String, _ value: String) -> some View {
        VStack(alignment: .leading, spacing: 5) {
            Text(label).font(.caption).foregroundStyle(.secondary)
            Text(value).font(.system(size: 20, weight: .medium)).monospacedDigit()
        }
    }

}

struct ImagesView: View {
    @Environment(AppModel.self) private var model
    @Environment(Navigation.self) private var nav
    @Environment(\.shell) private var shell
    @State private var selection: Set<DockerImage.ID> = []
    @State private var filter = ""
    @State private var pendingImage: DockerImage?

    var body: some View {
        let rows = model.images.filter { filter.isEmpty || $0.repoTags.joined(separator: " ").localizedCaseInsensitiveContains(filter) }
        Table(rows, selection: $selection) {
            TableColumn("Name") { i in
                HStack(spacing: 10) { EntityIcon(symbol: "square.stack.3d.up", size: 26); Text(i.name).fontWeight(.medium) }
                    .padding(.vertical, 5)
            }
            .width(min: 200, ideal: 320)
            TableColumn("Size") { i in Text(ByteCountFormatter.string(fromByteCount: i.size, countStyle: .file)).monospacedDigit() }
                .width(90)
            TableColumn("Created") { i in Text(Date(timeIntervalSince1970: TimeInterval(i.created)), style: .date) }
                .width(min: 120, ideal: 150)
            TableColumn("Used By") { i in Text(i.users(in: model.containers).joined(separator: ", ")) }
        }
        .alternatingRowBackgrounds(.disabled)
        .contextMenu(forSelectionType: DockerImage.ID.self) { ids in
            if let id = ids.first, let i = model.images.first(where: { $0.id == id }) {
                Button("Debug Shell") { shell.openDebugShell(i.repoTags.first(where: { $0 != "<none>:<none>" }) ?? i.id, "image", false, "", i.name) }
                Button("Open Files") { nav.showFiles(FileBrowser.image(i)) }
                Divider()
                Button("Copy Name") { copyToPasteboard(i.name) }
                Button("Copy ID") { copyToPasteboard(i.id) }
                Button("Copy Command") { copyToPasteboard("docker rmi \(i.name)") }
                Divider()
                Button("Export…") { exportImage(i) }
                Divider()
                Button("Delete…", role: .destructive) { pendingImage = i }
            }
        }
        .toolbar {
            if let id = selection.first, let image = model.images.first(where: { $0.id == id }) {
                Button("Debug Shell", systemImage: "ladybug") { shell.openDebugShell(image.repoTags.first(where: { $0 != "<none>:<none>" }) ?? image.id, "image", false, "", image.name) }
                    .help("Open a shell with debugging tools inside this image")
                Button("Open Files", systemImage: "folder") { nav.showFiles(FileBrowser.image(image)) }
                Button("Export", systemImage: "square.and.arrow.up") { exportImage(image) }
            }
            Button("Import…", systemImage: "square.and.arrow.down") { importImages() }
                .labelStyle(.titleAndIcon)
                .help("Load images from a tar file")
            Button("Prune", systemImage: "trash") { Task { if let r = await model.pruneImages() { model.lastInfo = "Pruned dangling images: \(r)" } } }
                .labelStyle(.titleAndIcon)
                .help("Remove dangling images")
        }
        .searchable(text: $filter, prompt: "Filter images")
        .findFocus()
        .onDeleteCommand {
            if let id = selection.first, let i = model.images.first(where: { $0.id == id }) { pendingImage = i }
        }
        .confirmationDialog("Delete \(pendingImage?.name ?? "")?", isPresented: Binding(get: { pendingImage != nil }, set: { if !$0 { pendingImage = nil } }),
                            presenting: pendingImage) { i in
            Button("Delete Image", role: .destructive) { Task { await model.removeImage(i.repoTags.first ?? i.id, force: false) } }
            Button("Cancel", role: .cancel) {}
        } message: { i in
            Text(i.inUse(by: model.containers) ? "A container uses it: the engine refuses to delete it while that container exists." : "docker rmi \(i.name)")
        }
        .overlay {
            if model.images.isEmpty {
                WorkspaceEmptyState(symbol: "square.stack.3d.up", title: "No images",
                                    actionTitle: "Import Images…", action: importImages)
            } else if rows.isEmpty {
                WorkspaceEmptyState(symbol: "magnifyingglass", title: "No matching images")
            }
        }
        .workspacePage(summary: "\(rows.count) of \(model.images.count) images")
        .navigationTitle("Images")
        .onAppear { model.watchList(.images) }
        .onDisappear { model.unwatchList(.images) }
    }

    func exportImage(_ i: DockerImage) {
        let panel = NSSavePanel()
        let ref = i.repoTags.first(where: { $0 != "<none>:<none>" }) ?? i.id
        panel.nameFieldStringValue = i.name.replacingOccurrences(of: "/", with: "_").replacingOccurrences(of: ":", with: "_") + ".tar"
        guard panel.runModal() == .OK, let url = panel.url else { return }
        Task { await model.exportImage(ref, to: url) }
    }

    func importImages() {
        let panel = NSOpenPanel()
        panel.canChooseDirectories = false
        panel.allowsMultipleSelection = false
        guard panel.runModal() == .OK, let url = panel.url else { return }
        Task { await model.importImages(from: url) }
    }
}

struct VolumesView: View {
    @Environment(AppModel.self) private var model
    @Environment(Navigation.self) private var nav
    @State private var selection: Set<DockerVolume.ID> = []
    @State private var creating = false
    @State private var newName = ""
    @State private var filter = ""
    @State private var pendingVolume: String?
    @State private var cloning: String?
    @State private var cloneName = ""

    var body: some View {
        Table(model.volumes.filter { filter.isEmpty || $0.name.localizedCaseInsensitiveContains(filter) }, selection: $selection) {
            TableColumn("Name") { v in
                HStack(spacing: 10) { EntityIcon(symbol: "externaldrive", color: .purple, size: 26); Text(v.name).fontWeight(.medium) }
                    .padding(.vertical, 5)
            }
            TableColumn("Driver") { v in Text(v.driver) }
            TableColumn("Used By") { v in Text(v.users(in: model.containers).joined(separator: ", ")) }
        }
        .alternatingRowBackgrounds(.disabled)
        .contextMenu(forSelectionType: DockerVolume.ID.self) { ids in
            if let name = ids.first {
                Button("Open Files") { nav.showFiles(FileBrowser.volume(name)) }
                Divider()
                Button("Copy Name") { copyToPasteboard(name) }
                Button("Copy Command") { copyToPasteboard("docker volume rm \(name)") }
                Divider()
                Button("Clone…") { cloneName = name + "-clone"; cloning = name }
                Button("Export…") { exportVolume(name) }
                Divider()
                Button("Delete…", role: .destructive) { pendingVolume = name }
            }
        }
        .toolbar {
            if let name = selection.first {
                Button("Open Files", systemImage: "folder") { nav.showFiles(FileBrowser.volume(name)) }
                Button("Export", systemImage: "square.and.arrow.up") { exportVolume(name) }
            }
            Button("Import…", systemImage: "square.and.arrow.down") { importVolume() }
                .help("Import a volume from an archive Fevom exported")
            Button("New Volume", systemImage: "plus") { creating = true }
                .labelStyle(.titleAndIcon)
        }
        .sheet(isPresented: $creating) {
            NamePrompt(title: "New Volume", label: "Name", name: $newName, actionTitle: "Create Volume") {
                let n = newName
                Task { await model.createVolume(n) }
            }
        }
        .sheet(item: Binding(get: { cloning.map { CloneSource(name: $0) } }, set: { cloning = $0?.name })) { src in
            NamePrompt(title: "Clone \(src.name)", label: "Name of the clone", name: $cloneName, actionTitle: "Clone Volume") {
                let to = cloneName
                Task { await model.cloneVolume(src.name, to: to) }
            }
        }
        .searchable(text: $filter, prompt: "Filter volumes")
        .findFocus()
        .onDeleteCommand { pendingVolume = selection.first }
        .confirmationDialog("Delete the volume \(pendingVolume ?? "")?", isPresented: Binding(get: { pendingVolume != nil }, set: { if !$0 { pendingVolume = nil } }),
                            presenting: pendingVolume) { name in
            Button("Delete Volume", role: .destructive) { Task { await model.removeVolume(name) } }
            Button("Cancel", role: .cancel) {}
        } message: { name in
            Text("Its data is deleted.")
        }
        .overlay {
            if model.volumes.isEmpty {
                WorkspaceEmptyState(symbol: "externaldrive", title: "No volumes",
                                    actionTitle: "New Volume", action: { creating = true })
            } else if !filter.isEmpty && !model.volumes.contains(where: { $0.name.localizedCaseInsensitiveContains(filter) }) {
                WorkspaceEmptyState(symbol: "magnifyingglass", title: "No matching volumes")
            }
        }
        .workspacePage(summary: "\(model.volumes.count) volumes")
        .navigationTitle("Volumes")
        .onAppear { model.watchList(.volumes) }
        .onDisappear { model.unwatchList(.volumes) }
    }

    struct CloneSource: Identifiable { let name: String; var id: String { name } }

    func exportVolume(_ name: String) {
        let panel = NSSavePanel()
        panel.nameFieldStringValue = "\(name).tar.zst"
        guard panel.runModal() == .OK, let url = panel.url else { return }
        let replace = FileManager.default.fileExists(atPath: url.path)
        Task { await model.exportVolume(name, to: url.path, replace: replace) }
    }

    func importVolume() {
        let panel = NSOpenPanel()
        panel.canChooseDirectories = false
        panel.allowsMultipleSelection = false
        guard panel.runModal() == .OK, let url = panel.url else { return }
        Task { await model.importVolume(from: url.path) }
    }
}

struct MachinesView: View {
    @Environment(AppModel.self) private var model
    @Environment(Navigation.self) private var nav
    @Environment(\.shell) private var shell
    @State private var filter = ""
    @State private var selection: Set<String> = []
    @State private var creating = false
    @State private var prompt: Prompt?
    @State private var newName = ""
    @State private var pendingDelete: DeleteRequest?

    enum Prompt: Identifiable {
        case rename(String), clone(String), settings(String)
        var id: String { switch self { case .rename(let n): "r" + n; case .clone(let n): "c" + n; case .settings(let n): "s" + n } }
    }

    var body: some View {
        Table(model.machines.filter { filter.isEmpty || $0.name.localizedCaseInsensitiveContains(filter) || $0.image.distro.localizedCaseInsensitiveContains(filter) },
              selection: $selection) {
            TableColumn("Name") { m in
                HStack(spacing: 10) {
                    EntityIcon(symbol: "desktopcomputer", size: 27)
                    Text(m.name).fontWeight(.medium)
                    if m.default {
                        Text("Default").font(.system(size: 10, weight: .medium)).foregroundStyle(.secondary)
                            .padding(.horizontal, 6).padding(.vertical, 3)
                            .background(.primary.opacity(0.05), in: Capsule())
                    }
                }
                .padding(.vertical, 6)
            }
            .width(min: 180, ideal: 220)
            TableColumn("Distribution") { m in Text("\(MachineCatalog.displayName(m.image.distro)) \(m.image.release)").foregroundStyle(.secondary) }
                .width(min: 120, ideal: 150)
            TableColumn("Runtime") { m in
                VStack(alignment: .leading, spacing: 3) {
                    Text(m.image.arch.uppercased()).font(.caption.weight(.medium))
                    Text(m.image.arch == "amd64" ? (m.emulation.isEmpty ? "Emulated" : m.emulation == "fex" ? "FEX" : "Rosetta") : "Native")
                        .font(.caption2).foregroundStyle(.secondary)
                }
                .help(m.emulationReason)
            }
            .width(90)
            TableColumn("Status") { m in
                VStack(alignment: .leading, spacing: 3) {
                    StatusPill(state: m.error.isEmpty ? m.state : "failed")
                    if !m.error.isEmpty { Text(m.error).font(.caption).foregroundStyle(.red).lineLimit(1).help(m.error) }
                }
            }
            .width(min: 125, ideal: 135)
            TableColumn("Address") { m in Text(m.address).font(.callout.monospacedDigit()).foregroundStyle(.secondary) }
                .width(min: 100, ideal: 130)
        }
        .alternatingRowBackgrounds(.disabled)
        .contextMenu(forSelectionType: String.self) { names in
            if let name = names.first, let m = model.machines.first(where: { $0.name == name }) { menu(m) }
        } primaryAction: { names in
            if let name = names.first { shell.openTerminal(name) }
        }
        .toolbar {
            if let name = selection.first, let machine = model.machines.first(where: { $0.name == name }) {
                Button(machine.state == DaemonAPI.Const.machineRunning ? "Stop" : "Start",
                       systemImage: machine.state == DaemonAPI.Const.machineRunning ? "stop.fill" : "play.fill") {
                    Task { await model.machine(name, start: machine.state != DaemonAPI.Const.machineRunning) }
                }
                .disabled(machine.state == "starting" || machine.state == "stopping")
                Button("Terminal", systemImage: "terminal") { shell.openTerminal(name) }
                    .labelStyle(.titleAndIcon)
                Menu("More", systemImage: "ellipsis") { menu(machine) }
                    .help("More actions")
            }
            Button("New Machine", systemImage: "plus") { creating = true }
                .labelStyle(.titleAndIcon)
                .help("Create a Linux machine (⌘N)")
        }
        .safeAreaInset(edge: .bottom) {
            if !model.operations.isEmpty {
                VStack(alignment: .leading) {
                    ForEach(Array(model.operations.values), id: \.id) { op in
                        HStack {
                            ProgressView().controlSize(.small)
                            Text("\(op.method) \(op.target): \(op.phase)").font(.caption)
                            Button("Cancel") { Task { await model.cancelOperation(op.id) } }.controlSize(.small)
                        }
                    }
                }
                .padding(8)
            }
        }
        .searchable(text: $filter, prompt: "Filter machines")
        .findFocus()
        .onDeleteCommand { if let n = selection.first { pendingDelete = .machine(n) } }
        .onChange(of: nav.newMachine) { creating = true }
        .onChange(of: nav.machineSettings, initial: true) { _, name in
            if let name { prompt = .settings(name); nav.machineSettings = nil }
        }
        .sheet(isPresented: $creating) { CreateMachineSheet() }
        .confirmDelete($pendingDelete, model: model)
        .sheet(item: $prompt) { p in
            switch p {
            case .rename(let n):
                NamePrompt(title: "Rename \(n)", label: "New name", name: $newName, actionTitle: "Rename Machine") { let to = newName; Task { await model.renameMachine(n, to: to) } }
            case .clone(let n):
                NamePrompt(title: "Clone \(n)", label: "Name of the clone", name: $newName, actionTitle: "Clone Machine") { let to = newName; Task { await model.cloneMachine(n, to: to) } }
            case .settings(let n):
                MachineSettingsSheet(name: n)
            }
        }
        .overlay {
            if model.machines.isEmpty {
                WorkspaceEmptyState(symbol: "desktopcomputer", title: "No machines",
                                    actionTitle: "New Machine", action: { creating = true })
            } else if !filter.isEmpty && !model.machines.contains(where: { $0.name.localizedCaseInsensitiveContains(filter) || $0.image.distro.localizedCaseInsensitiveContains(filter) }) {
                WorkspaceEmptyState(symbol: "magnifyingglass", title: "No matching machines")
            }
        }
        .workspacePage(summary: "\(model.machines.count) machines, \(model.machines.filter { $0.state == DaemonAPI.Const.machineRunning }.count) running")
        .navigationTitle("Machines")
    }

    @ViewBuilder func menu(_ m: DaemonAPI.Machine) -> some View {
        let up = m.state == DaemonAPI.Const.machineRunning
        Button("Open Terminal") { shell.openTerminal(m.name) }
        Button("Open in Terminal App") { try? ExternalTerminal.open(machine: m.name) }
        Button("Open Files") { nav.showFiles(FileBrowser.machine(m.name)) }
        Button("Logs") {
            shell.openLogs(LogFeed.bounded(title: m.name, place: model.place,
                                           daemonSources: [DaemonAPI.machineConsoleLog(m.name), DaemonAPI.machineRuntimeLog(m.name)]))
        }
        Divider()
        if up {
            Button("Stop") { Task { await model.machine(m.name, start: false) } }
            Button("Restart") { Task { await model.restartMachine(m.name) } }
        } else {
            Button("Start") { Task { await model.machine(m.name, start: true) } }
        }
        Divider()
        Button("Settings…") { prompt = .settings(m.name) }
        Button("Clone…") { newName = m.name + "-clone"; prompt = .clone(m.name) }
        Button("Rename…") { newName = m.name; prompt = .rename(m.name) }
        if !m.default { Button("Set as Default") { Task { await model.setDefaultMachine(m.name) } } }
        Button("Export…") { export(m.name) }
        Divider()
        Button("Copy SSH Command") { copyToPasteboard("ssh \(m.name)@fev") }
        Button("Copy Address") { copyToPasteboard(m.address) }
        Divider()
        Button("Delete…", role: .destructive) { pendingDelete = .machine(m.name) }
    }

    func export(_ name: String) {
        let panel = NSSavePanel()
        panel.nameFieldStringValue = "\(name).tar.zst"
        guard panel.runModal() == .OK, let url = panel.url else { return }
        Task { await model.exportMachine(name, to: url.path) }
    }
}

struct CreateMachineSheet: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    @FocusState private var nameFocused: Bool
    @State private var name = ""
    @State private var distro = "ubuntu"
    @State private var release = ""
    @State private var architecture = "arm64"
    @State private var user = ""
    @State private var userData = ""
    @State private var isolated = false
    @State private var isolateNetwork = false
    @State private var showAdvanced = false
    @State private var showCommand = false
    @State private var submitting = false
    @State private var creationError: String?

    var body: some View {
        VStack(spacing: 0) {
            HStack(spacing: 14) {
                EntityIcon(symbol: "desktopcomputer", size: 46)
                Text("New Linux Machine").font(.title2.weight(.semibold))
                Spacer()
            }
            .padding(24)
            Form {
                if let creationError {
                    Section {
                        Label(creationError, systemImage: "exclamationmark.circle.fill")
                            .font(.callout).foregroundStyle(Color(nsColor: StatusAppearance.color("failed")))
                            .textSelection(.enabled).help(model.lastError ?? creationError)
                    }
                }
                Section("Machine") {
                    TextField("Name", text: $name, prompt: Text(distro)).focused($nameFocused)
                    Picker("Distribution", selection: $distro) {
                        ForEach(MachineCatalog.defaults, id: \.distro) { d in
                            Text(MachineCatalog.displayName(d.distro)).tag(d.distro)
                        }
                    }
                    TextField("Release", text: $release, prompt: Text(MachineCatalog.defaultRelease(distro) ?? "default"))
                }
                Section("Architecture") {
                    Picker("Architecture", selection: $architecture) {
                        ForEach(MachineCatalog.architectures, id: \.self) { arch in
                            Text(arch == "arm64" ? "ARM64 · Native" : "AMD64 · Emulated").tag(arch)
                        }
                    }
                    .pickerStyle(.segmented).labelsHidden()
                    Text(architecture == "arm64"
                         ? "Runs natively on Apple Silicon for the best performance."
                         : "Runs Intel and AMD Linux software using your emulation settings.")
                        .font(.caption).foregroundStyle(.secondary)
                }
                DisclosureGroup("Advanced options", isExpanded: $showAdvanced) {
                    TextField("User", text: $user, prompt: Text(NSUserName()))
                    Toggle("Isolate this machine", isOn: $isolated)
                        .help("A separate environment without shared Mac files or services")
                    Toggle("Internet access only", isOn: $isolateNetwork).disabled(!isolated)
                        .help("Allow internet access, with no access to the Mac or other workloads")
                    VStack(alignment: .leading, spacing: 8) {
                        Text("Cloud-init user data").font(.callout)
                        TextEditor(text: $userData).font(.callout.monospaced()).frame(minHeight: 110)
                            .accessibilityLabel("Cloud-init user data")
                    }
                }
            }
            .formStyle(.grouped)
            .frame(height: showAdvanced ? 420 : 360)
            .disabled(submitting)
            Divider()
            DisclosureGroup("Terminal command", isExpanded: $showCommand) {
                CommandLine(text: command).padding(.top, 8)
            }
            .font(.caption).foregroundStyle(.secondary)
            .padding(.horizontal, 24).padding(.vertical, 13)
            HStack {
                Button("Import…", systemImage: "square.and.arrow.down", action: importMachine)
                    .help("Create a machine from a Fevom export archive").disabled(submitting)
                Spacer()
                Button("Cancel") { dismiss() }.keyboardShortcut(.cancelAction).disabled(submitting)
                Button(submitting ? "Creating…" : "Create Machine", action: create)
                    .buttonStyle(.borderedProminent).keyboardShortcut(.defaultAction).disabled(submitting)
            }
            .controlSize(.large)
            .padding(.horizontal, 24).padding(.bottom, 20)
        }
        .frame(width: 560)
        .background(WorkspaceStyle.canvas)
        .onAppear { nameFocused = true }
    }

    func create() {
        let p = DaemonAPI.MachinesCreateParams(name: name.isEmpty ? distro : name, distro: distro, release: release,
                                               arch: architecture, user: user, userData: userData,
                                               isolated: isolated, isolateNetwork: isolated && isolateNetwork)
        submitting = true
        creationError = nil
        Task {
            let operation = await model.createMachine(p)
            submitting = false
            if operation != nil {
                dismiss()
            } else {
                let message = (model.lastError ?? "The machine could not be created. Check its settings and try again.")
                    .replacingOccurrences(of: #" \(code -?\d+\)$"#, with: "", options: .regularExpression)
                creationError = message.prefix(1).uppercased() + message.dropFirst()
            }
        }
    }

    func importMachine() {
        let panel = NSOpenPanel()
        guard panel.runModal() == .OK, let url = panel.url else { return }
        let n = name.isEmpty ? url.deletingPathExtension().deletingPathExtension().lastPathComponent : name
        Task { await model.importMachine(from: url.path, name: n) }
        dismiss()
    }

    var command: String {
        var c = "fev create"
        if architecture == "amd64" { c += " --arch amd64" }
        if isolated { c += " --isolated" }
        if isolated && isolateNetwork { c += " --isolate-network" }
        if !user.isEmpty { c += " -u \(user)" }
        if !userData.isEmpty { c += " -c user-data.yaml" }
        c += " \(distro)\(release.isEmpty ? "" : ":" + release)"
        if !name.isEmpty { c += " \(name)" }
        return c
    }
}

extension View {
    func confirmDelete(_ pending: Binding<DeleteRequest?>, model: AppModel) -> some View {
        confirmationDialog(pending.wrappedValue?.title ?? "", isPresented: Binding(get: { pending.wrappedValue != nil }, set: { if !$0 { pending.wrappedValue = nil } }),
                           titleVisibility: .visible, presenting: pending.wrappedValue) { r in
            Button(r.button, role: .destructive) { Task { await model.perform(r) } }
            Button("Cancel", role: .cancel) {}
        } message: { r in
            Text(r.message)
        }
    }
}

struct NamePrompt: View {
    let title: String
    let label: String
    @Binding var name: String
    var actionTitle = "Continue"
    let done: () -> Void
    @Environment(\.dismiss) private var dismiss

    @FocusState private var focused: Bool

    var body: some View {
        VStack(alignment: .leading, spacing: 20) {
            HStack(spacing: 12) {
                EntityIcon(symbol: "square.and.pencil", size: 40)
                Text(title).font(.title2.weight(.semibold))
            }
            TextField(label, text: $name).textFieldStyle(.roundedBorder).focused($focused)
                .onSubmit { submit() }
            HStack {
                Spacer()
                Button("Cancel", role: .cancel) { dismiss() }.keyboardShortcut(.cancelAction)
                Button(actionTitle, action: submit).buttonStyle(.borderedProminent)
                    .keyboardShortcut(.defaultAction).disabled(name.trimmingCharacters(in: .whitespaces).isEmpty)
            }
        }
        .padding(24).frame(width: 400)
        .onAppear { focused = true }
    }

    private func submit() {
        guard !name.trimmingCharacters(in: .whitespaces).isEmpty else { return }
        done()
        dismiss()
    }

}

extension DaemonAPI.Machine: Identifiable {
    public var id: String { name }
}

func copyToPasteboard(_ s: String) {
    NSPasteboard.general.clearContents()
    NSPasteboard.general.setString(s, forType: .string)
}

extension LogFeed {
    @MainActor static func bounded(title: String, socketPath: String, sources: [Source]) -> LogFeed {
        let p = AppPreferences()
        return LogFeed(title: title, socketPath: socketPath, sources: sources, capacity: p.logLines, byteCapacity: p.logMiB << 20)
    }

    @MainActor static func bounded(title: String, place: DaemonPlace, daemonSources: [String]) -> LogFeed {
        let p = AppPreferences()
        return LogFeed(title: title, place: place, daemonSources: daemonSources, capacity: p.logLines, byteCapacity: p.logMiB << 20)
    }

    @MainActor static func fevs(_ place: DaemonPlace) -> LogFeed { bounded(title: "Fevom", place: place, daemonSources: []) }
}

struct FindFocus: ViewModifier {
    @Environment(Navigation.self) private var nav
    @FocusState private var focused: Bool
    func body(content: Content) -> some View {
        content.searchFocused($focused).onChange(of: nav.find) { focused = true }
    }
}

extension View {
    func findFocus() -> some View { modifier(FindFocus()) }
}
