import AppKit
import FevomAPI
import FevomStores
import SwiftUI

struct KubernetesView: View {
    @Environment(AppModel.self) private var model
    @Environment(\.shell) private var shell
    @State private var tab: Tab = .workloads
    @State private var pane: Pane?
    @State private var scaling: DaemonAPI.K8sObject?
    @State private var replicas = 1
    @State private var pendingDelete: DaemonAPI.K8sObject?
    @State private var resetting = false
    @State private var resetAll = false
    @State private var hooked = false
    @State private var filter = ""

    enum Tab: String, CaseIterable, Identifiable {
        case workloads = "Deployments", pods = "Pods", services = "Services", ingresses = "Ingresses"
        var id: String { rawValue }
    }

    enum Pane: Equatable {
        case apply
        case edit(DaemonAPI.K8sObject)
    }

    var k8s: K8sModel { model.k8s }
    var running: Bool { k8s.status?.state == DaemonAPI.Const.k8sStateRunning }

    var body: some View {
        VStack(spacing: 0) {
            ClusterHeader(resetting: $resetting)
                .padding(.horizontal, WorkspaceStyle.inset).padding(.vertical, 14)
            Divider().overlay(WorkspaceStyle.border)
            if running {
                controls
                Divider().overlay(WorkspaceStyle.border)
                switch pane {
                case .apply:
                    ApplyPane { pane = nil }
                    Divider().overlay(WorkspaceStyle.border)
                case .edit(let o):
                    EditPane(object: o) { pane = nil }
                    Divider().overlay(WorkspaceStyle.border)
                case nil:
                    EmptyView()
                }
                lists
            } else {
                offState
            }
        }
        .onAppear { model.watchK8s() }
        .onDisappear { model.unwatchK8s() }
        .onChange(of: k8s.objects.count) { runHooks() }
        .sheet(item: $scaling) { o in scaleSheet(o) }
        .confirmationDialog(deleteTitle, isPresented: Binding(get: { pendingDelete != nil }, set: { if !$0 { pendingDelete = nil } })) {
            Button("Delete", role: .destructive) {
                if let o = pendingDelete { Task { await model.deleteK8s(o) } }
                pendingDelete = nil
            }
        } message: {
            Text("What it owns, such as a Deployment's pods, is deleted with it.")
        }
        .confirmationDialog("Reset Kubernetes?", isPresented: $resetting) {
            Button(resetAll ? "Reset and Delete the Images" : "Reset", role: .destructive) {
                let all = resetAll
                Task { await model.resetK8s(all: all) }
            }
        } message: {
            Text("This deletes the cluster's workloads, volumes and images. Docker's images, containers and volumes are kept."
                 + (resetAll ? " The downloaded Kubernetes images are deleted too: the next start downloads them again." : " The downloaded Kubernetes images are kept."))
        }
        .searchable(text: $filter, prompt: "Filter \(tab.rawValue.lowercased())")
        .workspacePage(summary: summary)
        .navigationTitle("Kubernetes")
    }

    var deleteTitle: String {
        guard let o = pendingDelete else { return "" }
        return "Delete \(o.kind) \(o.name)?"
    }

    var summary: String {
        guard running else { return k8s.status?.cost ?? "" }
        let d = k8s.list(DaemonAPI.Const.k8sKindDeployment).count, p = k8s.list(DaemonAPI.Const.k8sKindPod).count
        let s = k8s.list(DaemonAPI.Const.k8sKindService).count
        return "\(d) deployments, \(p) pods, \(s) services" + (k8s.namespace.isEmpty ? " in every namespace" : " in \(k8s.namespace)")
    }

    var controls: some View {
        @Bindable var k = model.k8s
        return HStack(spacing: 12) {
            Picker("Namespace", selection: $k.namespace) {
                Text("All namespaces").tag("")
                ForEach(k8s.namespaces, id: \.self) { Text($0).tag($0) }
            }
            .frame(maxWidth: 240)
            Picker("", selection: $tab) {
                ForEach(Tab.allCases) { Text($0.rawValue).tag($0) }
            }
            .pickerStyle(.segmented)
            .frame(maxWidth: 420)
            .labelsHidden()
            Spacer()
            Button("Apply…", systemImage: "square.and.arrow.down.on.square") { pane = .apply }
                .help("Apply manifests from a file, a folder or pasted YAML")
        }
        .padding(.horizontal, WorkspaceStyle.inset).padding(.vertical, 10)
    }

