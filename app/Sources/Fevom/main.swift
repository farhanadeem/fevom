import AppKit

if ProcessInfo.processInfo.arguments.dropFirst().first == UninstallCleanup.argument {
    exit(UninstallCleanup.run())
}
UserDefaults.standard.register(defaults: ["NSViewCanUseGPUAcceleration": false])
let app = NSApplication.shared
if DevHooks.unattended {
    app.setActivationPolicy(.prohibited)
    DevHooks.unattendedActivity = ProcessInfo.processInfo.beginActivity(options: [.userInitiated, .latencyCritical], reason: "an unattended test run")
}
let delegate = AppDelegate()
app.delegate = delegate
app.run()
