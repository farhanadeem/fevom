import Foundation
import Testing

@testable import FevomDocker

@Suite struct ExecTests {
    func pair() -> (Int32, Int32) {
        var fds: [Int32] = [0, 0]
        _ = socketpair(AF_UNIX, SOCK_STREAM, 0, &fds)
        return (fds[0], fds[1])
    }

    func answer(_ fd: Int32, _ parts: [String]) {
        DispatchQueue.global().async {
            var buf = [UInt8](repeating: 0, count: 4096)
            _ = read(fd, &buf, buf.count)
            for p in parts {
                _ = p.utf8CString.withUnsafeBufferPointer { write(fd, $0.baseAddress, $0.count - 1) }
                usleep(50_000)
            }
        }
    }

    @Test func upgradedWithTheStreamsFirstBytes() throws {
        let (a, b) = pair()
        defer { close(a); close(b) }
        answer(b, ["HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\nfake# "])
        let first = try DockerClient.upgrade(a, path: "/v1.44/exec/x/start", body: Data("{}".utf8))
        #expect(String(decoding: first, as: UTF8.self) == "fake# ")
    }

    @Test func anEngineThatDoesNotUpgrade() throws {
        let (a, b) = pair()
        defer { close(a); close(b) }
        answer(b, ["HTTP/1.1 200 OK\r\nContent-Type: application/vnd.docker.raw-stream\r\n\r\n"])
        #expect(try DockerClient.upgrade(a, path: "/x", body: Data()).isEmpty)
    }

    @Test func theEnginesErrorWithItsBodyLater() throws {
        let (a, b) = pair()
        defer { close(a); close(b) }
        let body = #"{"message":"container abc is not running"}"#
        answer(b, ["HTTP/1.1 409 Conflict\r\nContent-Type: application/json\r\nContent-Length: \(body.utf8.count)\r\n\r\n", body])
        #expect {
            try DockerClient.upgrade(a, path: "/x", body: Data())
        } throws: { e in
            (e as? DockerClient.Failure).map { $0.status == 409 && $0.message == "container abc is not running" } ?? false
        }
    }
}
