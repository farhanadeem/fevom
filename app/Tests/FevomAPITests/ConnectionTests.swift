import Foundation
import Synchronization
import Testing

@testable import FevomAPI

final class FakeDaemon: @unchecked Sendable {
    let fd: Int32
    private let lock = NSLock()
    private(set) var requests: [[String: Any]] = []

    init(fd: Int32) { self.fd = fd }

    func serve(answer: @escaping @Sendable ([String: Any]) -> Data?) {
        Thread.detachNewThread { [self] in
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
                    lock.withLock { requests.append(req) }
                    if let reply = answer(req) { send(reply) }
                }
            }
        }
    }

    func send(_ line: Data) {
        let data = line + Data([0x0a])
        data.withUnsafeBytes { p in
            var off = 0
            while off < p.count {
                let n = write(fd, p.baseAddress! + off, p.count - off)
                if n <= 0 { return }
                off += n
            }
        }
    }

    func close() { Darwin.close(fd) }
}

func socketPair() -> (Int32, Int32) {
    var fds: [Int32] = [0, 0]
    precondition(socketpair(AF_UNIX, SOCK_STREAM, 0, &fds) == 0)
    return (fds[0], fds[1])
}

func reply(id: Any, result: Data) -> Data {
    let idText = String(decoding: try! JSONSerialization.data(withJSONObject: id, options: [.fragmentsAllowed]), as: UTF8.self)
    return Data(#"{"jsonrpc":"2.0","id":\#(idText),"result":"#.utf8) + result + Data("}".utf8)
}

func typedCall<P: DaemonType, R: DaemonType>(_ conn: RPCConnection, _ call: Call<P, R>, params: Data, result: Data) async throws -> Bool {
    let p = try JSONDecoder().decode(P.self, from: params)
    let got = try await conn.call(call, p)
    return got == (try JSONDecoder().decode(R.self, from: result))
}

func anyCall<P: DaemonType, R: DaemonType>(_ conn: RPCConnection, method: String, _ p: P.Type, _ r: R.Type, params: Data, result: Data) async throws -> Bool {
    try await typedCall(conn, Call<P, R>(method), params: params, result: result)
}

@Suite struct ConnectionTests {
    @Test func everyFixtureCallGoesOverTheWire() async throws {
        let fixtures = try loadFixtures().filter { !$0.pending && !$0.notification }
        let (a, b) = socketPair()
        let daemon = FakeDaemon(fd: b)
        let next = Mutex(0)
        daemon.serve { req in
            let f = fixtures[next.withLock { n in defer { n += 1 }; return n }]
            let id = req["id"] ?? 0
            let sent = req["params"] ?? NSNull()
            let want = (try? JSONSerialization.jsonObject(with: f.params, options: [.fragmentsAllowed])) ?? NSNull()
            guard req["method"] as? String == f.method, jsonDiff(sent, want).isEmpty else {
                return Data(#"{"jsonrpc":"2.0","id":\#(id),"error":{"code":-32602,"message":"not the fixture's call"}}"#.utf8)
            }
            return reply(id: id, result: f.result!)
        }
        let conn = RPCConnection(fd: a)
        var failed: [String] = []
        for f in fixtures {
            let entry = DaemonAPI.schema[f.method]!
            do {
                if try await anyCall(conn, method: f.method, entry.params, entry.result!, params: f.params, result: f.result!) == false {
                    failed.append("\(f): result differs")
                }
            } catch {
                failed.append("\(f): \(error)")
            }
        }
        #expect(failed.isEmpty, Comment(rawValue: failed.joined(separator: "\n")))
        #expect(daemon.requests.allSatisfy { $0["jsonrpc"] as? String == "2.0" && $0["id"] != nil })
        conn.close()
        daemon.close()
    }

    @Test func notificationsArriveInOrder() async throws {
        let notes = try loadFixtures().filter { !$0.pending && $0.notification }
        #expect(notes.count >= 10)
        let (a, b) = socketPair()
        let daemon = FakeDaemon(fd: b)
        let conn = RPCConnection(fd: a)
        for f in notes {
            daemon.send(Data(#"{"jsonrpc":"2.0","method":"\#(f.method)","params":"#.utf8) + f.params + Data("}".utf8))
        }
        daemon.close()
        var got: [String] = []
        for await n in conn.notes {
            got.append(n.method)
            if n.method == DaemonAPI.Methods.stateChanged.method {
                let p = try n.decode(DaemonAPI.Methods.stateChanged)
                #expect(p?.status.state.isEmpty == false)
            }
        }
        #expect(got == notes.map(\.method))
        #expect(conn.isClosed)
    }

    @Test func errorsAndTheEnd() async throws {
        let (a, b) = socketPair()
        let daemon = FakeDaemon(fd: b)
        daemon.serve { req in
            if req["method"] as? String == DaemonAPI.Methods.daemonHello.method {
                return Data(#"{"jsonrpc":"2.0","id":\#(req["id"]!),"error":{"code":-32003,"message":"build mismatch","data":{"daemon_build":"aaa","client_build":"bbb","pid":42,"state":"running","running_containers":2}}}"#.utf8)
            }
            return nil
        }
        let conn = RPCConnection(fd: a)
        do {
            _ = try await conn.call(DaemonAPI.Methods.daemonHello, .init(apiVersion: 1, build: "bbb", client: "test"))
            Issue.record("no error")
        } catch let e as DaemonError {
            #expect(e.code == DaemonAPI.Const.codeBuildMismatch)
            let m = e.data(as: DaemonAPI.BuildMismatch.self)
            #expect(m?.daemonBuild == "aaa" && m?.runningContainers == 2)
        }
        let pending = Task { try await conn.call(DaemonAPI.Methods.daemonStatus, .init()) }
        try await Task.sleep(for: .milliseconds(50))
        daemon.close()
        await #expect(throws: RPCConnection.Failure.closed) { _ = try await pending.value }
    }
}