    @ViewBuilder var lists: some View {
        switch tab {
        case .workloads: deployments
        case .pods: pods
        case .services: services
        case .ingresses: ingresses
        }
    }

    var deployments: some View {
        Table(shown(DaemonAPI.Const.k8sKindDeployment), selection: .constant(Set<String>())) {
            TableColumn("Name") { o in nameCell(o, symbol: "square.stack.3d.down.right") }.width(min: 180, ideal: 240)
            TableColumn("Namespace") { o in Text(o.namespace).foregroundStyle(.secondary) }.width(min: 90, ideal: 120)
            TableColumn("Ready") { o in
                let w = o.workload ?? DaemonAPI.K8sWorkload()
                Text("\(w.ready) of \(w.wanted)").monospacedDigit()
                    .foregroundStyle(w.ready == w.wanted ? Color.primary : Color(nsColor: StatusAppearance.color("starting")))
            }.width(80)
            TableColumn("Images") { o in Text((o.workload?.images ?? []).joined(separator: ", ")).foregroundStyle(.secondary).lineLimit(1) }
        }
        .alternatingRowBackgrounds(.disabled)
    }

    var pods: some View {
        Table(shown(DaemonAPI.Const.k8sKindPod), selection: .constant(Set<String>())) {
            TableColumn("Name") { o in nameCell(o, symbol: "cube") }.width(min: 200, ideal: 260)
            TableColumn("Namespace") { o in Text(o.namespace).foregroundStyle(.secondary) }.width(min: 90, ideal: 120)
            TableColumn("Status") { o in
                let p = o.pod ?? DaemonAPI.K8sPod()
                StatusPill(state: p.ready ? "running" : p.phase == "Failed" ? "failed" : p.phase.lowercased())
                    .help(K8sModel.podState(o))
            }.width(min: 100, ideal: 120)
            TableColumn("Restarts") { o in Text("\(o.pod?.restarts ?? 0)").monospacedDigit().foregroundStyle(.secondary) }.width(70)
            TableColumn("Address") { o in Text(o.pod?.ip ?? "").font(.callout.monospacedDigit()).foregroundStyle(.secondary) }.width(min: 100, ideal: 120)
        }
        .alternatingRowBackgrounds(.disabled)
    }

    var services: some View {
        Table(shown(DaemonAPI.Const.k8sKindService), selection: .constant(Set<String>())) {
            TableColumn("Name") { o in nameCell(o, symbol: "network") }.width(min: 160, ideal: 200)
            TableColumn("Type") { o in Text(o.service?.type ?? "").foregroundStyle(.secondary) }.width(min: 90, ideal: 110)
            TableColumn("Address") { o in
                let s = o.service ?? DaemonAPI.K8sService()
                Text(s.loadBalancer.isEmpty ? s.clusterIP : s.loadBalancer).font(.callout.monospacedDigit()).foregroundStyle(.secondary)
            }.width(min: 100, ideal: 120)
            TableColumn("URLs") { o in urlCell(K8sModel.urls(o)) }
        }
        .alternatingRowBackgrounds(.disabled)
    }

    var ingresses: some View {
        Table(shown(DaemonAPI.Const.k8sKindIngress), selection: .constant(Set<String>())) {
            TableColumn("Name") { o in nameCell(o, symbol: "arrow.triangle.branch") }.width(min: 160, ideal: 200)
            TableColumn("Hosts") { o in Text((o.ingress?.hosts ?? []).joined(separator: ", ")).foregroundStyle(.secondary) }
            TableColumn("URLs") { o in urlCell(K8sModel.urls(o)) }
        }
        .alternatingRowBackgrounds(.disabled)
    }

    func shown(_ kind: String) -> [DaemonAPI.K8sObject] {
        let f = filter.trimmingCharacters(in: .whitespaces)
        guard !f.isEmpty else { return k8s.list(kind) }
        return k8s.list(kind).filter { $0.name.localizedCaseInsensitiveContains(f) || $0.namespace.localizedCaseInsensitiveContains(f) }
    }

