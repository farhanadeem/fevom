import AppKit
import FevomAPI
import FevomStores
import SwiftUI

@MainActor
final class SettingsWindowController: NSObject, NSWindowDelegate {
    let model: AppModel
    let onboarding: () -> Void
    let statusItem: (Bool) -> Void
    private(set) var window: NSWindow?
    private var viewing = false

    init(model: AppModel, onboarding: @escaping () -> Void, statusItem: @escaping (Bool) -> Void) {
        self.model = model
        self.onboarding = onboarding
        self.statusItem = statusItem
    }

    func present(tab: SettingsTab? = nil) {
        if let tab { SettingsTab.requested = tab }
        if window == nil {
            let root = SettingsView(onboarding: onboarding, statusItem: statusItem).environment(model)
            let w = NSWindow(contentViewController: NSHostingController(rootView: root))
            w.title = "Fevom Settings"
            w.styleMask = [.titled, .closable, .miniaturizable, .resizable]
            w.setContentSize(NSSize(width: 860, height: 650))
            w.contentMinSize = NSSize(width: 760, height: 560)
            w.isReleasedWhenClosed = false
            w.isRestorable = false
            w.setFrameAutosaveName("settings")
            w.delegate = self
            window = w
        }
        DevHooks.show(window)
        setViewing(true)
    }

    private func setViewing(_ on: Bool) {
        guard on != viewing else { return }
        viewing = on
        if on { model.show() } else { model.hide() }
    }

    func windowWillClose(_ notification: Notification) {
        setViewing(false)
        DispatchQueue.main.async { [weak self] in
            if let self, self.window?.isVisible == false { self.window = nil }
        }
    }
    func windowDidMiniaturize(_ notification: Notification) { setViewing(false) }
    func windowDidDeminiaturize(_ notification: Notification) { setViewing(true) }
}

enum SettingsTab: String, CaseIterable, Identifiable {
    case general = "General", resources = "Resources", network = "Network", machines = "Machines", engine = "Docker Engine", kubernetes = "Kubernetes", app = "App"
    var id: String { rawValue }
    var symbol: String {
        switch self {
        case .general: "gearshape"
        case .resources: "cpu"
        case .network: "network"
        case .machines: "desktopcomputer"
        case .engine: "shippingbox"
        case .kubernetes: "helm"
        case .app: "macwindow"
        }
    }

    @MainActor static var requested: SettingsTab?
    @MainActor static var scrollRequested: String?

    static let hidden: Set<String> = ["debug.cache_url", "debug.index_url", "kubernetes.images_url", "kubernetes.kubelet",
                                      "docker.node_name", "network.subnet", "network.subnet6"]

    static func of(_ key: String) -> SettingsTab {
        if key == "cpu" || key == "memory_mib" { return .resources }
        if key.hasPrefix("network.") || key.hasPrefix("admin.") { return .network }
        if key.hasPrefix("machines.") || key.hasPrefix("ssh.") || key.hasPrefix("machine.") { return .machines }
        if key.hasPrefix("kubernetes.") { return .kubernetes }
        return .general
    }
}

struct SettingsView: View {
    @Environment(AppModel.self) private var model
    let onboarding: () -> Void
    let statusItem: (Bool) -> Void
    @State private var tab: SettingsTab = SettingsTab.requested ?? .general

    var body: some View {
        HStack(spacing: 0) {
            List(selection: $tab) {
                ForEach(SettingsTab.allCases) { t in
                    Label(t.rawValue, systemImage: t.symbol).tag(t)
                        .padding(.vertical, 7)
                }
            }
            .listStyle(.sidebar)
            .frame(width: 185)
            .accessibilityLabel("Settings categories")
            Divider()
            VStack(spacing: 0) {
                if let e = model.lastError {
                    Banner(text: e, color: .red) { model.lastError = nil }
                }
                content(tab).frame(maxWidth: .infinity, maxHeight: .infinity)
            }
            .background(WorkspaceStyle.canvas)
        }
        .frame(minWidth: 760, minHeight: 560)
        .onAppear {
            if let r = SettingsTab.requested { tab = r; SettingsTab.requested = nil }
            model.watchConfig()
        }
        .onDisappear { model.unwatchConfig() }
    }

