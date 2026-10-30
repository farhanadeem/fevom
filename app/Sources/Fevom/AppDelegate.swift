import AppKit
import FevomAPI
import FevomStores
import ServiceManagement
import SwiftUI
@preconcurrency import UserNotifications

enum BuildInfo {
    static var id: String {
        Bundle.main.object(forInfoDictionaryKey: "FevomBuild") as? String ?? "dev"
    }
}

@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate {
    let model = AppModel(place: DaemonPlace.resolve(build: BuildInfo.id))
    let nav = Navigation(section: SidebarItem(rawValue: DevHooks.value("FEV_APP_SECTION") ?? "") ?? .containers)
    lazy var window: MainWindowController = MainWindowController(model: model, nav: nav, shell: shell)
    lazy var statusItem: StatusItemController = StatusItemController(model: model, open: { [weak self] section in
        if let section { self?.nav.section = section }
        self?.window.present()
    }, settings: { [weak self] in self?.settings.present() }, logs: { [weak self] in self?.openFevLog() })
    lazy var onboarding: Onboarding = {
        let o = Onboarding(home: model.place.home, cli: Onboarding.findCLI(fevd: model.place.fevd),
                           environment: DaemonPlace.honoursEnvironment ? nil : Onboarding.releaseEnvironment(lang: ProcessInfo.processInfo.environment["LANG"]))
        o.runAsAdmin = { try await AdminPrompt.run($0) }
        return o
    }()
    lazy var onboardingWindow: OnboardingWindowController = OnboardingWindowController(model: model, onboarding: onboarding)
    lazy var settings: SettingsWindowController = SettingsWindowController(model: model, onboarding: { [weak self] in self?.onboardingWindow.present() },
                                                 statusItem: { [weak self] on in self?.statusItem.setVisible(on) })
    let commands = CommandsWindowController()
    private var windows: [ObjectIdentifier: NSObject] = [:]

    var shell: Shell {
        Shell(openLogs: { [weak self] feed in self?.openLogs(feed) },
              openTerminal: { [weak self] machine in self?.openTerminal(machine) },
              openContainerTerminal: { [weak self] id, name in self?.openContainerTerminal(id, name: name) },
              openDebugShell: { [weak self] target, kind, readOnly, context, name in self?.openDebugShell(target, kind: kind, readOnly: readOnly, context: context, name: name) },
              openK8sTerminal: { [weak self] ns, pod, container in self?.openK8sTerminal(ns, pod: pod, container: container) })
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        model.notify = { notice in Notifier.post(notice) }
        if !DaemonPlace.honoursEnvironment {
            model.onStartAtLogin = { on in LoginItem.set(on) }
        }
        NSApp.mainMenu = MainMenu.build(.init(
            settings: { [weak self] in self?.settings.present() },
            checkForUpdates: Updates.shared.available ? { Updates.shared.check() } : nil,
            onboarding: { [weak self] in self?.onboardingWindow.present() },
            uninstall: { [weak self] in self?.uninstall() },
            commands: { [weak self] in self?.commands.present() },
            open: { [weak self] in self?.window.present() },
            section: { [weak self] s in self?.nav.section = s; self?.window.present() },
            newMachine: { [weak self] in
                self?.nav.section = .machines
                self?.window.present()
                self?.nav.newMachine += 1
            },
            find: { [weak self] in self?.nav.find += 1 },
            start: { [model] in Task { await model.startService() } },
            stop: { [model] in Task { await model.stopService() } },
            restart: { [model] in Task { await model.restartService() } },
            logs: { [weak self] in self?.openFevLog() }))
        statusItem.setVisible(AppPreferences().showStatusItem)
        model.opened()
        window.present()
        if onboarding.needed || DevHooks.value("FEV_APP_ONBOARDING") != nil { onboardingWindow.present() }
        DevHooks.arm(window: window, app: self)
    }

    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        model.opened()
        window.present()
        return false
    }

    var leavesFevom = false
    private var quitting = false

    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        if leavesFevom { return .terminateNow }
        if quitting { return .terminateCancel }
        quitting = true
        Task { [model] in
            await model.stopService()
            NSApp.reply(toApplicationShouldTerminate: true)
        }
        return .terminateLater
    }

    func applicationWillTerminate(_ notification: Notification) {
        Measure.writeRunLoopLog()
    }

    func application(_ application: NSApplication, open urls: [URL]) {
        for url in urls where url.scheme == "fev" && url.host == "update" {
            Updates.shared.check()
        }
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { false }

    func openLogs(_ feed: LogFeed) {
        let w = LogWindowController(feed: feed)
        let key = ObjectIdentifier(w)
        windows[key] = w
        w.onClose = { [weak self] in self?.windows[key] = nil }
        w.show()
    }

    func openFevLog() { openLogs(.fevs(model.place)) }

    @discardableResult
    func openTerminal(_ machine: String) -> TerminalWindowController {
        keep(TerminalWindowController(place: model.place, machine: machine))
    }

    @discardableResult
    func openContainerTerminal(_ id: String, name: String) -> TerminalWindowController {
        keep(TerminalWindowController(place: model.place, container: id, name: name))
    }

    @discardableResult
    func openDebugShell(_ target: String, kind: String, readOnly: Bool, context: String = "", name: String) -> TerminalWindowController {
        keep(TerminalWindowController(place: model.place, debugTarget: target, kind: kind, readOnly: readOnly, context: context, name: name))
    }

    @discardableResult
    func openK8sTerminal(_ namespace: String, pod: String, container: String) -> TerminalWindowController {
        let t = keep(TerminalWindowController(stream: TerminalSession(place: model.place, k8s: .init(namespace: namespace, pod: pod, container: container)), name: pod))
        DevHooks.lastTerminal = t
        return t
    }

    private func keep(_ w: TerminalWindowController) -> TerminalWindowController {
        let key = ObjectIdentifier(w)
        windows[key] = w
        w.onClose = { [weak self] in self?.windows[key] = nil }
        w.show()
        return w
    }
}

