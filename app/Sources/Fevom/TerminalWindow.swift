import AppKit
import FevomAPI
import FevomStores
@preconcurrency import SwiftTerm

@MainActor
final class TerminalWindowController: NSObject, NSWindowDelegate {
    let session: any TerminalStream
    let window: NSWindow
    let term: TerminalView
    private let relay: Relay
    var onClose: () -> Void = {}

    convenience init(place: DaemonPlace, machine: String, user: String = "") {
        self.init(stream: TerminalSession(place: place, machine: machine, user: user), name: machine)
    }

    convenience init(place: DaemonPlace, container: String, name: String) {
        self.init(stream: ContainerTerminal(place: place, container: container), name: name)
    }

    convenience init(place: DaemonPlace, debugTarget: String, kind: String, readOnly: Bool, context: String = "", name: String) {
        self.init(stream: DebugSession(place: place, target: debugTarget, kind: kind, readOnly: readOnly, context: context),
                  name: context.isEmpty ? "Debug \(name)" : "Debug \(name) @ \(context)")
    }

    init(stream: any TerminalStream, name: String) {
        let machine = name
        session = stream
        term = TerminalView(frame: NSRect(x: 0, y: 0, width: 820, height: 500))
        term.font = NSFont.monospacedSystemFont(ofSize: 12, weight: .regular)
        relay = Relay()
        window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 820, height: 500), styleMask: [.titled, .closable, .miniaturizable, .resizable], backing: .buffered, defer: false)
        super.init()
        window.title = "\(machine) — Terminal"
        window.contentView = term
        window.isReleasedWhenClosed = false
        window.isRestorable = false
        window.delegate = self
        term.setAccessibilityLabel("Terminal into \(machine)")
        relay.session = session
        term.terminalDelegate = relay
        session.onOutput = { [weak self] bytes in self?.term.feed(byteArray: bytes[...]) }
        session.onExit = { [weak self] why in
            self?.term.feed(text: "\r\n[\(why)]\r\n")
            self?.window.title = "\(machine) — \(why)"
        }
    }

    func show() {
        window.center()
        DevHooks.show(window, regular: false)
        window.makeFirstResponder(term)
        let t = term.getTerminal()
        Task {
            do {
                try await session.start(rows: t.rows, cols: t.cols)
            } catch {
                term.feed(text: "fev: \(error)\r\n")
            }
        }
    }

    func windowWillClose(_ notification: Notification) {
        session.close()
        onClose()
    }

    @MainActor
    final class Relay: NSObject, @preconcurrency TerminalViewDelegate {
        weak var session: (any TerminalStream)?

        func send(source: TerminalView, data: ArraySlice<UInt8>) { session?.send(Array(data)) }
        func sizeChanged(source: TerminalView, newCols: Int, newRows: Int) { session?.resize(rows: newRows, cols: newCols) }
        func setTerminalTitle(source: TerminalView, title: String) { source.window?.title = title }
        func hostCurrentDirectoryUpdate(source: TerminalView, directory: String?) {}
        func scrolled(source: TerminalView, position: Double) {}
        func requestOpenLink(source: TerminalView, link: String, params: [String: String]) {
            if let url = URL(string: link), ["http", "https"].contains(url.scheme ?? "") { NSWorkspace.shared.open(url) }
        }
        func bell(source: TerminalView) { NSSound.beep() }
        func clipboardCopy(source: TerminalView, content: Data) {
            NSPasteboard.general.clearContents()
            NSPasteboard.general.setString(String(decoding: content, as: UTF8.self), forType: .string)
        }
        func iTermContent(source: TerminalView, content: ArraySlice<UInt8>) {}
        func rangeChanged(source: TerminalView, startY: Int, endY: Int) {}
    }
}

@MainActor
enum ExternalTerminal {
    static func kubectlExec(namespace: String, pod: String, container: String) throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("fev-terminal", isDirectory: true)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        let file = dir.appendingPathComponent("k8s-\(namespace)-\(pod).command")
        func q(_ s: String) -> String { "'" + s.replacingOccurrences(of: "'", with: "'\\''") + "'" }
        var cmd = "exec kubectl --context fev exec -it -n \(q(namespace)) \(q(pod))"
        if !container.isEmpty { cmd += " -c \(q(container))" }
        cmd += " -- sh -c 'command -v bash >/dev/null && exec bash || exec sh'"
        try "#!/bin/sh\n\(cmd)\n".write(to: file, atomically: true, encoding: .utf8)
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: file.path)
        let terminal = URL(fileURLWithPath: "/System/Applications/Utilities/Terminal.app")
        NSWorkspace.shared.open([file], withApplicationAt: terminal, configuration: NSWorkspace.OpenConfiguration())
    }

    static func open(machine: String) throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("fev-terminal", isDirectory: true)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        let file = dir.appendingPathComponent("\(machine).command")
        let quoted = "'" + machine.replacingOccurrences(of: "'", with: "'\\''") + "'"
        try "#!/bin/sh\nexec ssh \(quoted)@fev\n".write(to: file, atomically: true, encoding: .utf8)
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: file.path)
        let terminal = URL(fileURLWithPath: "/System/Applications/Utilities/Terminal.app")
        NSWorkspace.shared.open([file], withApplicationAt: terminal, configuration: NSWorkspace.OpenConfiguration())
    }

    static func debug(target: String, context: String = "") throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("fev-terminal", isDirectory: true)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        let file = dir.appendingPathComponent("debug-\(abs(target.hashValue)).command")
        let quoted = "'" + target.replacingOccurrences(of: "'", with: "'\\''") + "'"
        var cmd = "exec fev debug "
        if !context.isEmpty {
            cmd += "-c '" + context.replacingOccurrences(of: "'", with: "'\\''") + "' "
        }
        cmd += quoted
        try "#!/bin/sh\n\(cmd)\n".write(to: file, atomically: true, encoding: .utf8)
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: file.path)
        let terminal = URL(fileURLWithPath: "/System/Applications/Utilities/Terminal.app")
        NSWorkspace.shared.open([file], withApplicationAt: terminal, configuration: NSWorkspace.OpenConfiguration())
    }
}