    @ViewBuilder func content(_ t: SettingsTab) -> some View {
        switch t {
        case .engine: EngineConfigView()
        case .app: AppPreferencesView(onboarding: onboarding, statusItem: statusItem)
        default:
            ScrollViewReader { proxy in
            Form {
                if t == .network { NetworkStatusSection() }
                if !model.pendingRestart.isEmpty { PendingRestartSection() }
                if model.configKeys.isEmpty {
                    Text(model.link == .connected ? "Reading settings…" : "Start Fevom to view and change its settings.")
                        .foregroundStyle(.secondary)
                }
                ForEach(groups(t), id: \.self) { group in
                    Section(group) {
                        ForEach(model.configKeys.filter { Self.shown($0.key, on: t) && Self.group($0.key, tab: t) == group }, id: \.key) { k in
                            ConfigRow(key: k)
                        }
                    }
                    if t == .general && group == "Files and storage" { DebugToolsSection().id("debug-tools") }
                }
            }
            .formStyle(.grouped)
            .onChange(of: model.configKeys.count, initial: true) { _, count in
                if count > 0, let target = SettingsTab.scrollRequested {
                    SettingsTab.scrollRequested = nil
                    DispatchQueue.main.asyncAfter(deadline: .now() + 0.3) { proxy.scrollTo(target, anchor: .top) }
                }
            }
            }
        }
    }

    static func shown(_ key: String, on tab: SettingsTab) -> Bool {
        SettingsTab.of(key) == tab && !key.hasPrefix("machine.") && !SettingsTab.hidden.contains(key)
    }

    static func isKnownARM64(_ name: String, _ machines: [DaemonAPI.Machine]) -> Bool {
        guard let m = machines.first(where: { $0.name == name }) else { return false }
        return m.image.arch != "amd64"
    }

    private func groups(_ tab: SettingsTab) -> [String] {
        var result: [String] = []
        for key in model.configKeys where Self.shown(key.key, on: tab) {
            let group = Self.group(key.key, tab: tab)
            if !result.contains(group) { result.append(group) }
        }
        if tab == .general {
            return ["Startup and power", "Compatibility", "Files and storage", "Docker"].filter { result.contains($0) }
        }
        return result
    }

    private static func group(_ key: String, tab: SettingsTab) -> String {
        switch tab {
        case .resources: return "Resource limits"
        case .network: return key.hasPrefix("network.proxy") ? "Proxy" : "Connections"
        case .machines: return key.hasPrefix("ssh.") ? "SSH" : "Ports"
        case .kubernetes: return "Cluster"
        default: break
        }
        if key.hasPrefix("docker.") { return "Docker" }
        if key.hasPrefix("emulation.") { return "Compatibility" }
        if key.hasPrefix("files.") || key == "data_allow_backup" { return "Files and storage" }
        return "Startup and power"
    }

}

struct PendingRestartSection: View {
    @Environment(AppModel.self) private var model
    var body: some View {
        Section {
            HStack {
                Image(systemName: "arrow.clockwise.circle.fill").foregroundStyle(.orange).font(.title2)
                VStack(alignment: .leading, spacing: 4) {
                    Text("Restart to apply your changes").fontWeight(.medium)
                    Text(model.pendingRestart.map(ConfigRow.title).joined(separator: ", "))
                        .font(.caption).foregroundStyle(.secondary)
                }
                Spacer()
                Button("Restart Fevom") { Task { await model.restartService() } }
            }
        }
    }
}

struct ConfigRow: View {
    @Environment(AppModel.self) private var model
    let key: DaemonAPI.ConfigKey
    var restartNote = true
    @State private var text = ""
    @State private var newItem = ""

