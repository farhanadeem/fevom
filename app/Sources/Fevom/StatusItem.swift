import AppKit
import FevomAPI
import FevomDocker
import FevomStores
import Observation

@MainActor
final class StatusItemController: NSObject, NSMenuDelegate {
    let model: AppModel
    let openWindow: (SidebarItem?) -> Void
    let openSettings: () -> Void
    let openLogs: () -> Void
    private var item: NSStatusItem?
    private let menu = NSMenu()
    private var open = false

    init(model: AppModel, open: @escaping (SidebarItem?) -> Void, settings: @escaping () -> Void, logs: @escaping () -> Void = {}) {
        self.model = model
        self.openWindow = open
        self.openSettings = settings
        self.openLogs = logs
        super.init()
        menu.delegate = self
        menu.autoenablesItems = false
    }

    func setVisible(_ on: Bool) {
        if on, item == nil {
            let i = NSStatusBar.system.statusItem(withLength: NSStatusItem.squareLength)
            i.button?.image = Brand.menuBar
            i.button?.setAccessibilityLabel("Fevom")
            i.menu = menu
            item = i
        } else if !on, let i = item {
            NSStatusBar.system.removeStatusItem(i)
            item = nil
        }
    }

    var isVisible: Bool { item != nil }

    func menuWillOpen(_ menu: NSMenu) {
        open = true
        model.show()
        model.watchForwards()
        rebuild()
    }

    func menuDidClose(_ menu: NSMenu) {
        open = false
        model.unwatchForwards()
        model.hide()
    }

    private func rebuild() {
        guard open else { return }
        withObservationTracking {
            build()
        } onChange: {
            Task { @MainActor [weak self] in self?.rebuild() }
        }
    }

