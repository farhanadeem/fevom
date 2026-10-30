import Foundation
import Testing

@testable import FevomAPI
@testable import FevomDocker
@testable import FevomStores

struct NoteBody<Q: Encodable>: Encodable {
    var jsonrpc = "2.0"
    let method: String
    let params: Q
}

func note<P: DaemonType>(_ n: Notice<P>, _ p: P) throws -> RPCConnection.Note {
    RPCConnection.Note(method: n.method, line: try JSONEncoder.daemon.encode(NoteBody(method: n.method, params: p)))
}

@MainActor
@Suite struct AppModelTests {
    func model() -> AppModel {
        AppModel(place: DaemonPlace(home: "/nonexistent-fev-home", build: "test", fevd: nil))
    }

    @Test func stateFollowsTransitionsAndIgnoresStaleOnes() throws {
        let m = model()
        m.load(status: .init(seq: 5, state: "starting", phase: "kernel", engine: "stopped"), machines: [])
        m.apply(try note(DaemonAPI.Methods.stateChanged, .init(subscription: 2, status: .init(seq: 6, state: "running", engine: "starting"))))
        #expect(m.status?.state == "running")
        m.apply(try note(DaemonAPI.Methods.stateChanged, .init(subscription: 2, status: .init(seq: 4, state: "stopped", engine: "stopped"))))
        #expect(m.status?.seq == 6, "an older transition is not applied")
        #expect(m.counters.notifications == 2)
    }

    @Test func machinesChangedAddsUpdatesAndRemoves() throws {
        let m = model()
        let dev = DaemonAPI.Machine(name: "dev", state: "running", default: true)
        m.load(status: nil, machines: [dev], default: "dev")
        m.apply(try note(DaemonAPI.Methods.machinesChanged, .init(machine: .init(name: "alp", state: "creating"))))
        #expect(m.machines.map(\.name) == ["alp", "dev"])
        m.apply(try note(DaemonAPI.Methods.machinesChanged, .init(machine: .init(name: "alp", state: "running"))))
        #expect(m.machines.first?.state == "running")
        m.apply(try note(DaemonAPI.Methods.machinesChanged, .init(machine: .init(name: "dev", state: "stopped"), removed: true)))
        #expect(m.machines.map(\.name) == ["alp"])
    }

    @Test func showAndHideOpenAndCloseEverything() async throws {
        let m = model()
        #expect(!m.isActive && m.link == .idle)
        m.show()
        m.show()
        m.hide()
        #expect(m.isActive, "still one viewer")
        m.hide()
        #expect(!m.isActive && m.link == .idle)
        m.apply(try note(DaemonAPI.Methods.dockerEvent, .init(type: "container", action: "start", id: "x")))
        try await Task.sleep(for: .milliseconds(20))
        #expect(m.counters.containerFetches == 0)
    }
}

@MainActor
@Suite struct DaemonStartTests {
    func tempHome() throws -> String {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("bh-\(UUID().uuidString.prefix(8))")
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        return dir.path
    }

    @Test func aStopMarkMeansNothingIsStarted() async throws {
        let home = try tempHome()
        FileManager.default.createFile(atPath: DaemonAPI.stopMarkPath(home: home), contents: nil)
        let trap = home + "/fevd"
        try "#!/bin/sh\ntouch \(home)/ran\n".write(toFile: trap, atomically: true, encoding: .utf8)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: trap)
        let m = AppModel(place: DaemonPlace(home: home, build: "test", fevd: trap))
        m.show()
        for _ in 0..<100 where m.link == .connecting || m.link == .idle { try await Task.sleep(for: .milliseconds(10)) }
        #expect(m.link == .stoppedByUser)
        #expect(!FileManager.default.fileExists(atPath: home + "/ran"))
        m.hide()
    }

    @Test func noFevdIsSaid() async throws {
        let m = AppModel(place: DaemonPlace(home: try tempHome(), build: "test", fevd: nil))
        m.show()
        for _ in 0..<100 where m.link == .connecting || m.link == .idle { try await Task.sleep(for: .milliseconds(10)) }
        guard case .failed(let why) = m.link else { Issue.record("link is \(m.link)"); return }
        #expect(why.contains(DaemonPlace.honoursEnvironment ? "FEV_DAEMON" : "Contents/Helpers/fevd"))
        m.hide()
    }

    @Test func placeFromTheEnvironment() {
        let p = DaemonPlace.resolve(build: "b", environment: ["FEV_HOME": "/x", "FEV_DAEMON": "/bin/sh", "FEV_DAEMON_ARGS": "--fake-vm --no-autostart"],
                                    honourEnvironment: true)
        #expect(p.home == "/x" && p.fevd == "/bin/sh" && p.daemonArgs == ["--fake-vm", "--no-autostart"])
        #expect(DaemonPlace.resolve(build: "b", environment: [:]).home == NSHomeDirectory() + "/.fev")
    }
}