@MainActor
enum Notifier {
    private static var asked = false

    static func post(_ notice: AppNotice) {
        guard Bundle.main.bundleIdentifier != nil else { return }
        let center = UNUserNotificationCenter.current()
        let content = UNMutableNotificationContent()
        content.title = notice.title
        content.body = notice.body
        let request = UNNotificationRequest(identifier: UUID().uuidString, content: content, trigger: nil)
        if asked {
            center.add(request)
            return
        }
        asked = true
        center.requestAuthorization(options: [.alert, .sound]) { granted, _ in
            if granted { center.add(request) }
        }
    }
}

@MainActor
final class MainWindowController: NSObject, NSWindowDelegate {
    let model: AppModel
    let nav: Navigation
    let shell: Shell
    private(set) var window: NSWindow?
    private var viewing = false
    private var observers: [NSObjectProtocol] = []

    init(model: AppModel, nav: Navigation, shell: Shell) {
        self.model = model
        self.nav = nav
        self.shell = shell
        super.init()
        let nc = NotificationCenter.default
        observers.append(nc.addObserver(forName: NSApplication.didHideNotification, object: nil, queue: .main) { [weak self] _ in
            MainActor.assumeIsolated { self?.setViewing(false) }
        })
        observers.append(nc.addObserver(forName: NSApplication.didUnhideNotification, object: nil, queue: .main) { [weak self] _ in
            MainActor.assumeIsolated {
                guard let self, let w = self.window, w.isVisible, !w.isMiniaturized else { return }
                self.setViewing(true)
            }
        })
    }

    func present() {
        if window == nil {
            let host = NSHostingController(rootView: MainView().environment(model).environment(nav).environment(\.shell, shell).frame(minWidth: 900, minHeight: 560))
            let w = NSWindow(contentViewController: host)
            w.title = nav.section.title
            let available = NSScreen.main?.visibleFrame.size ?? NSSize(width: 1200, height: 800)
            w.setContentSize(NSSize(width: min(1160, available.width - 60), height: min(740, available.height - 60)))
            w.styleMask.insert(.fullSizeContentView)
            w.toolbarStyle = .unified
            w.isReleasedWhenClosed = false
            w.isRestorable = false
            w.setFrameAutosaveName("main")
            w.delegate = self
            window = w
        }
        DevHooks.show(window)
        setViewing(true)
    }

