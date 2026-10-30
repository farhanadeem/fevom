import AppKit
import FevomAPI
import FevomStores
import SwiftUI

@MainActor
final class OnboardingWindowController: NSObject, NSWindowDelegate {
    let model: AppModel
    let onboarding: Onboarding
    private(set) var window: NSWindow?
    private var viewing = false

    init(model: AppModel, onboarding: Onboarding) {
        self.model = model
        self.onboarding = onboarding
    }

    func present() {
        if onboarding.step == .done { onboarding.step = .welcome }
        if window == nil {
            let root = OnboardingView(close: { [weak self] in self?.window?.close() }).environment(model).environment(onboarding)
            let w = NSWindow(contentViewController: NSHostingController(rootView: root))
            w.title = "Set Up Fevom"
            w.styleMask = [.titled, .closable]
            w.isReleasedWhenClosed = false
            w.isRestorable = false
            w.delegate = self
            w.center()
            window = w
        }
        DevHooks.show(window)
        if !viewing {
            viewing = true
            model.show()
        }
    }

    func windowWillClose(_ notification: Notification) {
        if viewing {
            viewing = false
            model.hide()
        }
        DispatchQueue.main.async { [weak self] in
            if let self, self.window?.isVisible == false { self.window = nil }
        }
    }
}

struct OnboardingView: View {
    @Environment(AppModel.self) private var model
    @Environment(Onboarding.self) private var onboarding
    let close: () -> Void