@Suite struct LogBufferTests {
    @Test func boundedByCapacityInWholeChunks() {
        var b = LogBuffer(capacity: 1000, chunkLines: 100)
        for i in 0..<5000 { b.append(Array("line \(i)".utf8)) }
        #expect(b.count <= 1100 && b.count >= 1000)
        #expect(b.line(b.endSeq - 1) == "line 4999")
        #expect(b.line(b.firstSeq) == "line \(b.firstSeq)")
        #expect(b.line(b.firstSeq - 1) == nil)
    }

    @Test func filterIsCaseFoldedAndFollowsAppends() {
        var b = LogBuffer(capacity: 1000, chunkLines: 64)
        for i in 0..<300 { b.append(Array((i % 3 == 0 ? "ERROR at \(i)" : "ok \(i)").utf8)) }
        var (rows, upTo) = LogRows.scanned(LogFilter("error"), b)
        #expect(rows.count(b) == 100 && upTo == 300)
        let from = b.endSeq
        for i in 300..<330 { b.append(Array((i % 3 == 0 ? "Error late \(i)" : "ok").utf8)) }
        rows.appended(from: from, b)
        #expect(rows.count(b) == 110)
        #expect(b.line(rows.seq(row: 109, b)) == "Error late 327")
        let from2 = b.endSeq
        for _ in 0..<2000 { b.append(Array("ok".utf8)) }
        rows.appended(from: from2, b)
        #expect(rows.count(b) == 0)
        #expect(LogFilter("").matches("anything"))
        #expect(!LogFilter("zz").matches("z"))
    }
}

@Suite struct DockerClientTests {
    @Test func parsesContentLengthAndChunkedReplies() throws {
        let plain = Data("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 2\r\n\r\n[]".utf8)
        #expect(try DockerClient.parse(plain) == Data("[]".utf8))
        let chunked = Data("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n3\r\n[{}\r\n1\r\n]\r\n0\r\n\r\n".utf8)
        #expect(try DockerClient.parse(chunked) == Data("[{}]".utf8))
        let err = Data("HTTP/1.1 404 Not Found\r\nContent-Length: 27\r\n\r\n{\"message\":\"no such thing\"}".utf8)
        #expect(throws: DockerClient.Failure.self) { try DockerClient.parse(err) }
    }

    @Test func decodesTheEngineList() throws {
        let json = #"[{"Id":"abc","Names":["/web"],"Image":"nginx","State":"running","Status":"Up","Created":1,"Ports":[{"IP":"0.0.0.0","PrivatePort":80,"PublicPort":8080,"Type":"tcp"}],"Labels":{"com.docker.compose.project":"shop"}}]"#
        let list = try JSONDecoder().decode([Container].self, from: Data(json.utf8))
        #expect(list.first?.name == "web" && list.first?.composeProject == "shop" && list.first?.ports.first?.publicPort == 8080)
    }

    @Test func anImageIsInUseByItsID() throws {
        let json = #"[{"Id":"a","Names":["/a"],"Image":"docker.io/library/nginx","ImageID":"sha256:n"},{"Id":"b","Names":["/b"],"Image":"host:5000/app","ImageID":"sha256:h"},{"Id":"c","Names":["/c"],"Image":"redis","ImageID":"sha256:r2"}]"#
        let cs = try JSONDecoder().decode([Container].self, from: Data(json.utf8))
        #expect(cs.map(\.imageID) == ["sha256:n", "sha256:h", "sha256:r2"])
        #expect(DockerImage(id: "sha256:n", repoTags: ["nginx:latest"]).inUse(by: cs))
        #expect(DockerImage(id: "sha256:h", repoTags: ["host:5000/app:latest"]).inUse(by: cs))
        #expect(!DockerImage(id: "sha256:r1", repoTags: ["redis:latest"]).inUse(by: cs), "the tag moved on: the old image is not the container's")
        #expect(!DockerImage(id: "sha256:x", repoTags: ["spare:1"]).inUse(by: cs))
        let again = try JSONDecoder().decode([Container].self, from: try JSONEncoder().encode(cs))
        #expect(again.map(\.imageID) == cs.map(\.imageID))
    }
}