    var value: DaemonAPI.ConfigValue? { model.configValues[key.key] }

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                control
                if value?.isDefault == false {
                    Button { Task { await model.resetConfig(key.key) } } label: {
                        Image(systemName: "arrow.uturn.backward")
                    }
                    .buttonStyle(.borderless)
                    .help("Reset to Default")
                    .accessibilityLabel("Reset \(Self.title(key.key)) to default")
                }
            }
            Text(Self.explanation(key)).font(.caption).foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
            if value?.pending == true {
                Label("Waiting for a restart", systemImage: "arrow.clockwise")
                    .font(.caption).foregroundStyle(.orange)
            } else if restartNote && key.apply == DaemonAPI.Const.applyMachineRestart {
                Text("Applies the next time this machine starts.").font(.caption).foregroundStyle(.secondary)
            }
        }
        .padding(.vertical, 5)
        .contextMenu {
            Button("Copy Command") { copyToPasteboard(AppModel.configCommand(key.key, value?.value)) }
            if value?.isDefault == false {
                Button("Reset to Default") { Task { await model.resetConfig(key.key) } }
            }
        }
        .help("\(key.key): \(key.description)\n\(AppModel.configCommand(key.key, value?.value))")
        .onAppear { text = AppModel.text(value?.value ?? key.default) }
        .onChange(of: value?.value) { _, v in text = AppModel.text(v ?? key.default) }
    }

    @ViewBuilder var control: some View {
        let label = Self.title(key.key)
        switch key.type {
        case "string" where key.key == "emulation.engine" || key.key.hasPrefix("machine.") && key.key.hasSuffix(".emulation"):
            Picker(label, selection: Binding(get: { AppModel.text(value?.value ?? key.default) },
                                            set: { engine in Task { await model.setConfig(key.key, .string(engine)) } })) {
                Text(key.key == "emulation.engine" ? "Automatic (Rosetta and FEX)" : "Use default").tag("auto")
                Text("FEX").tag("fex")
                Text("Rosetta").tag("rosetta")
            }
        case "bool":
            Toggle(label, isOn: Binding(get: { value?.value == .bool(true) },
                                        set: { on in Task { await model.setConfig(key.key, .bool(on)) } }))
        case "int":
            LabeledContent(label) {
                HStack {
                    TextField(label, text: $text).labelsHidden().frame(width: 90).multilineTextAlignment(.trailing)
                        .onSubmit { commitInt() }
                    if let lo = key.min, let hi = key.max {
                        Stepper(label, value: Binding(get: { Int64(text) ?? lo }, set: { text = String(min(max($0, lo), hi)); commitInt() }),
                                in: lo...hi).labelsHidden()
                        if key.key == "memory_mib", let amount = Int64(text) {
                            Text(String(format: "%.1f GiB", Double(amount) / 1024)).font(.caption).foregroundStyle(.secondary).frame(width: 58, alignment: .trailing)
                        }
                    }
                }
            }
        case DaemonAPI.Const.typeList:
            LabeledContent(label) {
                VStack(alignment: .trailing) {
                    ForEach(Self.items(value?.value), id: \.self) { item in
                        HStack {
                            Text(item).font(.callout.monospaced())
                            Button("Remove \(item)", systemImage: "minus.circle") { Task { await model.changeList(key.key, remove: [item]) } }
                                .labelStyle(.iconOnly).buttonStyle(.borderless)
                        }
                    }
                    HStack {
                        TextField("Add", text: $newItem).frame(width: 200)
                            .onSubmit { addItem() }
                        Button("Add to \(label)", systemImage: "plus.circle") { addItem() }
                            .labelStyle(.iconOnly).buttonStyle(.borderless).disabled(newItem.isEmpty)
                    }
                }
            }
        default:
            LabeledContent(label) {
                TextField(label, text: $text).labelsHidden().frame(minWidth: 200)
                    .onSubmit { Task { await model.setConfig(key.key, .string(text)) } }
            }
        }
    }

    func commitInt() {
        guard let v = Int64(text.trimmingCharacters(in: .whitespaces)) else {
            model.lastError = "\(key.key): a whole number"
            return
        }
        if value?.value != .int(v) { Task { await model.setConfig(key.key, .int(v)) } }
    }

    func addItem() {
        let v = newItem.trimmingCharacters(in: .whitespaces)
        guard !v.isEmpty else { return }
        newItem = ""
        Task { await model.changeList(key.key, add: [v]) }
    }

    static func items(_ v: JSONValue?) -> [String] {
        if case .array(let a)? = v { return a.map(AppModel.text) }
        return []
    }

    static func explanation(_ key: DaemonAPI.ConfigKey) -> String {
        let captions: [String: String] = [
            "start_at_login": "Fevom runs in the background; the VM starts only when something needs it.",
            "power.pause_in_sleep": "Containers and machines carry on where they were when your Mac wakes.",
            "emulation.x86": "Runs linux/amd64 images, amd64 machines and 32-bit programs through translation.",
            "emulation.engine": "Automatic uses Rosetta, and FEX for software known to fail under Rosetta. 32-bit programs always use FEX. Each machine can override this, and a container can with the label dev.fevom.emulation.",
            "files.share": "Lets containers bind-mount folders from your Mac.",
            "files.view_hidden": "Removes it from the sidebar and the desktop; the folder stays at ~/Fevom.",
            "data_allow_backup": "Fevom's data is one large disk image, which can make backups much larger.",
            "docker.set_context": "Creates the fev context and selects it, so docker commands use Fevom.",
            "docker.enabled": "When off, docker commands are refused. Linux machines keep running.",
            "cpu": "The most CPU cores the VM can use.",
            "memory_mib": "The most memory the VM can use. Memory it isn't using goes back to your Mac.",
            "network.publish_lan": "Ports published on 127.0.0.1 or ::1 stay on this Mac.",
            "network.host_loopback": "Containers reach servers on your Mac's localhost at host.docker.internal.",
            "network.direct_access": "Needs the admin helper.",
            "network.https": "Certificates come from Fevom's own root certificate, which your Mac trusts after fev setup --https.",
            "network.proxy": "For image pulls and for container and machine traffic. auto follows your Mac's settings, none turns it off, or enter a proxy URL.",
            "network.proxy_exclude": "Hosts, domains and IP ranges, in addition to your Mac's own exceptions.",
            "network.proxy.transparent": "TCP connections go through the proxy without proxy settings inside the container or machine.",
            "admin.enabled": "For direct IP access, fev.local names on this Mac and ports below 1024. Install it with sudo fev setup --admin.",
            "machines.forward_ports": "Ports a machine listens on are reachable at localhost.",
            "machines.expose_ports_to_lan": "For ports a machine listens on at all addresses (0.0.0.0 or ::).",
            "ssh.expose_port": "When off, only this Mac can connect.",
            "kubernetes.enabled": "A single-node cluster beside Docker that can run your Docker images. The first start downloads about 250 MB; while on, it uses about a tenth of a CPU core and up to 2 GiB of memory.",
            "kubernetes.expose_services_to_lan": "When off, NodePort services are reachable from this Mac only.",
            "kubernetes.metrics_server": "Needed for kubectl top."
        ]
        if let c = captions[key.key] { return c }
        let parts = key.key.split(separator: ".")
        if key.key.hasPrefix("machine."), parts.count >= 3 {
            let name = parts[1]
            switch parts[2] {
            case "emulation": return "Use default follows the setting in General."
            case "username": return "Used by fev run, fev and ssh when no user is given."
            case "isolated": return "It gets its own user namespace, no access to your Mac's files except folders you add, and no mac command."
            case "isolate_network": return "An isolated machine can't reach other machines, containers, your Mac or its published ports."
            case "mounts": return "Each as SOURCE or SOURCE:DESTINATION; the destination defaults to the same path."
            case "forward_ssh_agent": return "Gives an isolated machine your Mac's SSH agent."
            case "http_port": return "The port \(name).fev.local serves, if the machine serves plain HTTP. 0 detects it."
            case "https_port": return "The port https://\(name).fev.local uses if the machine serves TLS itself. 0 detects it."
            default: break
            }
        }
        let d = key.description
        return d.hasSuffix(".") || d.isEmpty ? d : d + "."
    }

    static func title(_ key: String) -> String {
        let titles: [String: String] = [
            "start_at_login": "Start Fevom at login",
            "power.pause_in_sleep": "Pause the VM while your Mac sleeps",
            "emulation.x86": "Run x86 Linux software",
            "emulation.engine": "x86-64 translator",
            "files.share": "Share Mac files with the VM",
            "files.view_hidden": "Hide ~/Fevom in Finder",
            "data_allow_backup": "Include Fevom's data in Time Machine",
            "docker.set_context": "Use Fevom as the Docker context",
            "docker.enabled": "Run the Docker engine",
            "docker.node_name": "Docker host name",
            "cpu": "CPUs",
            "memory_mib": "Memory (MiB)",
            "network.publish_lan": "Let other devices reach published ports",
            "network.host_loopback": "Let containers reach your Mac's localhost",
            "network.direct_access": "Reach containers and machines by IP address",
            "network.https": "HTTPS for fev.local names",
            "network.proxy": "Proxy",
            "network.proxy_exclude": "Bypass the proxy for",
            "network.proxy.transparent": "Send all container and machine traffic through the proxy",
            "admin.enabled": "Use the admin helper",
            "machines.forward_ports": "Forward machines' ports to your Mac",
            "machines.expose_ports_to_lan": "Let other devices reach machines' ports",
            "ssh.port": "SSH port",
            "ssh.expose_port": "Let other devices reach the SSH server",
            "kubernetes.enabled": "Enable Kubernetes",
            "kubernetes.expose_services_to_lan": "Let other devices reach NodePort services",
            "kubernetes.metrics_server": "Run metrics-server"
        ]
        if let title = titles[key] { return title }
        let machine: [String: String] = ["emulation": "x86-64 translator", "username": "Default user", "isolated": "Isolate this machine",
                                         "isolate_network": "Internet access only", "mounts": "Mac folders", "forward_ssh_agent": "Forward SSH agent",
                                         "http_port": "HTTP port", "https_port": "HTTPS port"]
        let parts = key.split(separator: ".").map(String.init)
        if key.hasPrefix("machine."), parts.count >= 3, let t = machine[parts[2]] { return t }
        let last = parts.last ?? key
        let named: [String: String] = ["lan": "LAN", "ssh": "SSH", "http": "HTTP", "https": "HTTPS", "url": "URL", "ip": "IP", "dns": "DNS", "cpu": "CPU"]
        return last.split(separator: "_").map(String.init).enumerated()
            .map { i, w in named[w] ?? (i == 0 ? w.prefix(1).uppercased() + w.dropFirst() : w) }.joined(separator: " ")
    }
}