    private func setViewing(_ on: Bool) {
        guard on != viewing else { return }
        viewing = on
        model.mainWindow(shown: on)
        if on { model.show() } else { model.hide() }
    }

    func windowWillClose(_ notification: Notification) {
        model.mainWindowClosed()
        setViewing(false)
        if AppPreferences().showStatusItem, !DevHooks.unattended { NSApp.setActivationPolicy(.accessory) }
        DispatchQueue.main.async { [weak self] in
            if let self, let w = self.window, !w.isVisible {
                w.contentViewController = nil
                self.window = nil
            }
        }
    }

    func windowDidMiniaturize(_ notification: Notification) { setViewing(false) }
    func windowDidDeminiaturize(_ notification: Notification) { setViewing(true) }
}

@MainActor
enum DevHooks {
    static var lastTerminal: TerminalWindowController?

    static func typeInto(pod: String, text: String) {
        lastTerminal?.session.send(Array((text + "\n").utf8))
    }

    static func value(_ name: String) -> String? {
        DaemonPlace.honoursEnvironment ? ProcessInfo.processInfo.environment[name] : nil
    }

    static let unattended = value("FEV_APP_EXIT_AFTER") != nil || value("FEV_APP_QUIT_AFTER") != nil

    static var unattendedActivity: NSObjectProtocol?

    static func show(_ window: NSWindow?, regular: Bool = true) {
        guard let window else { return }
        if unattended {
            window.alphaValue = 0
            window.hasShadow = false
            window.ignoresMouseEvents = true
            window.orderFrontRegardless()
        } else {
            if regular { NSApp.setActivationPolicy(.regular) }
            window.makeKeyAndOrderFront(nil)
            NSApp.activate()
        }
    }