@Suite struct LogBufferLongLineTests {
    @Test func chunksCloseByBytesAndEveryLineIsWhole() {
        var b = LogBuffer(capacity: 1000, byteCapacity: 1 << 30, chunkLines: 16384, chunkBytes: 1 << 20, maxLineBytes: 1 << 20)
        for i in 0..<64 { b.append(Array(repeating: UInt8(65 + i % 26), count: 300 << 10) + Array("#\(i)".utf8)) }
        #expect(b.count == 64)
        #expect(b.chunks.allSatisfy { $0.bytes.count <= 1 << 20 }, "no chunk past its byte bound")
        #expect(b.chunks.count >= 16)
        for i in [0, 1, 3, 4, 31, 63] {
            let line = b.bytes(i)!
            #expect(line.count == (300 << 10) + "#\(i)".utf8.count && line.first == UInt8(65 + i % 26))
            #expect(String(decoding: line.suffix(3), as: UTF8.self).hasSuffix("#\(i)"))
        }
        var (rows, _) = LogRows.scanned(LogFilter("#3"), b)
        #expect(rows.count(b) == 11, "#3 and #30 to #39")
        let from = b.endSeq
        b.append(Array("short #3x".utf8))
        rows.appended(from: from, b)
        #expect(b.line(rows.seq(row: rows.count(b) - 1, b)) == "short #3x")
    }

    @Test func sixteenThousandLinesOfQuarterMebibyteDoNotTrap() {
        var b = LogBuffer()
        var line = [UInt8](repeating: 0x61, count: (256 << 10) + 1)
        for i in 0..<16384 {
            line[0] = UInt8(48 + i % 10)
            b.append(line)
        }
        #expect(b.endSeq == 16384)
        #expect(b.lineBytes <= LogBuffer.defaultByteCapacity)
        #expect(b.heldBytes <= LogBuffer.defaultByteCapacity + (8 << 20) + (1 << 20) + 16384 * 4 * 2)
        #expect(b.bytes(16383)?.count == (256 << 10) + 1 && b.bytes(16383)?.first == UInt8(48 + 16383 % 10))
        #expect(b.count < 16384 && b.count > 200, "dropped by bytes, not lines")
    }

    @Test func aLineOverTheMostIsCutAndLinesAreCutForDisplay() {
        var b = LogBuffer(capacity: 10, maxLineBytes: 1000)
        b.append(Array(repeating: 0x62, count: 5000))
        #expect(b.bytes(0)?.count == 1000 && b.cutLines == 1)
        b.append(Array("héllo wörld".utf8))
        let d = b.display(1, limit: 2)
        #expect(d?.text == "h" && d?.more == "héllo wörld".utf8.count - 1)
        #expect(b.display(1, limit: 100)?.more == 0)
        #expect(b.cells(1, limit: 100) == 11)
        b.append(Array("a\tb".utf8))
        #expect(b.cells(2, limit: 100) == 9)
        b.clear()
        #expect(b.count == 0 && b.line(2) == nil && b.endSeq == 3)
    }
}

@MainActor
@Suite struct TerminalSessionEndTests {
    func session() -> (TerminalSession, Int32, () -> [String]) {
        var fds: [Int32] = [0, 0]
        precondition(socketpair(AF_UNIX, SOCK_STREAM, 0, &fds) == 0)
        let t = TerminalSession(place: DaemonPlace(home: "/nonexistent-fev-home", build: "test", fevd: nil), machine: "m")
        var events: [String] = []
        t.onOutput = { events.append("out:" + String(decoding: $0, as: UTF8.self)) }
        t.onExit = { events.append("exit:" + $0) }
        t.follow(fds[0])
        return (t, fds[1], { events })
    }

    func until(_ cond: () -> Bool) async throws {
        let end = Date().addingTimeInterval(5)
        while !cond() && Date() < end { try await Task.sleep(for: .milliseconds(5)) }
    }

    @Test func anExitBeforeTheLastOutputIsToldAfterIt() async throws {
        let (t, peer, events) = session()
        defer { t.close() }
        t.ended(.init(id: 1, exitCode: 0, streamEnded: true))
        #expect(events().isEmpty, "told before the stream's end: \(events())")
        _ = "last words".withCString { write(peer, $0, 10) }
        close(peer)
        try await until { events().count >= 2 }
        #expect(events() == ["out:last words", "exit:exited with status 0"])
    }