struct NetworkStatusSection: View {
    @Environment(AppModel.self) private var model
    @State private var confirm: String?

    var body: some View {
        Section("Direct access") {
            if let n = model.status?.network {
                LabeledContent("State", value: n.reason.isEmpty ? Self.state(n.directAccess) : "\(Self.state(n.directAccess)): \(Self.reason(n.reason))")
                LabeledContent("Addresses", value: [n.subnet, n.subnet6].filter { !$0.isEmpty }.joined(separator: ", "))
                if !n.interface.isEmpty { LabeledContent("Interface", value: n.interface) }
                ForEach(n.conflicts, id: \.route) { c in
                    LabeledContent("Conflict", value: "\(c.route) on \(c.interface) overlaps \(c.prefix)\(c.withdrawn ? " (withdrawn)" : "")")
                }
                if let h = n.helper {
                    LabeledContent("Admin helper", value: "Version \(h.protocolVersion), level \(h.securityLevel)\(h.outdated ? ", needs updating" : "")")
                }
                if n.reason == DaemonAPI.Const.directNoHelper || n.helper?.outdated == true {
                    CommandLine(text: Onboarding.adminCommand)
                }
            } else {
                Text("Not reported while Fevom is not running.").foregroundStyle(.secondary)
            }
        }
        if let d = model.status?.domains {
            Section("Names") {
                LabeledContent("fev.local on this Mac", value: Self.resolver(d.resolver))
                LabeledContent("Registered names", value: "\(d.names)")
                ForEach(d.collisions, id: \.self) { Text($0).foregroundStyle(.orange) }
            }
        }
        Section("HTTPS") {
            if let h = model.status?.https {
                LabeledContent("Root certificate", value: Self.trust(h.trust))
                if !h.root.isEmpty { LabeledContent("SHA-256", value: String(h.root.prefix(16)) + "…").help(h.root) }
                LabeledContent("Key", value: Self.key(h.key))
                if !h.error.isEmpty { Text(h.error).font(.caption).foregroundStyle(.orange) }
                HStack {
                    if h.trust != DaemonAPI.Const.trustTrusted {
                        Button("Trust Fevom's Root…") { Task { await model.trustHTTPS(DaemonAPI.Const.trustInstall) } }
                    } else {
                        Button("Remove Trust…") { confirm = DaemonAPI.Const.trustRemove }
                    }
                    Button("Make a New Root…") { confirm = DaemonAPI.Const.trustRotate }
                }
                CommandLine(text: Onboarding.httpsCommand)
            } else {
                Text("Not reported while Fevom is not running.").foregroundStyle(.secondary)
            }
        }
        .confirmationDialog(confirm == DaemonAPI.Const.trustRotate ? "Make a new root?" : "Stop trusting Fevom's root certificate?",
                            isPresented: Binding(get: { confirm != nil }, set: { if !$0 { confirm = nil } })) {
            Button(confirm == DaemonAPI.Const.trustRotate ? "Make a New Root" : "Remove Trust", role: .destructive) {
                let a = confirm ?? ""
                Task { await model.trustHTTPS(a) }
            }
            Button("Cancel", role: .cancel) {}
        } message: {
            Text(confirm == DaemonAPI.Const.trustRotate
                 ? "The old root's trust and key are removed, then a new root is trusted: two dialogs. Certificates made with the old root stop being trusted."
                 : "fev.local names served over HTTPS are no longer trusted by this Mac's browsers. One dialog.")
        }
    }

