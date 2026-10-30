import Darwin
import Foundation

extension DockerClient {
    public struct ExecState: Decodable, Sendable, Equatable {
        public var Running: Bool
        public var ExitCode: Int?
    }

    public func createExec(container: String, cmd: [String], env: [String] = [], user: String = "", rows: Int, cols: Int) async throws -> String {
        struct Config: Encodable {
            let AttachStdin = true, AttachStdout = true, AttachStderr = true, Tty = true
            let Cmd: [String]
            let Env: [String]
            let User: String
            let ConsoleSize: [Int]?
        }
        struct Made: Decodable { let Id: String }
        let size = rows > 0 && cols > 0 ? [rows, cols] : nil
        let body = try JSONEncoder().encode(Config(Cmd: cmd, Env: env, User: user, ConsoleSize: size))
        return try JSONDecoder().decode(Made.self, from: try await request("POST", "/containers/\(container)/exec", body: body)).Id
    }

    public func resizeExec(_ id: String, rows: Int, cols: Int) async throws {
        _ = try await request("POST", "/exec/\(id)/resize?h=\(rows)&w=\(cols)")
    }

    public func inspectExec(_ id: String) async throws -> ExecState {
        try JSONDecoder().decode(ExecState.self, from: try await request("GET", "/exec/\(id)/json"))
    }

    public func startExec(_ id: String, on socket: Int32) async throws -> [UInt8] {
        let path = "/v\(apiVersion)/exec/\(id)/start"
        return try await withCheckedThrowingContinuation { k in
            DispatchQueue.global(qos: .userInitiated).async {
                k.resume(with: Result {
                    do {
                        return try Self.upgrade(socket, path: path, body: Data(#"{"Detach":false,"Tty":true}"#.utf8))
                    } catch {
                        close(socket)
                        throw error
                    }
                })
            }
        }
    }

    public static func openStream(_ socketPath: String) throws -> Int32 {
        let fd = try connectUnix(socketPath)
        _ = fcntl(fd, F_SETFD, FD_CLOEXEC)
        return fd
    }

    static func upgrade(_ fd: Int32, path: String, body: Data) throws -> [UInt8] {
        let head = "POST \(path) HTTP/1.1\r\nHost: docker\r\nUser-Agent: fev-app\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n"
            + "Content-Type: application/json\r\nContent-Length: \(body.count)\r\n\r\n"
        try writeAll(fd, Data(head.utf8) + body)
        var got = [UInt8]()
        var buf = [UInt8](repeating: 0, count: 16 << 10)
        let end: [UInt8] = [13, 10, 13, 10]
        while true {
            var p = pollfd(fd: fd, events: Int16(POLLIN), revents: 0)
            if poll(&p, 1, 30_000) == 0 { throw Failure(status: 0, message: "the engine did not answer the exec's start in 30 s") }
            let n = read(fd, &buf, buf.count)
            if n < 0 {
                if errno == EINTR || errno == EAGAIN { continue }
                throw Failure(status: 0, message: "reading the exec's start: \(String(cString: strerror(errno)))")
            }
            if n == 0 {
                try failed(got)
            }
            got += buf[0..<n]
            if let at = find(end, in: got) {
                let status = Int(String(decoding: got[..<at], as: UTF8.self).split(separator: " ").dropFirst().first ?? "") ?? 0
                if status == 101 || status == 200 { return Array(got[(at + 4)...]) }
                let headText = String(decoding: got[..<at], as: UTF8.self).lowercased()
                let length = headText.components(separatedBy: "\r\n").first { $0.hasPrefix("content-length:") }
                    .flatMap { Int($0.dropFirst("content-length:".count).trimmingCharacters(in: .whitespaces)) } ?? 0
                while got.count - (at + 4) < length {
                    var p = pollfd(fd: fd, events: Int16(POLLIN), revents: 0)
                    if poll(&p, 1, 5_000) <= 0 { break }
                    let n = read(fd, &buf, buf.count)
                    if n <= 0 { break }
                    got += buf[0..<n]
                }
                try failed(got)
            }
            if got.count > 64 << 10 { throw Failure(status: 0, message: "the exec's start answered with a head over 64 KiB") }
        }
    }

    private static func failed(_ got: [UInt8]) throws -> Never {
        _ = try parse(Data(got))
        throw Failure(status: 0, message: "the engine ended the exec's start without taking the connection over")
    }

    private static func find(_ needle: [UInt8], in hay: [UInt8]) -> Int? {
        guard hay.count >= needle.count else { return nil }
        for i in 0...(hay.count - needle.count) where hay[i] == needle[0] && Array(hay[i..<(i + needle.count)]) == needle { return i }
        return nil
    }
}