    func nameCell(_ o: DaemonAPI.K8sObject, symbol: String) -> some View {
        HStack(spacing: 8) {
            EntityIcon(symbol: symbol, size: 24)
            Text(o.name).fontWeight(.medium).lineLimit(1)
            Spacer(minLength: 4)
            Menu {
                actions(o)
            } label: {
                Image(systemName: "ellipsis.circle")
            }
            .menuStyle(.borderlessButton)
            .menuIndicator(.hidden)
            .fixedSize()
            .help("Actions on \(o.name)")
        }
        .padding(.vertical, 4)
        .contentShape(Rectangle())
        .contextMenu { actions(o) }
        .onTapGesture(count: 2) { if o.kind == DaemonAPI.Const.k8sKindPod { openLogs(o) } }
    }

    func urlCell(_ urls: [String]) -> some View {
        VStack(alignment: .leading, spacing: 2) {
            ForEach(urls, id: \.self) { u in
                Button(u) { if let url = URL(string: u) { NSWorkspace.shared.open(url) } }
                    .buttonStyle(.link)
                    .lineLimit(1)
                    .fixedSize()
                    .help("Open \(u)")
            }
        }
        .padding(.vertical, 3)
    }

    @ViewBuilder func actions(_ o: DaemonAPI.K8sObject) -> some View {
        if o.kind == DaemonAPI.Const.k8sKindPod {
            Button("Logs") { openLogs(o) }
            Button("Terminal") { openTerminal(o) }
            Button("Open in Terminal App") { try? ExternalTerminal.kubectlExec(namespace: o.namespace, pod: o.name, container: o.pod?.containers.first?.name ?? "") }
            Divider()
        }
        if [DaemonAPI.Const.k8sKindDeployment, DaemonAPI.Const.k8sKindStatefulSet, DaemonAPI.Const.k8sKindReplicaSet].contains(o.kind) {
            Button("Scale…") { replicas = o.workload?.wanted ?? 1; scaling = o }
        }
        if [DaemonAPI.Const.k8sKindDeployment, DaemonAPI.Const.k8sKindStatefulSet, DaemonAPI.Const.k8sKindDaemonSet].contains(o.kind) {
            Button("Restart") { Task { await model.restartK8s(o) } }
        }
        Button("Edit…") { pane = .edit(o) }
        if !K8sModel.urls(o).isEmpty {
            Button("Copy URL") { copyToPasteboard(K8sModel.urls(o)[0]) }
        }
        Divider()
        Button("Delete…", role: .destructive) { pendingDelete = o }
    }

    func openLogs(_ o: DaemonAPI.K8sObject) {
        let c = o.pod?.containers.first?.name ?? ""
        shell.openLogs(LogFeed(title: o.name, place: model.place, pod: .init(namespace: o.namespace, pod: o.name, container: c, follow: true)))
    }

    func openTerminal(_ o: DaemonAPI.K8sObject) {
        shell.openK8sTerminal(o.namespace, o.name, o.pod?.containers.first?.name ?? "")
    }

    func scaleSheet(_ o: DaemonAPI.K8sObject) -> some View {
        VStack(alignment: .leading, spacing: 16) {
            Text("Scale \(o.name)").font(.headline)
            Stepper("Replicas: \(replicas)", value: $replicas, in: 0...100)
            HStack {
                Spacer()
                Button("Cancel", role: .cancel) { scaling = nil }
                Button("Scale") {
                    let n = replicas
                    Task { await model.scaleK8s(o, replicas: n) }
                    scaling = nil
                }
                .keyboardShortcut(.defaultAction)
            }
        }
        .padding(20)
        .frame(width: 320)
    }

    var offState: some View {
        let first = !(k8s.status?.archive.cached ?? false)
        let size = Double(k8s.status?.archive.size ?? 0) / 1_000_000
        return WorkspaceEmptyState(symbol: "helm", title: "Kubernetes is off",
                                   message: first ? "Turning it on downloads Kubernetes (\(Int(size.rounded())) MB) once." : "",
                                   command: "kubectl --context fev get nodes")
    }