    static func reason(_ r: String) -> String {
        switch r {
        case DaemonAPI.Const.directSetting: "turned off below"
        case DaemonAPI.Const.directAdminOff: "the admin helper is turned off below"
        case DaemonAPI.Const.directNoHelper: "the admin helper isn't installed"
        case DaemonAPI.Const.directOutdated: "the admin helper needs updating"
        case DaemonAPI.Const.directOtherUser: "the admin helper belongs to another user"
        case DaemonAPI.Const.directHelperOff: "the admin helper is turned off in Login Items"
        default: r.replacingOccurrences(of: "_", with: " ")
        }
    }

    static func state(_ s: String) -> String {
        switch s {
        case DaemonAPI.Const.directOn: "On"
        case DaemonAPI.Const.directOff: "Off"
        case DaemonAPI.Const.directDegraded: "Limited"
        case DaemonAPI.Const.directFailed: "Failed"
        default: s.prefix(1).uppercased() + s.dropFirst().replacingOccurrences(of: "_", with: " ")
        }
    }

    static func resolver(_ r: String) -> String {
        switch r {
        case "fevs": "Set up"
        case "absent": "Not set up"
        case "foreign": "Taken by another app"
        default: state(r)
        }
    }

    static func trust(_ t: String) -> String {
        switch t {
        case DaemonAPI.Const.trustTrusted: "Trusted by this Mac"
        case "untrusted": "Not trusted"
        case "absent": "Not created yet"
        case "declined": "Not trusted (declined)"
        default: state(t)
        }
    }

