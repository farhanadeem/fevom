import Foundation
import Synchronization
import Testing

@testable import FevomAPI
@testable import FevomStores

final class SocketDaemon: @unchecked Sendable {
    let home: String
    let listener: Int32
    let helloError: String?
    private let lock = NSLock()
    private var _requests: [[String: Any]] = []
    private var _markAt: [Bool] = []
    var requests: [[String: Any]] { lock.withLock { _requests } }
    var markAt: [Bool] { lock.withLock { _markAt } }

    private var conns: Set<Int32> = []

    init(home: String, build: String, helloError: String? = nil, answer: @escaping @Sendable (String, [String: Any]) -> (result: String?, close: Bool)) throws {
        self.home = home
        self.helloError = helloError
        try FileManager.default.createDirectory(atPath: home + "/run", withIntermediateDirectories: true)
        let path = DaemonAPI.socketPath(home: home)
        unlink(path)
        listener = socket(AF_UNIX, SOCK_STREAM, 0)
        var addr = sockaddr_un()
        addr.sun_family = sa_family_t(AF_UNIX)
        withUnsafeMutableBytes(of: &addr.sun_path) { dst in
            let b = Array(path.utf8)
            dst.copyBytes(from: b)
            dst[b.count] = 0
        }
        let rc = withUnsafePointer(to: &addr) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(listener, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) } }
        precondition(rc == 0 && listen(listener, 8) == 0, "bind \(path): \(errno)")
        let l = listener
        Thread.detachNewThread { [self] in
            while true {
                let fd = accept(l, nil, nil)
                if fd < 0 { return }
                lock.withLock {
                    _ = conns.insert(fd)
                    if exited { shutdown(fd, SHUT_RDWR) }
                }
                Thread.detachNewThread { [self] in serve(fd, build: build, answer: answer) }
            }
        }
    }

    private func serve(_ fd: Int32, build: String, answer: @Sendable (String, [String: Any]) -> (result: String?, close: Bool)) {
        defer {
            lock.withLock {
                _ = conns.remove(fd)
                close(fd)
            }
        }
        var buf = Data()
        var chunk = [UInt8](repeating: 0, count: 65536)
        while true {
            let n = read(fd, &chunk, chunk.count)
            if n <= 0 { return }
            buf.append(contentsOf: chunk[0..<n])
            while let nl = buf.firstIndex(of: 0x0a) {
                let line = buf[buf.startIndex..<nl]
                buf.removeSubrange(buf.startIndex...nl)
                let req = (try? JSONSerialization.jsonObject(with: line)) as? [String: Any] ?? [:]
                let mark = FileManager.default.fileExists(atPath: DaemonAPI.stopMarkPath(home: home))
                lock.withLock { _requests.append(req); _markAt.append(mark) }
                let method = req["method"] as? String ?? ""
                let id = req["id"] as? Int ?? 0
                var result: String?
                var end = false
                if method == "daemon.hello", let helloError {
                    let out = Array(#"{"jsonrpc":"2.0","id":\#(id),"error":\#(helloError)}"#.utf8) + [0x0a]
                    _ = out.withUnsafeBytes { write(fd, $0.baseAddress, $0.count) }
                    continue
                } else if method == "daemon.hello" {
                    result = #"{"api_version":1,"build":"\#(build)","pid":1}"#
                } else {
                    (result, end) = answer(method, req["params"] as? [String: Any] ?? [:])
                }
                if let result {
                    let out = Array(#"{"jsonrpc":"2.0","id":\#(id),"result":\#(result)}"#.utf8) + [0x0a]
                    _ = out.withUnsafeBytes { write(fd, $0.baseAddress, $0.count) }
                }
                if end { return }
            }
        }
    }

    private var stopped = false
    private var exited = false

    func stop() {
        lock.withLock {
            guard !stopped else { return }
            stopped = true
            close(listener)
            unlink(DaemonAPI.socketPath(home: home))
        }
    }

    func exit() {
        stop()
        lock.withLock {
            exited = true
            for fd in conns { shutdown(fd, SHUT_RDWR) }
        }
    }
}

func shortHome() throws -> String {
    let dir = "/tmp/bv.\(UUID().uuidString.prefix(6))"
    try FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
    return dir
}

@MainActor
func until(_ seconds: Double = 5, _ cond: () -> Bool) async throws {
    let end = Date().addingTimeInterval(seconds)
    while !cond() && Date() < end { try await Task.sleep(for: .milliseconds(10)) }
}

@MainActor
@Suite struct ReleaseEnvironmentTests {
    @Test func theHooksAreIgnoredWhenTheEnvironmentIsNot() {
        let env = ["FEV_HOME": "/x", "FEV_DAEMON": "/bin/sh", "FEV_DAEMON_ARGS": "--fake-vm", "LANG": "fr_FR.UTF-8", "HOME": "/elsewhere"]
        let debug = DaemonPlace.resolve(build: "b", environment: env, honourEnvironment: true)
        #expect(debug.fevd == "/bin/sh" && debug.daemonArgs == ["--fake-vm"] && debug.daemonEnvironment == nil)
        let release = DaemonPlace.resolve(build: "b", environment: env, honourEnvironment: false)
        #expect(debug.home == "/x", "a debug build's home is FEV_HOME, as the CLI's")
        #expect(release.home == DaemonPlace.userHome() + "/.fev", "a release build's is ~/.fev alone")
        #expect(release.fevd != "/bin/sh" && release.daemonArgs.isEmpty)
        let e = release.daemonEnvironment ?? [:]
        #expect(Set(e.keys) == ["PATH", "HOME", "USER", "LANG", "TMPDIR"])
        #expect(e["HOME"] != "/elsewhere" && e["HOME"] == DaemonPlace.userHome(), "from the user database, not $HOME")
        #expect(e["PATH"] == "/usr/bin:/bin:/usr/sbin:/sbin" && e["LANG"] == "fr_FR.UTF-8")
        #expect(DaemonPlace.minimalEnvironment(lang: "x;rm -rf /")["LANG"] == "en_US.UTF-8")
    }

    #if DEBUG
    @Test func aDebugBuildHonoursTheEnvironment() { #expect(DaemonPlace.honoursEnvironment) }
    #else
    @Test func aReleaseBuildDoesNot() {
        #expect(!DaemonPlace.honoursEnvironment)
        #expect(DaemonPlace.resolve(build: "t", environment: ["FEV_HOME": "/tmp/elsewhere"]).home == DaemonPlace.userHome() + "/.fev")
    }
    #endif

    @Test func fevdIsStartedWithTheMinimalEnvironment() async throws {
        let home = try shortHome()
        defer { try? FileManager.default.removeItem(atPath: home) }
        let trap = home + "/fevd"
        try "#!/bin/sh\n/usr/bin/env > \(home)/env.txt\necho \"$@\" > \(home)/args.txt\nexit 1\n".write(toFile: trap, atomically: true, encoding: .utf8)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: trap)
        setenv("FEV_VMM", "/tmp/evil-vmm", 1)
        defer { unsetenv("FEV_VMM") }
        let place = DaemonPlace(home: home, build: "t", fevd: trap, daemonEnvironment: DaemonPlace.minimalEnvironment(lang: nil))
        let m = AppModel(place: place)
        m.show()
        try await until { if case .failed = m.link { return true }; return false }
        m.hide()
        let text = try String(contentsOfFile: home + "/env.txt", encoding: .utf8)
        let keys = Set(text.split(separator: "\n").compactMap { $0.split(separator: "=", maxSplits: 1).first.map(String.init) })
        #expect(keys.subtracting(["PWD", "SHLVL", "_", "OLDPWD"]) == ["PATH", "HOME", "USER", "LANG", "TMPDIR"], "\(keys)")
        #expect(!text.contains("FEV_VMM"))
        #expect(try String(contentsOfFile: home + "/args.txt", encoding: .utf8) == "--home \(home)\n")
    }
}

@MainActor
@Suite struct StopTests {
    @Test func stopIsFevomStopsTwoCallsWithTheMoment() async throws {
        let home = try shortHome()
        defer { try? FileManager.default.removeItem(atPath: home) }
        let d = try SocketDaemon(home: home, build: "t") { method, _ in
            switch method {
            case "vm.stop": return (#"{"seq":3,"state":"stopped","engine":"stopped"}"#, false)
            case "daemon.shutdown": return ("{}", true)
            default: return ("{}", false)
            }
        }
        defer { d.stop() }
        let m = AppModel(place: DaemonPlace(home: home, build: "t", fevd: nil))
        let before = clock_gettime_nsec_np(CLOCK_UPTIME_RAW)
        await m.stopService()
        #expect(m.lastError == nil)
        let reqs = d.requests
        #expect(reqs.map { $0["method"] as? String ?? "" } == ["daemon.hello", "vm.stop", "daemon.shutdown"])
        #expect(d.markAt.allSatisfy { $0 }, "the mark is left before anybody is asked")
        let stopP = reqs[1]["params"] as? [String: Any] ?? [:]
        let downP = reqs[2]["params"] as? [String: Any] ?? [:]
        let began = (stopP["stop_began_ns"] as? NSNumber)?.uint64Value ?? 0
        #expect(began >= before && began <= clock_gettime_nsec_np(CLOCK_UPTIME_RAW))
        #expect((downP["stop_began_ns"] as? NSNumber)?.uint64Value == began)
        #expect(downP["stop_requested"] as? Bool == true)
        #expect(FileManager.default.fileExists(atPath: DaemonAPI.stopMarkPath(home: home)))
    }

    @Test func theMarkIsLeftAgainOnceTheDaemonIsGone() async throws {
        let home = try shortHome()
        defer { try? FileManager.default.removeItem(atPath: home) }
        let d = try SocketDaemon(home: home, build: "t") { method, _ in
            switch method {
            case "vm.stop":
                try? FileManager.default.removeItem(atPath: DaemonAPI.stopMarkPath(home: home))
                return (#"{"seq":3,"state":"stopped","engine":"stopped"}"#, false)
            case "daemon.shutdown": return ("{}", true)
            default: return ("{}", false)
            }
        }
        defer { d.stop() }
        let m = AppModel(place: DaemonPlace(home: home, build: "t", fevd: nil))
        await m.stopService()
        #expect(m.lastError == nil)
        #expect(d.markAt == [true, true, false], "the mark was gone by the shutdown")
        #expect(FileManager.default.fileExists(atPath: DaemonAPI.stopMarkPath(home: home)), "left again after the daemon's end")
    }

    @Test func aSupersededStopSaysSo() async throws {
        let home = try shortHome()
        defer { try? FileManager.default.removeItem(atPath: home) }
        let d = try SocketDaemon(home: home, build: "t") { method, _ in
            switch method {
            case "vm.stop", "daemon.status": return (#"{"seq":9,"state":"running","engine":"ready"}"#, false)
            case "daemon.shutdown": return (#"{"superseded":true}"#, false)
            default: return ("{}", false)
            }
        }
        defer { d.stop() }
        let m = AppModel(place: DaemonPlace(home: home, build: "t", fevd: nil))
        await m.stopService()
        #expect(m.lastError == nil)
        #expect(m.lastInfo?.contains("Started again") == true)
        #expect(m.status?.state == "running")
    }

    @Test func stopWithNoDaemonLeavesTheMark() async throws {
        let home = try shortHome()
        defer { try? FileManager.default.removeItem(atPath: home) }
        let m = AppModel(place: DaemonPlace(home: home, build: "t", fevd: "/usr/bin/false"))
        await m.stopService()
        #expect(m.lastError == nil && m.counters.daemonStarts == 0)
        #expect(FileManager.default.fileExists(atPath: DaemonAPI.stopMarkPath(home: home)))
    }

    @Test func anotherBuildIsStoppedByItsPid() async throws {
        let home = try shortHome()
        defer { try? FileManager.default.removeItem(atPath: home) }
        let refusal = #"{"code":-32003,"message":"build mismatch","data":{"daemon_build":"old","client_build":"t","pid":4242,"state":"running"}}"#
        let d = try SocketDaemon(home: home, build: "old", helloError: refusal) { _, _ in ("{}", false) }
        defer { d.stop() }
        let asked = Mutex<[pid_t]>([])
        let m = AppModel(place: DaemonPlace(home: home, build: "t", fevd: nil))
        m.terminate = { pid in
            asked.withLock { $0.append(pid) }
            d.exit()
            return 0
        }
        await m.stopService()
        #expect(m.lastError == nil, "\(m.lastError ?? "")")
        #expect(asked.withLock { $0 } == [4242])
        #expect(m.lastInfo?.contains("another build (old, pid 4242)") == true)
        #expect(d.requests.map { $0["method"] as? String ?? "" } == ["daemon.hello"])
        #expect(FileManager.default.fileExists(atPath: DaemonAPI.stopMarkPath(home: home)))
    }

    @Test func anotherBuildThatDoesNotGoIsSaid() async throws {
        let home = try shortHome()
        defer { try? FileManager.default.removeItem(atPath: home) }
        let refusal = #"{"code":-32003,"message":"build mismatch","data":{"daemon_build":"old","client_build":"t","pid":4242,"state":"running"}}"#
        let d = try SocketDaemon(home: home, build: "old", helloError: refusal) { _, _ in ("{}", false) }
        defer { d.stop() }
        let m = AppModel(place: DaemonPlace(home: home, build: "t", fevd: nil))
        m.terminate = { _ in 0 }
        m.otherBuildStopBound = .milliseconds(300)
        let t0 = Date()
        await m.stopService()
        #expect(m.lastError?.contains("had not gone") == true)
        #expect(Date().timeIntervalSince(t0) < 5)
    }

    @Test func anotherBuildThatSaysNothingIsNotStoppedAndTheMarkGoes() async throws {
        let home = try shortHome()
        defer { try? FileManager.default.removeItem(atPath: home) }
        let d = try SocketDaemon(home: home, build: "other") { _, _ in ("{}", false) }
        defer { d.stop() }
        let m = AppModel(place: DaemonPlace(home: home, build: "t", fevd: nil))
        await m.stopService()
        #expect(m.lastError != nil)
        #expect(!FileManager.default.fileExists(atPath: DaemonAPI.stopMarkPath(home: home)))
        #expect(!d.requests.contains { $0["method"] as? String == "vm.stop" })
    }
}

@MainActor
@Suite struct ReconnectTests {
    @Test func theWatchFollowsTheRunDirectoryAsItIsMade() async throws {
        let home = try shortHome() + "/h"
        defer { try? FileManager.default.removeItem(atPath: (home as NSString).deletingLastPathComponent) }
        var events = 0
        let w = RunDirWatch(home: home) { events += 1 }
        #expect(w.place == (home as NSString).deletingLastPathComponent, "no home yet: its parent")
        try FileManager.default.createDirectory(atPath: home, withIntermediateDirectories: true)
        try await until { w.place == home }
        try FileManager.default.createDirectory(atPath: home + "/run", withIntermediateDirectories: true)
        try await until { w.place == home + "/run" }
        let before = events
        FileManager.default.createFile(atPath: home + "/run/fevd.sock", contents: nil)
        try await until { events > before }
        #expect(events > before)
        w.cancel()
        #expect(w.place == "")
    }

    @Test func aStoppedModelReconnectsWhenTheDaemonComesBack() async throws {
        let home = try shortHome()
        defer { try? FileManager.default.removeItem(atPath: home) }
        FileManager.default.createFile(atPath: DaemonAPI.stopMarkPath(home: home), contents: nil)
        let m = AppModel(place: DaemonPlace(home: home, build: "t", fevd: "/usr/bin/false"))
        m.show()
        try await until { m.link == .stoppedByUser }
        #expect(m.link == .stoppedByUser && m.isWatching)
        try FileManager.default.removeItem(atPath: DaemonAPI.stopMarkPath(home: home))
        let d = try SocketDaemon(home: home, build: "t") { method, _ in
            switch method {
            case "events.subscribe": return (#"{"subscription":1,"status":{"seq":1,"state":"running","engine":"ready"}}"#, false)
            case "machines.list": return (#"{"machines":[],"default":""}"#, false)
            default: return ("{}", false)
            }
        }
        defer { d.stop() }
        try await until { m.link == .connected }
        #expect(m.link == .connected && m.counters.connects == 1 && m.counters.daemonStarts == 0 && m.counters.watchEvents >= 1)
        #expect(!m.isWatching, "connected: nothing watched")
        m.hide()
        #expect(!m.isWatching && m.link == .idle)
    }

    @Test func noViewerNoWatch() async throws {
        let home = try shortHome()
        defer { try? FileManager.default.removeItem(atPath: home) }
        FileManager.default.createFile(atPath: DaemonAPI.stopMarkPath(home: home), contents: nil)
        let m = AppModel(place: DaemonPlace(home: home, build: "t", fevd: nil))
        m.show()
        try await until { m.link == .stoppedByUser }
        m.hide()
        #expect(!m.isWatching)
    }
}

@MainActor
@Suite struct OperationViewerTests {
    func op(_ state: String, method: String = DaemonAPI.Const.methodMachinesCreate, error: String = "") -> DaemonAPI.Operation {
        .init(id: "op1", method: method, target: "dev", state: state, error: error)
    }

    @Test func anOperationHoldsAViewerUntilItsEnd() throws {
        let m = AppModel(place: DaemonPlace(home: "/nonexistent-fev-home", build: "t", fevd: nil))
        var told: [AppNotice] = []
        m.notify = { told.append($0) }
        m.began(op(DaemonAPI.Const.operationRunning))
        #expect(m.isActive, "the window may close: the operation still looks")
        m.apply(try note(DaemonAPI.Methods.operationProgress, .init(subscription: 1, operation: op(DaemonAPI.Const.operationRunning))))
        #expect(m.isActive && told.isEmpty)
        m.apply(try note(DaemonAPI.Methods.operationProgress, .init(subscription: 1, operation: op(DaemonAPI.Const.operationDone))))
        #expect(!m.isActive && m.operations.isEmpty)
        #expect(told == [AppNotice(title: "dev is ready", body: "The machine is created and running.")])
    }

    @Test func aFailedOperationIsToldWithItsError() throws {
        let m = AppModel(place: DaemonPlace(home: "/nonexistent-fev-home", build: "t", fevd: nil))
        var told: [AppNotice] = []
        m.notify = { told.append($0) }
        m.began(op(DaemonAPI.Const.operationRunning))
        m.apply(try note(DaemonAPI.Methods.operationProgress, .init(subscription: 1, operation: op(DaemonAPI.Const.operationFailed, error: "no such release"))))
        #expect(told.first?.title == "Failed creating dev" && told.first?.body == "no such release")
        #expect(!m.isActive)
        m.apply(try note(DaemonAPI.Methods.operationProgress, .init(subscription: 1, operation: .init(id: "other", method: "machines.create", target: "x", state: "done"))))
        #expect(told.count == 1 && !m.isActive)
    }
}