    private func build() {
        menu.removeAllItems()
        let state = NSMenuItem(title: ServiceLine.describe(link: model.link, status: model.status), action: nil, keyEquivalent: "")
        state.isEnabled = false
        menu.addItem(state)
        let running = model.status?.state == DaemonAPI.Const.stateRunning
        menu.addItem(action(running ? "Stop" : "Start") { [model] in
            Task { running ? await model.stopService() : await model.startService() }
        })
        if running {
            menu.addItem(action("Restart") { [model] in Task { await model.restartService() } })
        }
        if !model.pendingRestart.isEmpty || !(model.status?.pendingRestart ?? []).isEmpty {
            let mi = NSMenuItem(title: "Restart Fevom to apply settings", action: nil, keyEquivalent: "")
            mi.isEnabled = false
            menu.addItem(mi)
        }
        if !model.containers.isEmpty {
            menu.addItem(.separator())
            menu.addItem(header("Containers"))
            for row in ContainerRow.rows(model.containers).prefix(20) {
                if let p = row.project {
                    let mi = NSMenuItem(title: p, action: nil, keyEquivalent: "")
                    mi.image = NSImage(systemSymbolName: "square.stack", accessibilityDescription: "Compose project, \(row.state)")
                    let sub = NSMenu()
                    let up = row.state != "exited"
                    sub.addItem(action(up ? "Stop" : "Start") { [model] in Task { await model.project(p, up ? "stop" : "start") } })
                    sub.addItem(action("Restart") { [model] in Task { await model.project(p, "restart") } })
                    sub.addItem(.separator())
                    for child in row.children ?? [] {
                        if let c = child.container { sub.addItem(containerItem(c, title: child.name)) }
                    }
                    mi.submenu = sub
                    menu.addItem(mi)
                } else if let c = row.container {
                    menu.addItem(containerItem(c, title: c.name))
                }
            }
        }
        if !model.machines.isEmpty {
            menu.addItem(.separator())
            menu.addItem(header("Machines"))
            for m in model.machines {
                let mi = NSMenuItem(title: m.name, action: nil, keyEquivalent: "")
                let up = m.state == DaemonAPI.Const.machineRunning
                mi.image = NSImage(systemSymbolName: up ? "play.circle.fill" : "stop.circle", accessibilityDescription: m.state)
                let sub = NSMenu()
                sub.addItem(action(up ? "Stop" : "Start") { [model] in Task { await model.machine(m.name, start: !up) } })
                sub.addItem(action("Copy SSH Command") { copyToPasteboard("ssh \(m.name)@fev") })
                mi.submenu = sub
                menu.addItem(mi)
            }
        }
        let ports = model.forwards.filter { $0.state == "active" }.sorted { ($0.mac.first ?? "") < ($1.mac.first ?? "") }
        if !ports.isEmpty {
            menu.addItem(.separator())
            menu.addItem(header("Ports"))
            for f in ports.prefix(20) {
                let port = f.mac.first.flatMap { $0.split(separator: ":").last }.map(String.init) ?? ""
                let who = f.source == "docker" ? (model.containers.first { $0.id == f.owner }?.name ?? String(f.owner.prefix(12))) : f.owner
                let mi = action("localhost:\(port) (\(f.proto)) → \(who)") {
                    if f.proto == "tcp", let u = URL(string: "http://localhost:\(port)") { NSWorkspace.shared.open(u) }
                }
                mi.toolTip = (f.mac + ["to " + f.guest]).joined(separator: ", ")
                menu.addItem(mi)
            }
        }
        let mounts = MenuMounts.items(view: model.status?.view, machines: model.machines, containers: model.containers)
        if !mounts.isEmpty {
            menu.addItem(.separator())
            menu.addItem(header("Files"))
            for (i, f) in mounts.enumerated() {
                let mi: NSMenuItem
                if f.path.isEmpty {
                    mi = NSMenuItem(title: f.title, action: nil, keyEquivalent: "")
                    mi.isEnabled = false
                } else {
                    mi = action(f.title) { NSWorkspace.shared.open(URL(fileURLWithPath: f.path, isDirectory: true)) }
                    mi.toolTip = "Open \(f.path) in Finder"
                    mi.image = NSImage(systemSymbolName: i == 0 ? "externaldrive.connected.to.line.below" : "folder", accessibilityDescription: nil)
                    if i > 0 { mi.indentationLevel = 1 }
                }
                menu.addItem(mi)
            }
        }
        menu.addItem(.separator())
        menu.addItem(action("Open Fevom") { [openWindow] in openWindow(nil) })
        menu.addItem(action("Activity") { [openWindow] in openWindow(.activity) })
        menu.addItem(action("Fevom's Log") { [openLogs] in openLogs() })
        if Updates.shared.available { menu.addItem(action("Check for Updates…") { Updates.shared.check() }) }
        menu.addItem(action("Settings…") { [openSettings] in openSettings() })
        menu.addItem(action("Quit Fevom") { NSApp.terminate(nil) })
    }

    private func containerItem(_ c: Container, title: String) -> NSMenuItem {
        let mi = NSMenuItem(title: title, action: nil, keyEquivalent: "")
        mi.image = NSImage(systemSymbolName: c.state == "running" ? "play.circle.fill" : "stop.circle", accessibilityDescription: c.state)
        let sub = NSMenu()
        sub.addItem(action(c.state == "running" ? "Stop" : "Start") { [model] in
            Task { await model.container(c.id, c.state == "running" ? "stop" : "start") }
        })
        if let port = c.ports.compactMap(\.publicPort).sorted().first {
            sub.addItem(action("Open localhost:\(port)") { NSWorkspace.shared.open(URL(string: "http://localhost:\(port)")!) })
        }
        sub.addItem(action("Copy ID") { copyToPasteboard(c.id) })
        sub.addItem(action("Copy Domain") { copyToPasteboard(c.domains[0]) })
        mi.submenu = sub
        return mi
    }

    private func header(_ title: String) -> NSMenuItem {
        if #available(macOS 14, *) { return NSMenuItem.sectionHeader(title: title) }
        let mi = NSMenuItem(title: title, action: nil, keyEquivalent: "")
        mi.isEnabled = false
        return mi
    }

    private func action(_ title: String, _ run: @escaping () -> Void) -> NSMenuItem {
        let mi = NSMenuItem(title: title, action: #selector(Target.fire(_:)), keyEquivalent: "")
        let t = Target(run)
        mi.target = t
        mi.representedObject = t
        return mi
    }
}

final class Target: NSObject {
    let run: () -> Void
    init(_ run: @escaping () -> Void) { self.run = run }
    @objc func fire(_ sender: Any?) { run() }
}
