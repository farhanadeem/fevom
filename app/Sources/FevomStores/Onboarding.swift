import FevomAPI
import Darwin
import Foundation
import Observation

@MainActor @Observable
public final class Onboarding {
    public enum Step: Int, CaseIterable, Sendable {
        case welcome, commandLine, admin, https, done
    }

    public var step: Step = .welcome
    public private(set) var output = ""
    public private(set) var running = false
    public private(set) var lastStatus: Int32?
    public var downloadTools = true

    public let home: String
    public let cli: String?
    public var environment: [String: String]?
    public var runAsAdmin: @Sendable ([String]) async throws -> String = { _ in
        throw RPCConnection.Failure.protocolError("no way to ask for an administrator's password here")
    }
    let defaults: UserDefaults
    static let doneKey = "dev.fevom.onboarding.done"

    public init(home: String, cli: String?, environment: [String: String]? = nil, defaults: UserDefaults = .standard) {
        self.home = home
        self.cli = cli
        self.environment = environment
        self.defaults = defaults
    }

    public var needed: Bool {
        !setupDone && !defaults.bool(forKey: Self.doneKey)
    }

    public var setupDone: Bool { FileManager.default.fileExists(atPath: home + "/setup.json") }

    public func clearOutput() {
        output = ""
        lastStatus = nil
    }

    public func finish() {
        defaults.set(true, forKey: Self.doneKey)
        step = .done
    }

    public static func findCLI(fevd: String?, bundle: Bundle = .main, environment env: [String: String] = ProcessInfo.processInfo.environment,
                               honourEnvironment: Bool = DaemonPlace.honoursEnvironment) -> String? {
        var c: [String] = []
        if honourEnvironment, let e = env["FEV_CLI"], !e.isEmpty { c.append(e) }
        c.append(bundle.bundleURL.appendingPathComponent("Contents/MacOS/bin/fev").path)
        if let d = fevd { c.append((d as NSString).deletingLastPathComponent + "/fev") }
        return c.first { FileManager.default.isExecutableFile(atPath: $0) }
    }

    public var setupArguments: [String] {
        ["--home", home, "setup"] + (downloadTools ? [] : ["--no-tools"])
    }

    nonisolated public static let setupCommand = "fev setup"
    nonisolated public static let adminCommand = "sudo fev setup --admin"
    nonisolated public static let adminDryRunCommand = "fev setup --admin --dry-run"
    nonisolated public static let httpsCommand = "fev setup --https"

    public func adminArguments() -> [String]? {
        guard let cli else { return nil }
        return ["/usr/bin/env", "SUDO_UID=\(getuid())", cli, "--home", home, "setup", "--admin"]
    }

    @discardableResult
    public func runSetup() async -> Bool {
        await run(setupArguments) == 0
    }

    @discardableResult
    public func adminDryRun() async -> Bool {
        await run(["--home", home, "setup", "--admin", "--dry-run"]) == 0
    }

    @discardableResult
    public func installAdmin() async -> Bool {
        guard let args = adminArguments() else {
            output = "No fev command was found beside this app."
            lastStatus = 127
            return false
        }
        running = true
        defer { running = false }
        do {
            output = try await runAsAdmin(args)
            lastStatus = 0
            return true
        } catch {
            output = String(describing: error)
            lastStatus = 1
            return false
        }
    }

    public func uninstallArguments(deleteData: Bool) -> [String] {
        ["--home", home, "uninstall", "--from-app", "--yes"] + (deleteData ? ["--delete-data"] : [])
    }

    @discardableResult
    public func uninstall(deleteData: Bool) async -> Bool {
        await run(uninstallArguments(deleteData: deleteData)) == 0
    }

    func run(_ args: [String]) async -> Int32 {
        guard let cli else {
            output = "No fev command was found beside this app."
            lastStatus = 127
            return 127
        }
        running = true
        output = ""
        defer { running = false }
        let env = environment
        let r: (Int32, String)
        do {
            r = try await Self.capture(cli, args, environment: env)
        } catch {
            output = String(describing: error)
            lastStatus = 126
            return 126
        }
        output = r.1
        lastStatus = r.0
        return r.0
    }

    static func capture(_ exe: String, _ args: [String], environment: [String: String]?) async throws -> (Int32, String) {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: exe)
        p.arguments = args
        if let environment { p.environment = environment }
        p.standardInput = FileHandle.nullDevice
        let pipe = Pipe()
        p.standardOutput = pipe
        p.standardError = pipe
        let out = pipe.fileHandleForReading
        let reader = Task.detached { try out.readToEnd() ?? Data() }
        let status: Int32 = try await withCheckedThrowingContinuation { k in
            p.terminationHandler = { k.resume(returning: $0.terminationStatus) }
            do {
                try SessionAttach.spawning { try p.run() }
            } catch {
                k.resume(throwing: error)
            }
            try? pipe.fileHandleForWriting.close()
        }
        let data = try await reader.value
        return (status, String(decoding: data, as: UTF8.self))
    }

    public static func releaseEnvironment(lang: String?) -> [String: String] {
        var env = DaemonPlace.minimalEnvironment(lang: lang)
        if let pw = getpwuid(getuid()), let sh = pw.pointee.pw_shell { env["SHELL"] = String(cString: sh) }
        return env
    }
}