    func runHooks() {
        guard !hooked, let spec = DevHooks.value("FEV_APP_K8S"), !k8s.objects.isEmpty else { return }
        let (kind, arg) = spec.split(separator: ":", maxSplits: 1).map(String.init).pair
        switch kind {
        case "apply":
            hooked = true
            pane = .apply
        case "tab":
            hooked = true
            tab = Tab.allCases.first { $0.rawValue.lowercased() == arg.lowercased() } ?? .workloads
        case "edit":
            let p = arg.split(separator: "/").map(String.init)
            if p.count == 3, let o = k8s.objects[K8sModel.key(DaemonAPI.K8sObject(kind: p[0], namespace: p[1], name: p[2]))] {
                hooked = true
                pane = .edit(o)
            }
        case "logs", "exec":
            let p = arg.split(separator: "/").map(String.init)
            if p.count == 2, let o = k8s.list(DaemonAPI.Const.k8sKindPod).first(where: { $0.namespace == p[0] && $0.name.hasPrefix(p[1]) }) {
                hooked = true
                if kind == "logs" { openLogs(o) } else { openTerminal(o) }
                if kind == "exec", let text = DevHooks.value("FEV_APP_TYPE") {
                    DispatchQueue.main.asyncAfter(deadline: .now() + 1.5) { DevHooks.typeInto(pod: o.name, text: text) }
                }
            }
        default:
            hooked = true
        }
    }
}

struct ClusterHeader: View {
    @Environment(AppModel.self) private var model
    @Binding var resetting: Bool

    var body: some View {
        let s = model.k8s.status
        let op = model.k8sOperation
        HStack(alignment: .top, spacing: 14) {
            EntityIcon(symbol: "helm", size: 36)
            VStack(alignment: .leading, spacing: 6) {
                HStack(spacing: 10) {
                    if let s { StatusPill(state: s.state == DaemonAPI.Const.k8sStateRunning && !s.ready ? "starting" : s.state) }
                    if let v = s?.version, !v.isEmpty { Text(v).font(.caption).foregroundStyle(.secondary) }
                }
                if let op {
                    HStack(spacing: 10) {
                        if let f = K8sModel.fraction(op) {
                            ProgressView(value: f).frame(width: 220)
                        } else {
                            ProgressView().controlSize(.small)
                        }
                        Text(K8sModel.progressText(op)).font(.callout).foregroundStyle(.secondary)
                    }
                } else if let e = s?.error, !e.isEmpty {
                    Text(e).font(.callout).foregroundStyle(Color(nsColor: StatusAppearance.color("failed"))).lineLimit(2)
                }
                if !model.k8s.on, let cost = s?.cost, !cost.isEmpty {
                    Text(cost + ".").font(.caption).foregroundStyle(.secondary)
                }
                if !model.k8s.kubeconfigLine.isEmpty {
                    Text(model.k8s.kubeconfigLine).font(.caption).foregroundStyle(.secondary)
                }
                if let note = s?.api.note, !note.isEmpty {
                    Text(note).font(.caption).foregroundStyle(Color(nsColor: StatusAppearance.color("starting")))
                }
            }
            Spacer()
            VStack(alignment: .trailing, spacing: 8) {
                HStack(spacing: 8) {
                    Text(model.k8s.on ? "On" : "Off").font(.callout).foregroundStyle(.secondary)
                Toggle("Kubernetes", isOn: Binding(get: { model.k8s.on }, set: { on in
                    Task { if on { await model.startK8s() } else { await model.stopK8s() } }
                }))
                .toggleStyle(.switch)
                .labelsHidden()
                .disabled(op != nil || s == nil)
                .help(model.k8s.on ? "Turn Kubernetes off (its pods stop; Docker's containers keep running)" : "Turn Kubernetes on")
                }
                Button("Reset…") { resetting = true }
                    .controlSize(.small)
                    .disabled(op != nil || s == nil)
            }
        }
    }
}