    static func key(_ k: String) -> String {
        switch k {
        case DaemonAPI.Const.keyIdentity, DaemonAPI.Const.keySigner: "In your login keychain"
        case DaemonAPI.Const.keyFile: "In a file"
        case DaemonAPI.Const.keyNone: "Not created yet"
        case DaemonAPI.Const.keyUnusable: "Not usable by this build"
        default: state(k)
        }
    }
}

struct CommandLine: View {
    let text: String
    var body: some View {
        HStack {
            Text(text).font(.callout.monospaced()).textSelection(.enabled)
            Spacer()
            Button("Copy \(text)", systemImage: "doc.on.doc") { copyToPasteboard(text) }
                .labelStyle(.iconOnly).buttonStyle(.borderless)
        }
    }
}

struct EngineConfigView: View {
    @Environment(AppModel.self) private var model
    @State private var config: AppModel.EngineConfig?
    @State private var text = ""
    @State private var verdict: String?
    @State private var saving = false

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            if let c = config {
                Text(c.path).font(.caption.monospaced()).foregroundStyle(.secondary).textSelection(.enabled)
                TextEditor(text: $text)
                    .font(.body.monospaced())
                    .frame(minHeight: 260)
                    .accessibilityLabel("The Docker engine's configuration, JSON")
                if let p = AppModel.engineConfigProblem(text) {
                    Text(p).font(.caption).foregroundStyle(.red)
                } else if c.state == DaemonAPI.Const.engineConfigRefused, text == c.text {
                    Text("The Docker engine refused this file: \(c.error). It keeps its last working configuration.").font(.caption).foregroundStyle(.orange)
                } else if let verdict {
                    Text(verdict).font(.caption).foregroundStyle(.secondary)
                }
                HStack {
                    Button("Revert") { text = c.text; verdict = nil }.disabled(text == c.text)
                    Spacer()
                    CommandLine(text: "fev config docker").frame(maxWidth: 260)
                    Button("Save and Restart Engine") {
                        saving = true
                        Task {
                            verdict = await model.saveEngineConfig(text, path: c.path)
                            config = await model.loadEngineConfig()
                            saving = false
                        }
                    }
                    .keyboardShortcut("s", modifiers: .command)
                    .disabled(saving || text == c.text || AppModel.engineConfigProblem(text) != nil)
                }
            } else {
                Text(model.link == .connected ? "Reading the engine's configuration…" : "The engine's configuration is read from fev while it runs.")
                    .foregroundStyle(.secondary)
            }
        }
        .padding()
        .task(id: model.link == .connected) {
            guard model.link == .connected, config == nil else { return }
            config = await model.loadEngineConfig()
            text = config?.text ?? ""
        }
    }
}