    var body: some View {
        @Bindable var ob = onboarding
        VStack(alignment: .leading, spacing: 14) {
            StepHeader(step: ob.step)
            Divider()
            Group {
                switch ob.step {
                case .welcome: welcome
                case .commandLine: commandLine
                case .admin: admin
                case .https: https
                case .done: done
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            if !ob.output.isEmpty {
                ScrollView {
                    Text(ob.output).font(.caption.monospaced()).textSelection(.enabled).frame(maxWidth: .infinity, alignment: .leading)
                }
                .frame(height: 130)
                .padding(6)
                .background(.quaternary.opacity(0.5), in: RoundedRectangle(cornerRadius: 6))
                .accessibilityLabel("What the command printed")
            }
            Spacer(minLength: 0)
            footer
        }
        .padding(28)
        .frame(width: 580, height: 490)
        .background(WorkspaceStyle.canvas)
    }

    var welcome: some View {
        VStack(alignment: .leading, spacing: 8) {
            Text("Setup connects your terminal to Fevom. Direct networking and trusted HTTPS are optional.")
                .foregroundStyle(.secondary)
            VStack(alignment: .leading, spacing: 16) {
                Label("docker, docker compose and ssh to machines", systemImage: "terminal")
                Label("Containers and machines by IP address and *.fev.local name", systemImage: "network")
                Label("HTTPS for *.fev.local", systemImage: "lock.shield")
            }
            .font(.callout).padding(.top, 18)
        }
    }

    var commandLine: some View {
        @Bindable var ob = onboarding
        return VStack(alignment: .leading, spacing: 8) {
            Text("Command line")
                .font(.title3.weight(.medium))
            Text("Fevom adds its commands in ~/.fev/bin and updates your shell profile. Open a new terminal after setup to use them.")
                .foregroundStyle(.secondary)
            Toggle("Download the Docker client tools (compose, buildx)", isOn: $ob.downloadTools)
            if ob.setupDone, ob.lastStatus == nil {
                Label("Already set up.", systemImage: "checkmark.circle").foregroundStyle(.secondary)
            }
            CommandLine(text: Onboarding.setupCommand + (ob.downloadTools ? "" : " --no-tools"))
            Text("To undo it, run fev setup --undo.").font(.caption).foregroundStyle(.secondary)
        }
    }

    var admin: some View {
        VStack(alignment: .leading, spacing: 8) {
            Text("Direct networking").font(.title3.weight(.medium))
            Text("The optional admin helper enables container and machine IP addresses, *.fev.local names, and localhost ports below 1024.")
                .foregroundStyle(.secondary)
            Text("Installation asks for an administrator password. The helper runs as root only when called; you can review exactly what it installs first.")
                .font(.callout).padding(.vertical, 6)
            HStack {
                Button("Show What It Installs") { Task { await onboarding.adminDryRun() } }
                    .help(Onboarding.adminDryRunCommand + ": every path and command, with no password")
                Button("Install…") { Task { await onboarding.installAdmin() } }
                    .help("Asks for an administrator's password")
            }
            .disabled(onboarding.running)
            CommandLine(text: Onboarding.adminCommand)
        }
    }

    var https: some View {
        VStack(alignment: .leading, spacing: 8) {
            Text("Trusted HTTPS").font(.title3.weight(.medium))
            Text("Fevom creates certificates for https://NAME.fev.local. Trust its local root to open these sites without browser warnings.")
                .foregroundStyle(.secondary)
            Text("This optional step asks for your password. Trust applies to HTTPS in your Mac account only, and you can remove it in Settings.")
                .font(.callout).padding(.vertical, 6)
            if let h = model.status?.https {
                LabeledContent("Fevom's root certificate", value: h.trust == DaemonAPI.Const.trustTrusted ? "Trusted" : NetworkStatusSection.trust(h.trust))
                if !h.error.isEmpty { Text(h.error).font(.caption).foregroundStyle(.orange) }
                Button("Trust…") { Task { await model.trustHTTPS(DaemonAPI.Const.trustInstall) } }
                    .disabled(h.trust == DaemonAPI.Const.trustTrusted)
            } else {
                Text(model.link == .connected ? "Fevom hasn't reported HTTPS yet." : "Fevom is starting…").foregroundStyle(.secondary)
            }
            if let e = model.lastError { Text(e).font(.caption).foregroundStyle(.red).textSelection(.enabled) }
            CommandLine(text: Onboarding.httpsCommand)
        }
    }

    var done: some View {
        VStack(alignment: .leading, spacing: 8) {
            Text("Setup is done").font(.title3.weight(.medium))
            Text("Open a new terminal to use docker and fev.").foregroundStyle(.secondary)
        }
    }

    @ViewBuilder var footer: some View {
        HStack {
            if onboarding.step != .welcome && onboarding.step != .done {
                Button("Back") { move(-1) }
            }
            Spacer()
            if onboarding.running { ProgressView().controlSize(.small) }
            switch onboarding.step {
            case .welcome:
                Button("Not Now") { onboarding.finish(); close() }
                Button("Continue") { move(1) }.keyboardShortcut(.defaultAction)
            case .commandLine:
                Button("Skip") { move(1) }
                Button(onboarding.lastStatus == 0 ? "Continue" : "Set Up") {
                    if onboarding.lastStatus == 0 { move(1) } else { Task { await onboarding.runSetup() } }
                }
                .keyboardShortcut(.defaultAction).disabled(onboarding.running)
            case .admin, .https:
                Button("Skip") { move(1) }
                Button("Continue") { move(1) }.keyboardShortcut(.defaultAction).disabled(onboarding.running)
            case .done:
                Button("Done") { onboarding.finish(); close() }.keyboardShortcut(.defaultAction)
            }
        }
    }

    func move(_ by: Int) {
        let next = Onboarding.Step(rawValue: onboarding.step.rawValue + by) ?? .done
        onboarding.step = next
        onboarding.clearOutput()
    }
}

struct StepHeader: View {
    let step: Onboarding.Step
    var body: some View {
        HStack(spacing: 10) {
            if step == .done { EntityIcon(symbol: "checkmark", size: 48) } else { BrandIcon(size: 56) }
            VStack(alignment: .leading) {
                Text(title).font(.title2.bold())
                if step != .welcome && step != .done {
                    HStack(spacing: 5) {
                        ForEach(1...3, id: \.self) { i in
                            Capsule().fill(i <= step.rawValue ? Color.accentColor : Color.secondary.opacity(0.2)).frame(width: 24, height: 3)
                        }
                        Text("Step \(step.rawValue) of 3").font(.caption).foregroundStyle(.secondary).padding(.leading, 5)
                    }.padding(.top, 4)
                }
            }
        }
        .accessibilityElement(children: .combine)
    }

    var title: String {
        switch step {
        case .welcome: "Welcome to Fevom"
        case .commandLine: "Commands and Docker"
        case .admin: "Admin helper"
        case .https: "HTTPS for fev.local"
        case .done: "Fevom is set up"
        }
    }
}

enum AdminPrompt {
    static func run(_ args: [String]) async throws -> String {
        let command = args.map(quote).joined(separator: " ")
        let script = "do shell script \"\(command.replacingOccurrences(of: "\\", with: "\\\\").replacingOccurrences(of: "\"", with: "\\\""))\" "
            + "with prompt \"Fevom installs its admin helper.\" with administrator privileges"
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/usr/bin/osascript")
        p.arguments = ["-e", script]
        let pipe = Pipe()
        p.standardOutput = pipe
        p.standardError = pipe
        let out = pipe.fileHandleForReading
        let reader = Task.detached { try out.readToEnd() ?? Data() }
        let status: Int32 = try await withCheckedThrowingContinuation { k in
            p.terminationHandler = { k.resume(returning: $0.terminationStatus) }
            do { try SessionAttach.spawning { try p.run() } } catch { k.resume(throwing: error) }
            try? pipe.fileHandleForWriting.close()
        }
        let text = String(decoding: try await reader.value, as: UTF8.self)
        if status != 0 { throw RPCConnection.Failure.protocolError(text.isEmpty ? "the install was not made (status \(status))" : text) }
        return text
    }

    static func quote(_ s: String) -> String { "'" + s.replacingOccurrences(of: "'", with: "'\\''") + "'" }
}