    @Test func aStreamThatEndsFirstWaitsForTheExit() async throws {
        let (t, peer, events) = session()
        defer { t.close() }
        _ = "bye".withCString { write(peer, $0, 3) }
        close(peer)
        try await until { !events().isEmpty }
        try await Task.sleep(for: .milliseconds(50))
        #expect(events() == ["out:bye"])
        t.ended(.init(id: 1, exitCode: -1, signal: 2, streamEnded: true))
        #expect(events() == ["out:bye", "exit:ended by signal 2"])
        t.connectionEnded()
        #expect(events().count == 2, "told once")
    }

    @Test func theDaemonsOwnExitIsToldAtOnce() async throws {
        let (t, peer, events) = session()
        defer { t.close(); close(peer) }
        t.ended(.init(id: 1, exitCode: -1, error: "the VM stopped"))
        #expect(events() == ["exit:the VM stopped"])
        let (u, peer2, events2) = session()
        defer { u.close() }
        close(peer2)
        try await Task.sleep(for: .milliseconds(100))
        #expect(events2().isEmpty, "a stream's end alone tells nothing")
        u.connectionEnded()
        #expect(events2() == ["exit:the session's stream ended"])
    }
}

@Suite struct ListFilterTests {
    @Test func theContainersListFollowsWhatIsTyped() throws {
        let json = #"""
        [{"Id":"w","Names":["/shop-web-1"],"Image":"nginx:alpine","Labels":{"com.docker.compose.project":"shop","com.docker.compose.service":"web"}},
         {"Id":"d","Names":["/shop-db-1"],"Image":"alpine:latest","Labels":{"com.docker.compose.project":"shop","com.docker.compose.service":"db"}},
         {"Id":"s","Names":["/solo"],"Image":"alpine:latest"}]
        """#
        let cs = try JSONDecoder().decode([Container].self, from: Data(json.utf8))
        #expect(ContainerRow.rows(cs).map(\.id) == ["project:shop", "s"])
        let shop = ContainerRow.rows(cs, filter: "SHOP")
        #expect(shop.map(\.id) == ["project:shop"])
        #expect(shop.first?.children?.count == 2)
        let web = ContainerRow.rows(cs, filter: "nginx")
        #expect(web.map(\.id) == ["project:shop"])
        #expect(web.first?.children?.map(\.id) == ["w"])
        #expect(ContainerRow.rows(cs, filter: " Solo ").map(\.id) == ["s"])
        #expect(ContainerRow.rows(cs, filter: "redis").isEmpty)
    }
}

@MainActor
@Suite struct ListErrorTests {
    struct Said: Error, CustomStringConvertible { let description: String }

    func model() -> AppModel {
        AppModel(place: DaemonPlace(home: "/nonexistent-fev-home", build: "test", fevd: nil))
    }

    @Test func aListReadTakesBackOnlyItsOwnFailure() {
        let m = model()
        m.listFailed(.list(.volumes), Said(description: "volumes: cannot decode"))
        m.listSucceeded(.containers)
        #expect(m.lastError == "volumes: cannot decode", "the containers' read took back the volumes' failure")
        m.listSucceeded(.list(.networks))
        #expect(m.lastError == "volumes: cannot decode")
        m.listSucceeded(.list(.volumes))
        #expect(m.lastError == nil)

        m.listFailed(.containers, Said(description: "503"))
        m.listFailed(.list(.images), Said(description: "503"))
        m.listSucceeded(.containers)
        #expect(m.lastError == "503")
        m.listSucceeded(.list(.images))
        #expect(m.lastError == nil)

        m.listFailed(.list(.images), Said(description: "503"))
        m.lastError = "the network is in use"
        m.listSucceeded(.list(.images))
        #expect(m.lastError == "the network is in use")
    }

    @Test func aNetworksEventsAreCountedUntilItGoes() throws {
        let m = model()
        let ev = { (action: String) in try note(DaemonAPI.Methods.dockerEvent, .init(subscription: 1, type: "network", action: action, id: "n1")) }
        m.apply(try ev("create"))
        m.apply(try ev("connect"))
        #expect(m.networkEvents["n1"] == 2)
        m.apply(try ev("disconnect"))
        #expect(m.networkEvents["n1"] == 3)
        m.apply(try ev("destroy"))
        #expect(m.networkEvents["n1"] == nil)
    }
}
