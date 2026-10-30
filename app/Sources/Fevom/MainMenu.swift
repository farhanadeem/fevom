import AppKit
import FevomAPI
import FevomStores
import SwiftUI

@MainActor @Observable
final class Navigation {
    var section: SidebarItem
    var newMachine = 0
    var machineSettings: String?
    var find = 0
    var filesPath: [String]?

    func showFiles(_ parts: [String]) {
        filesPath = parts
        section = .files
    }

    init(section: SidebarItem) { self.section = section }
}

@MainActor
enum MainMenu {
    struct Actions {
        var settings: () -> Void
        var checkForUpdates: (() -> Void)?
        var onboarding: () -> Void
        var uninstall: () -> Void
        var commands: () -> Void
        var open: () -> Void
        var section: (SidebarItem) -> Void
        var newMachine: () -> Void
        var find: () -> Void
        var start: () -> Void
        var stop: () -> Void
        var restart: () -> Void
        var logs: () -> Void
    }

    static func build(_ a: Actions) -> NSMenu {
        let main = NSMenu()

        let app = submenu(main, "Fevom")
        app.addItem(withTitle: "About Fevom", action: #selector(NSApplication.orderFrontStandardAboutPanel(_:)), keyEquivalent: "")
        if let check = a.checkForUpdates { app.addItem(item("Check for Updates…", "", check)) }
        app.addItem(.separator())
        app.addItem(item("Settings…", ",", a.settings))
        app.addItem(item("Set Up Fevom…", "", a.onboarding))
        app.addItem(item("Uninstall Fevom…", "", a.uninstall))
        app.addItem(.separator())
        app.addItem(withTitle: "Hide Fevom", action: #selector(NSApplication.hide(_:)), keyEquivalent: "h")
        let others = app.addItem(withTitle: "Hide Others", action: #selector(NSApplication.hideOtherApplications(_:)), keyEquivalent: "h")
        others.keyEquivalentModifierMask = [.command, .option]
        app.addItem(withTitle: "Show All", action: #selector(NSApplication.unhideAllApplications(_:)), keyEquivalent: "")
        app.addItem(.separator())
        app.addItem(withTitle: "Quit Fevom", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")

        let file = submenu(main, "File")
        file.addItem(item("New Machine…", "n", a.newMachine))
        file.addItem(item("Open Main Window", "0", a.open))
        file.addItem(.separator())
        file.addItem(withTitle: "Close Window", action: #selector(NSWindow.performClose(_:)), keyEquivalent: "w")

        let edit = submenu(main, "Edit")
        edit.addItem(withTitle: "Undo", action: Selector(("undo:")), keyEquivalent: "z")
        let redo = edit.addItem(withTitle: "Redo", action: Selector(("redo:")), keyEquivalent: "z")
        redo.keyEquivalentModifierMask = [.command, .shift]
        edit.addItem(.separator())
        edit.addItem(withTitle: "Cut", action: #selector(NSText.cut(_:)), keyEquivalent: "x")
        edit.addItem(withTitle: "Copy", action: #selector(NSText.copy(_:)), keyEquivalent: "c")
        edit.addItem(withTitle: "Paste", action: #selector(NSText.paste(_:)), keyEquivalent: "v")
        edit.addItem(withTitle: "Select All", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")
        edit.addItem(.separator())
        edit.addItem(item("Find", "f", a.find))

        let view = submenu(main, "View")
        for s in SidebarItem.allCases {
            view.addItem(item(s.rawValue, s.shortcut.map { "\($0)" } ?? "", { a.section(s) }))
        }
        view.addItem(.separator())
        let full = view.addItem(withTitle: "Enter Full Screen", action: #selector(NSWindow.toggleFullScreen(_:)), keyEquivalent: "f")
        full.keyEquivalentModifierMask = [.command, .control]

        let service = submenu(main, "Service")
        service.addItem(item("Start", "", a.start))
        service.addItem(item("Stop", "", a.stop))
        service.addItem(item("Restart", "", a.restart))
        service.addItem(.separator())
        service.addItem(item("Fevom's Log", "", a.logs))

        let window = submenu(main, "Window")
        window.addItem(withTitle: "Minimize", action: #selector(NSWindow.performMiniaturize(_:)), keyEquivalent: "m")
        window.addItem(withTitle: "Zoom", action: #selector(NSWindow.performZoom(_:)), keyEquivalent: "")
        window.addItem(.separator())
        window.addItem(withTitle: "Bring All to Front", action: #selector(NSApplication.arrangeInFront(_:)), keyEquivalent: "")
        NSApp.windowsMenu = window

        let help = submenu(main, "Help")
        help.addItem(item("Command Line Equivalents", "?", a.commands))
        NSApp.helpMenu = help
        return main
    }

    private static func submenu(_ main: NSMenu, _ title: String) -> NSMenu {
        let top = NSMenuItem(title: title, action: nil, keyEquivalent: "")
        let m = NSMenu(title: title)
        top.submenu = m
        main.addItem(top)
        return m
    }

    private static func item(_ title: String, _ key: String, _ run: @escaping () -> Void) -> NSMenuItem {
        let mi = NSMenuItem(title: title, action: #selector(Target.fire(_:)), keyEquivalent: key)
        let t = Target(run)
        mi.target = t
        mi.representedObject = t
        return mi
    }
}

@MainActor
final class CommandsWindowController {
    private var window: NSWindow?

    func present() {
        if window == nil {
            let w = NSWindow(contentViewController: NSHostingController(rootView: CommandsView()))
            w.title = "Command Line Equivalents"
            w.styleMask = [.titled, .closable, .resizable, .miniaturizable]
            w.setContentSize(NSSize(width: 560, height: 520))
            w.isReleasedWhenClosed = false
            w.isRestorable = false
            w.center()
            window = w
        }
        DevHooks.show(window)
    }
}

struct CommandsView: View {
    @State private var filter = ""

    var body: some View {
        List {
            ForEach(CommandsHelp.groups) { g in
                let entries = g.entries.filter { filter.isEmpty || $0.action.localizedCaseInsensitiveContains(filter) || $0.command.localizedCaseInsensitiveContains(filter) }
                if !entries.isEmpty {
                    Section(g.title) {
                        ForEach(entries) { e in
                            HStack {
                                Text(e.action)
                                Spacer()
                                Text(e.command).font(.callout.monospaced()).textSelection(.enabled)
                                Button("Copy \(e.command)", systemImage: "doc.on.doc") { copyToPasteboard(e.command) }
                                    .labelStyle(.iconOnly).buttonStyle(.borderless)
                            }
                            .accessibilityElement(children: .combine)
                        }
                    }
                }
            }
        }
        .searchable(text: $filter, placement: .toolbar, prompt: "Filter commands")
        .frame(minWidth: 480, minHeight: 360)
    }
}