struct AppPreferencesView: View {
    let onboarding: () -> Void
    let statusItem: (Bool) -> Void
    @State private var showItem = AppPreferences().showStatusItem
    @State private var lines = AppPreferences().logLines
    @State private var mib = AppPreferences().logMiB
    @State private var checks = Updates.shared.checksAutomatically
    @State private var installs = Updates.shared.installsAutomatically

    var body: some View {
        Form {
            if Updates.shared.available {
                Section("Updates") {
                    Toggle("Check for updates every day", isOn: $checks)
                        .onChange(of: checks) { _, v in Updates.shared.checksAutomatically = v }
                    Toggle("Install updates without asking", isOn: $installs)
                        .onChange(of: installs) { _, v in Updates.shared.installsAutomatically = v }
                        .disabled(!checks)
                    Button("Check for Updates…") { Updates.shared.check() }
                    Text("Installing an update restarts Fevom. Running containers stop (those with a restart policy start again) and running machines restart.")
                        .font(.caption).foregroundStyle(.secondary)
                }
            }
            Section("Menu bar") {
                Toggle("Show Fevom in the menu bar", isOn: $showItem)
                    .onChange(of: showItem) { _, v in AppPreferences().showStatusItem = v; statusItem(v) }
            }
            Section("Logs") {
                Stepper("Keep up to \(lines.formatted()) lines", value: $lines, in: 1_000...2_000_000, step: 50_000)
                    .onChange(of: lines) { _, v in AppPreferences().logLines = v }
                Stepper("and up to \(mib) MiB per log window", value: $mib, in: 8...1024, step: 8)
                    .onChange(of: mib) { _, v in AppPreferences().logMiB = v }
                Text("Applies to log windows opened after the change.").font(.caption).foregroundStyle(.secondary)
            }
            Section("Setup") {
                Button("Set Up Fevom…", action: onboarding)
                Text("Sets up the command line, the admin helper and HTTPS trust.").font(.caption).foregroundStyle(.secondary)
            }
        }
        .formStyle(.grouped)
    }
}

