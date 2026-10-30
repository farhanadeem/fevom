import AppKit
import FevomStores
import ServiceManagement

enum UninstallCleanup {
    static let argument = "--uninstall-cleanup"

    static func run() -> Int32 {
        var status: Int32 = 0
        func fail(_ what: String, _ error: Error) {
            FileHandle.standardError.write(Data("Fevom: \(what): \(error)\n".utf8))
            status = 1
        }
        let item = SMAppService.mainApp
        if item.status == .enabled || item.status == .requiresApproval {
            do { try item.unregister() } catch { fail("the login item", error) }
        }
        if FinderSidebar.contains(DaemonPlace.defaultView) == true {
            do { try FinderSidebar.remove(DaemonPlace.defaultView) } catch { fail("~/Fevom in Finder's sidebar", error) }
        }
        return status
    }
}

extension AppDelegate {
    func uninstall() {
        let alert = NSAlert()
        alert.alertStyle = .warning
        alert.messageText = "Uninstall Fevom?"
        alert.informativeText = "Fevom is stopped and removed from this Mac with everything it set up. You may be asked for your password."
        let delete = NSButton(checkboxWithTitle: "Also delete all containers, images, volumes and machines", target: nil, action: nil)
        alert.accessoryView = delete
        let ok = alert.addButton(withTitle: "Uninstall")
        ok.hasDestructiveAction = true
        alert.addButton(withTitle: "Cancel")
        NSApp.activate()
        guard alert.runModal() == .alertFirstButtonReturn else { return }
        let deleteData = delete.state == .on

        let progress = UninstallProgress()
        progress.show()
        Task { @MainActor [onboarding] in
            let done = await onboarding.uninstall(deleteData: deleteData)
            progress.close()
            if !done {
                let failed = NSAlert()
                failed.alertStyle = .critical
                failed.messageText = "Fevom Was Not Fully Uninstalled"
                failed.informativeText = onboarding.output.split(separator: "\n").suffix(12).joined(separator: "\n")
                failed.runModal()
            }
            if done || !FileManager.default.fileExists(atPath: Bundle.main.bundlePath) {
                self.leavesFevom = true
                NSApp.terminate(nil)
            }
        }
    }
}

@MainActor
final class UninstallProgress {
    private let window: NSWindow

    init() {
        window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 300, height: 80), styleMask: [.titled], backing: .buffered, defer: true)
        window.title = "Uninstalling Fevom"
        let spinner = NSProgressIndicator()
        spinner.style = .spinning
        spinner.controlSize = .small
        spinner.startAnimation(nil)
        let label = NSTextField(labelWithString: "Uninstalling Fevom…")
        let stack = NSStackView(views: [spinner, label])
        stack.orientation = .horizontal
        stack.spacing = 10
        stack.edgeInsets = NSEdgeInsets(top: 20, left: 20, bottom: 20, right: 20)
        window.contentView = stack
        window.isReleasedWhenClosed = false
    }

    func show() {
        window.center()
        window.makeKeyAndOrderFront(nil)
    }

    func close() { window.orderOut(nil) }
}