    static func arm(window: MainWindowController, app: AppDelegate) {
        if let file = value("FEV_APP_RUNLOOP_LOG") { Measure.startRunLoopLog(file) }
        if let spec = value("FEV_APP_SCROLL")?.split(separator: ","), spec.count == 2, let at = Double(spec[0]), let secs = Double(spec[1]) {
            Measure.scroll(window.window, at: at, for: secs)
        }
        if let a = value("FEV_APP_APPEARANCE") {
            NSApp.appearance = NSAppearance(named: a == "dark" ? .darkAqua : .aqua)
        }
        var extra: NSWindow?
        if let open = value("FEV_APP_OPEN") {
            let (kind, arg) = open.split(separator: ":", maxSplits: 1).map(String.init).splitPair
            let before = Set(NSApp.windows.map(ObjectIdentifier.init))
            if kind == "logs" && arg == "fev" {
                app.openFevLog()
            } else if kind == "logs" && arg.hasPrefix("machine:") {
                let name = String(arg.dropFirst("machine:".count))
                app.openLogs(.bounded(title: name, place: app.model.place, daemonSources: [DaemonAPI.machineConsoleLog(name), DaemonAPI.machineRuntimeLog(name)]))
            } else if kind == "logs" {
                app.openLogs(LogFeed.bounded(title: arg, socketPath: DaemonAPI.dockerSocketPath(home: app.model.place.home), sources: [.init(id: arg)]))
            } else if kind == "machine-settings" {
                app.nav.machineSettings = arg
            } else if kind == "terminal" || kind == "exec" || kind == "debug" || kind == "debugimage" {
                let t: TerminalWindowController
                switch kind {
                case "exec": t = app.openContainerTerminal(arg, name: arg)
                case "debug": t = app.openDebugShell(arg, kind: "container", readOnly: false, name: arg)
                case "debugimage": t = app.openDebugShell(arg, kind: "image", readOnly: false, name: arg)
                default: t = app.openTerminal(arg)
                }
                if let text = value("FEV_APP_TYPE") {
                    DispatchQueue.main.asyncAfter(deadline: .now() + 1.5) { t.session.send(Array((text + "\n").utf8)) }
                }
            }
            extra = NSApp.windows.first { !before.contains(ObjectIdentifier($0)) }
        }
        if let tab = value("FEV_APP_SETTINGS"), let t = SettingsTab(rawValue: tab) {
            SettingsTab.scrollRequested = value("FEV_APP_SETTINGS_SCROLL")
            app.settings.present(tab: t)
            extra = app.settings.window
        }
        if let path = value("FEV_APP_SNAPSHOT") {
            let after = Double(value("FEV_APP_SNAPSHOT_AFTER") ?? "") ?? 3
            DispatchQueue.main.asyncAfter(deadline: .now() + after) {
                snapshot(window.window, to: path)
                if value("FEV_APP_SETTINGS") != nil { snapshot(app.settings.window, to: path.replacingOccurrences(of: ".png", with: "-settings.png")) }
                if value("FEV_APP_ONBOARDING") != nil { snapshot(app.onboardingWindow.window, to: path.replacingOccurrences(of: ".png", with: "-onboarding.png")) }
                if value("FEV_APP_OPEN")?.hasPrefix("machine-settings:") == true, let sheet = window.window?.attachedSheet {
                    snapshot(sheet, to: path.replacingOccurrences(of: ".png", with: "-machine-settings.png"))
                } else if let extra, let kind = value("FEV_APP_OPEN")?.split(separator: ":").first {
                    snapshot(extra, to: path.replacingOccurrences(of: ".png", with: "-\(kind).png"))
                }
                if let k = value("FEV_APP_K8S")?.split(separator: ":").first, k == "logs" || k == "exec",
                   let w = NSApp.orderedWindows.first(where: { $0 !== window.window && $0 !== app.settings.window && $0.isVisible && $0.contentView != nil }) {
                    snapshot(w, to: path.replacingOccurrences(of: ".png", with: "-k8s\(k).png"))
                }
            }
        }
        if let path = value("FEV_APP_STATE") {
            let after = Double(value("FEV_APP_STATE_AFTER") ?? "") ?? 6
            DispatchQueue.main.asyncAfter(deadline: .now() + after) {
                let m = app.model
                let state: [String: Any] = [
                    "section": "\(app.nav.section)",
                    "connected": m.link == .connected,
                    "window_visible": window.window?.isVisible ?? false,
                    "containers": m.containers.count,
                    "stats_samples": m.counters.statsSamples,
                    "connects": m.counters.connects,
                    "async_layers": asyncLayers(window.window?.contentView?.superview?.layer),
                    "outline": ContainersOutline.Coordinator.current?.state ?? [:],
                ]
                if let data = try? JSONSerialization.data(withJSONObject: state, options: [.sortedKeys]) {
                    try? data.write(to: URL(fileURLWithPath: path))
                }
            }
        }
        if let spec = value("FEV_APP_KEYS")?.split(separator: "@"), spec.count == 2, let at = Double(spec[1]) {
            let text = String(spec[0])
            DispatchQueue.main.asyncAfter(deadline: .now() + at) { type(text) }
        }
        if let path = value("FEV_APP_CHROME") {
            let settle = Double(value("FEV_APP_CHROME_SETTLE") ?? "") ?? 1.5
            Task { @MainActor in
                try? await Task.sleep(for: .seconds(settle))
                var sections: [[String: Any]] = []
                let first = app.nav.section
                for s in [first] + SidebarItem.allCases.filter({ $0 != first }) {
                    app.nav.section = s
                    try? await Task.sleep(for: .seconds(settle))
                    sections.append(["section": s.rawValue].merging(chrome(window.window)) { a, _ in a })
                }
                if let data = try? JSONSerialization.data(withJSONObject: ["sections": sections], options: [.sortedKeys, .prettyPrinted]) {
                    try? data.write(to: URL(fileURLWithPath: path))
                }
            }
        }
        if let secs = Double(value("FEV_APP_CLOSE_AFTER") ?? "") {
            DispatchQueue.main.asyncAfter(deadline: .now() + secs) {
                window.window?.close()
                if value("FEV_APP_SETTINGS") != nil { app.settings.window?.close() }
                if value("FEV_APP_ONBOARDING") != nil { app.onboardingWindow.window?.close() }
                extra?.close()
            }
        }
        if let secs = Double(value("FEV_APP_REOPEN_AFTER") ?? "") {
            DispatchQueue.main.asyncAfter(deadline: .now() + secs) { _ = app.applicationShouldHandleReopen(NSApp, hasVisibleWindows: true) }
        }
        if let secs = Double(value("FEV_APP_QUIT_AFTER") ?? "") {
            NSApp.perform(#selector(NSApplication.terminate(_:)), with: nil, afterDelay: secs)
        }
        if let secs = Double(value("FEV_APP_EXIT_AFTER") ?? "") {
            DispatchQueue.main.asyncAfter(deadline: .now() + secs) {
                app.leavesFevom = true
                for w in NSApp.windows { if let sheet = w.attachedSheet { w.endSheet(sheet) } }
                NSApp.terminate(nil)
            }
        }
    }

    static func type(_ spec: String) {
        let anyway = spec.hasPrefix("!")
        let text = anyway ? String(spec.dropFirst()) : spec
        let w = NSApp.isActive ? NSApp.keyWindow : anyway ? NSApp.orderedWindows.first { $0.isVisible && $0.canBecomeKey } : nil
        guard let w else {
            NSLog("keys: %@ not delivered: the app is not the active one", text)
            return
        }
        for c in text {
            for t in [NSEvent.EventType.keyDown, .keyUp] {
                if let e = NSEvent.keyEvent(with: t, location: .zero, modifierFlags: [], timestamp: ProcessInfo.processInfo.systemUptime,
                                            windowNumber: w.windowNumber, context: nil, characters: String(c),
                                            charactersIgnoringModifiers: String(c), isARepeat: false, keyCode: 0) {
                    if NSApp.isActive { NSApp.postEvent(e, atStart: false) } else { w.sendEvent(e) }
                }
            }
        }
        NSLog("keys: %@ delivered to the window %@", text, w.title)
    }

    static func chrome(_ window: NSWindow?) -> [String: Any] {
        guard let w = window else { return [:] }
        let items = w.toolbar?.items ?? []
        return [
            "title": w.title,
            "subtitle": w.subtitle,
            "toolbar": w.toolbar.map { $0.isVisible } ?? false,
            "top": (w.frame.height - w.contentLayoutRect.height).rounded(),
            "search": items.contains { $0 is NSSearchToolbarItem },
            "app_active": NSApp.isActive,
            "activation_policy": NSApp.activationPolicy().rawValue,
            "ignores_mouse": w.ignoresMouseEvents,
            "alpha": w.alphaValue,
        ]
    }

    static func asyncLayers(_ layer: CALayer?) -> Int {
        guard let layer else { return 0 }
        return (layer.drawsAsynchronously ? 1 : 0) + (layer.sublayers ?? []).reduce(0) { $0 + asyncLayers($1) }
    }

    static func snapshot(_ window: NSWindow?, to path: String) {
        guard let view = window?.contentView, let rep = view.bitmapImageRepForCachingDisplay(in: view.bounds) else { return }
        view.cacheDisplay(in: view.bounds, to: rep)
        try? rep.representation(using: .png, properties: [:])?.write(to: URL(fileURLWithPath: path))
    }
}

private extension Array where Element == String {
    var splitPair: (String, String) { (first ?? "", count > 1 ? self[1] : "") }
}

enum LoginItem {
    @MainActor static func set(_ on: Bool) {
        let app = SMAppService.mainApp
        do {
            if on, app.status != .enabled {
                try app.register()
            } else if !on, app.status == .enabled {
                try app.unregister()
            }
        } catch {
            NSLog("Fevom: the app as a login item (start_at_login %@): %@", on ? "on" : "off", String(describing: error))
        }
    }
}