struct ApplyPane: View {
    @Environment(AppModel.self) private var model
    var close: () -> Void
    @State private var text = ""
    @State private var sources: [DaemonAPI.K8sDocument] = []
    @State private var busy = false

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack {
                Text("Apply Manifests").font(.headline)
                Text("Manifests without a namespace go into \(model.k8s.namespace.isEmpty ? "default" : model.k8s.namespace).")
                    .font(.caption).foregroundStyle(.secondary)
                Spacer()
                Button("Open File or Folder…") { pick() }
                Button("Close") { close() }
            }
            TextEditor(text: $text)
                .font(.system(.callout, design: .monospaced))
                .frame(minHeight: 120, maxHeight: 220)
                .overlay(RoundedRectangle(cornerRadius: 6).strokeBorder(WorkspaceStyle.border))
                .overlay(alignment: .topLeading) {
                    if text.isEmpty && sources.isEmpty {
                        Text("Paste YAML here, or open a file or a folder").foregroundStyle(.tertiary).padding(6).allowsHitTesting(false)
                    }
                }
            if !sources.isEmpty {
                Text("From " + sources.map { ($0.source as NSString).lastPathComponent }.joined(separator: ", ")).font(.caption).foregroundStyle(.secondary)
            }
            HStack {
                Button(busy ? "Applying…" : "Apply") { apply() }
                    .keyboardShortcut(.defaultAction)
                    .disabled(busy || (text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty && sources.isEmpty))
                Spacer()
            }
            if !model.k8s.applied.isEmpty {
                VStack(alignment: .leading, spacing: 3) {
                    ForEach(Array(model.k8s.applied.enumerated()), id: \.offset) { _, a in
                        HStack(spacing: 6) {
                            Text(a.ref.map { "\($0.kind.lowercased())/\($0.name)" } ?? a.source).font(.callout.monospaced())
                            Text(a.action).font(.callout).foregroundStyle(a.action == DaemonAPI.Const.k8sActionFailed ? Color(nsColor: StatusAppearance.color("failed")) : .secondary)
                            if !a.error.isEmpty { Text(a.error).font(.caption).foregroundStyle(.secondary).lineLimit(1).help(a.error) }
                        }
                    }
                }
            }
        }
        .padding(.horizontal, WorkspaceStyle.inset).padding(.vertical, 12)
        .background(WorkspaceStyle.surface)
        .onAppear {
            if let hook = DevHooks.value("FEV_APP_K8S_YAML") { text = hook }
        }
    }

    func pick() {
        let panel = NSOpenPanel()
        panel.canChooseFiles = true
        panel.canChooseDirectories = true
        panel.allowsMultipleSelection = true
        guard panel.runModal() == .OK else { return }
        sources = K8sModel.documents(panel.urls)
    }

    func apply() {
        var docs = sources
        if !text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty { docs.append(.init(source: "pasted", yaml: text)) }
        busy = true
        Task {
            await model.applyK8s(docs)
            busy = false
        }
    }
}

struct EditPane: View {
    @Environment(AppModel.self) private var model
    let object: DaemonAPI.K8sObject
    var close: () -> Void
    @State private var text = ""
    @State private var manifest: DaemonAPI.K8sManifest?
    @State private var note = ""
    @State private var conflict = false
    @State private var busy = false

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack {
                Text("Edit \(object.kind.lowercased())/\(object.name)").font(.headline)
                if let m = manifest { Text("resourceVersion \(m.resourceVersion)").font(.caption).foregroundStyle(.secondary) }
                Spacer()
                Button("Reload") { Task { await load() } }
                Button("Close") { close() }
            }
            TextEditor(text: $text)
                .font(.system(.callout, design: .monospaced))
                .frame(minHeight: 160, maxHeight: 300)
                .overlay(RoundedRectangle(cornerRadius: 6).strokeBorder(WorkspaceStyle.border))
            HStack(spacing: 10) {
                Button(busy ? "Saving…" : "Save") { save() }
                    .keyboardShortcut(.defaultAction)
                    .disabled(busy || manifest == nil)
                if !note.isEmpty {
                    Text(note).font(.callout)
                        .foregroundStyle(conflict ? Color(nsColor: StatusAppearance.color("failed")) : .secondary)
                        .lineLimit(2)
                }
                Spacer()
            }
        }
        .padding(.horizontal, WorkspaceStyle.inset).padding(.vertical, 12)
        .background(WorkspaceStyle.surface)
        .task(id: Self.id(object)) { await load() }
    }

    static func id(_ o: DaemonAPI.K8sObject) -> String { K8sModel.key(o) }

    func load() async {
        note = ""
        conflict = false
        if let m = await model.manifestK8s(object) {
            manifest = m
            text = m.yaml
        }
    }

    func save() {
        guard let m = manifest else { return }
        busy = true
        let yaml = text
        Task {
            switch await model.updateK8s(m.ref, yaml: yaml) {
            case .saved(let r):
                note = r.first.map { "\($0.action)" } ?? "saved"
                conflict = false
                if let again = await model.manifestK8s(object) { manifest = again }
            case .conflict(let why):
                note = "Not saved: it changed since you opened it (\(why)). Reload to edit what it is now."
                conflict = true
            case .failed(let why):
                note = why
                conflict = true
            }
            busy = false
        }
    }
}

private extension Array where Element == String {
    var pair: (String, String) { (first ?? "", count > 1 ? self[1] : "") }
}