struct DebugToolsSection: View {
    @Environment(AppModel.self) private var model
    @State private var confirming = false

    var body: some View {
        Section("Debug tools") {
            if let d = model.debug, d.exists {
                LabeledContent("Size") {
                    Text(ByteCountFormatter.string(fromByteCount: d.bytes, countStyle: .file)).monospacedDigit()
                }
                LabeledContent("Packages") {
                    Text("\(d.packages.count) installed\(d.sessions > 0 ? ", \(d.sessions) session\(d.sessions == 1 ? "" : "s") open" : "")")
                        .foregroundStyle(.secondary)
                }
                Button("Reset Debug Tools…", role: .destructive) { confirming = true }
                    .disabled(d.sessions > 0)
                    .help(d.sessions > 0 ? "A debug session is open; close it first." : "Delete the downloaded debug tools and free their space.")
            } else {
                VStack(alignment: .leading, spacing: 8) {
                    LabeledContent("Size") {
                        Text("Not downloaded yet").foregroundStyle(.secondary)
                    }
                }
                .padding(.vertical, 5)
            }
        }
        .task { await model.loadDebugStatus() }
        .confirmationDialog("Reset the debug tools?", isPresented: $confirming) {
            Button("Reset Debug Tools", role: .destructive) { Task { await model.resetDebugStore() } }
            Button("Cancel", role: .cancel) {}
        } message: {
            Text("This frees \(ByteCountFormatter.string(fromByteCount: model.debug?.bytes ?? 0, countStyle: .file)). The next debug session downloads the default tools again.")
        }
    }
}

struct MachineSettingsSheet: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    let name: String

    var body: some View {
        VStack(spacing: 0) {
            Text("\(name) Settings").font(.headline).padding(.top, 16)
            Form {
                if model.configKeys.isEmpty {
                    Text(model.link == .connected ? "Reading settings…" : "Start Fevom to view and change its settings.")
                        .foregroundStyle(.secondary)
                }
                Section("Machine") {
                    ForEach(keys(["username", "emulation", "http_port", "https_port"]), id: \.key) { ConfigRow(key: $0, restartNote: false) }
                }
                Section("Isolation") {
                    ForEach(keys(isolated ? ["isolated", "isolate_network", "mounts", "forward_ssh_agent"] : ["isolated"]), id: \.key) {
                        ConfigRow(key: $0, restartNote: false)
                    }
                }
            }
            .formStyle(.grouped)
            Divider()
            HStack {
                Text("The default user changes at once; the rest apply the next time \(name) starts.")
                    .font(.caption).foregroundStyle(.secondary)
                Spacer()
                Button("Done") { dismiss() }.keyboardShortcut(.defaultAction)
            }
            .padding(16)
        }
        .frame(width: 600, height: 560)
        .onAppear { model.watchConfig() }
        .onDisappear { model.unwatchConfig() }
    }

    var isolated: Bool { model.configValues["machine.\(name).isolated"]?.value == .bool(true) }

    func keys(_ names: [String]) -> [DaemonAPI.ConfigKey] {
        names.compactMap { n in
            if n == "emulation" && SettingsView.isKnownARM64(name, model.machines) { return nil }
            return model.configKeys.first { $0.key == "machine.\(name).\(n)" }
        }
    }
}
