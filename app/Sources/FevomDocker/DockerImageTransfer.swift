import Foundation

extension DockerClient {
    @discardableResult
    public func saveImages(_ refs: [String], to url: URL) async throws -> Int64 {
        guard !refs.isEmpty else { throw Failure(status: 0, message: "no image to export") }
        let query = refs.map { "names=" + ($0.addingPercentEncoding(withAllowedCharacters: .urlQueryAllowed.subtracting(CharacterSet(charactersIn: "&=+"))) ?? $0) }.joined(separator: "&")
        let socketPath = self.socketPath
        let path = "/v\(apiVersion)/images/get?\(query)"
        return try await withCheckedThrowingContinuation { k in
            DispatchQueue.global(qos: .userInitiated).async {
                k.resume(with: Result { try Self.saveToFile(socketPath: socketPath, path: path, url: url) })
            }
        }
    }

    public func loadImages(from url: URL) async throws -> [String] {
        let socketPath = self.socketPath
        let path = "/v\(apiVersion)/images/load?quiet=1"
        return try await withCheckedThrowingContinuation { k in
            DispatchQueue.global(qos: .userInitiated).async {
                k.resume(with: Result { try Self.loadFromFile(socketPath: socketPath, path: path, url: url) })
            }
        }
    }

    static func saveToFile(socketPath: String, path: String, url: URL) throws -> Int64 {
        let fd = try connectUnix(socketPath)
        defer { close(fd) }
        try writeAll(fd, Data("GET \(path) HTTP/1.1\r\nHost: docker\r\nConnection: close\r\nUser-Agent: fev-app\r\n\r\n".utf8))
        let reader = SocketReader(fd: fd)
        let head = try reader.head()
        guard (200..<300).contains(head.status) else {
            throw parseFailure(status: head.status, body: try reader.rest(chunked: head.chunked, limit: 1 << 20))
        }
        let dir = url.deletingLastPathComponent()
        let tmp = dir.appendingPathComponent(".\(url.lastPathComponent).fev-\(UUID().uuidString.prefix(8))")
        guard FileManager.default.createFile(atPath: tmp.path, contents: nil) else {
            throw Failure(status: 0, message: "cannot write in \(dir.path)")
        }
        var kept = false
        defer { if !kept { try? FileManager.default.removeItem(at: tmp) } }
        let out = try FileHandle(forWritingTo: tmp)
        defer { try? out.close() }
        var total: Int64 = 0
        try reader.stream(chunked: head.chunked) { piece in
            try out.write(contentsOf: piece)
            total += Int64(piece.count)
        }
        try out.synchronize()
        if FileManager.default.fileExists(atPath: url.path) { try FileManager.default.removeItem(at: url) }
        try FileManager.default.moveItem(at: tmp, to: url)
        kept = true
        return total
    }

    static func loadFromFile(socketPath: String, path: String, url: URL) throws -> [String] {
        let input = try FileHandle(forReadingFrom: url)
        defer { try? input.close() }
        let size = try input.seekToEnd()
        try input.seek(toOffset: 0)
        let fd = try connectUnix(socketPath)
        defer { close(fd) }
        try writeAll(fd, Data("POST \(path) HTTP/1.1\r\nHost: docker\r\nConnection: close\r\nUser-Agent: fev-app\r\nContent-Type: application/x-tar\r\nContent-Length: \(size)\r\n\r\n".utf8))
        var sent: UInt64 = 0
        while sent < size {
            guard let piece = try input.read(upToCount: 1 << 20), !piece.isEmpty else {
                throw Failure(status: 0, message: "\(url.path) ended at \(sent) of \(size) bytes")
            }
            try writeAll(fd, piece)
            sent += UInt64(piece.count)
        }
        let reader = SocketReader(fd: fd)
        let head = try reader.head()
        let body = try reader.rest(chunked: head.chunked, limit: 16 << 20)
        guard (200..<300).contains(head.status) else { throw parseFailure(status: head.status, body: body) }
        var loaded: [String] = []
        for line in body.split(separator: UInt8(ascii: "\n")) where !line.isEmpty {
            struct Message: Decodable { let stream: String?; let error: String? }
            guard let m = try? JSONDecoder().decode(Message.self, from: Data(line)) else { continue }
            if let e = m.error { throw Failure(status: head.status, message: e) }
            if let s = m.stream?.trimmingCharacters(in: .whitespacesAndNewlines), !s.isEmpty { loaded.append(s) }
        }
        return loaded
    }

    static func parseFailure(status: Int, body: Data) -> Failure {
        struct Message: Decodable { let message: String }
        let message = (try? JSONDecoder().decode(Message.self, from: body).message) ?? String(decoding: body, as: UTF8.self)
        return Failure(status: status, message: message)
    }
}

final class SocketReader {
    let fd: Int32
    private var buffer = Data()
    private var eof = false

    init(fd: Int32) { self.fd = fd }

    struct Head { let status: Int; let chunked: Bool; let length: Int64? }

    private func fill() throws {
        var buf = [UInt8](repeating: 0, count: 1 << 20)
        while true {
            var p = pollfd(fd: fd, events: Int16(POLLIN), revents: 0)
            if poll(&p, 1, -1) < 0 {
                if errno == EINTR { continue }
                throw DockerClient.Failure(status: 0, message: "poll: \(String(cString: strerror(errno)))")
            }
            let n = read(fd, &buf, buf.count)
            if n < 0 {
                if errno == EINTR { continue }
                throw DockerClient.Failure(status: 0, message: "read: \(String(cString: strerror(errno)))")
            }
            if n == 0 { eof = true } else { buffer.append(contentsOf: buf[0..<n]) }
            return
        }
    }

    func head() throws -> Head {
        let sep = Data("\r\n\r\n".utf8)
        while buffer.firstRange(of: sep) == nil {
            if eof { throw DockerClient.Failure(status: 0, message: "no HTTP response") }
            if buffer.count > 64 << 10 { throw DockerClient.Failure(status: 0, message: "an HTTP head over 64 KiB") }
            try fill()
        }
        let r = buffer.firstRange(of: sep)!
        let lines = String(decoding: buffer[buffer.startIndex..<r.lowerBound], as: UTF8.self).components(separatedBy: "\r\n")
        buffer = Data(buffer[r.upperBound...])
        let status = Int(lines.first?.split(separator: " ").dropFirst().first ?? "") ?? 0
        var chunked = false
        var length: Int64?
        for l in lines.dropFirst() {
            let lower = l.lowercased()
            if lower.hasPrefix("transfer-encoding:") && lower.contains("chunked") { chunked = true }
            if lower.hasPrefix("content-length:") { length = Int64(l.dropFirst("content-length:".count).trimmingCharacters(in: .whitespaces)) }
        }
        return Head(status: status, chunked: chunked, length: length)
    }

    func stream(chunked: Bool, _ take: (Data) throws -> Void) throws {
        if !chunked {
            while true {
                if !buffer.isEmpty {
                    try take(buffer)
                    buffer.removeAll(keepingCapacity: true)
                }
                if eof { return }
                try fill()
            }
        }
        while true {
            let line = try readLine()
            let sizeText = line.split(separator: ";").first.map(String.init) ?? ""
            guard let size = Int(sizeText.trimmingCharacters(in: .whitespaces), radix: 16) else {
                throw DockerClient.Failure(status: 0, message: "bad chunk size")
            }
            if size == 0 { return }
            var left = size
            while left > 0 {
                if buffer.isEmpty {
                    if eof { throw DockerClient.Failure(status: 0, message: "the response ended inside a chunk") }
                    try fill()
                    continue
                }
                let n = min(left, buffer.count)
                try take(Data(buffer.prefix(n)))
                buffer = Data(buffer.dropFirst(n))
                left -= n
            }
            _ = try readLine()
        }
    }

    func rest(chunked: Bool, limit: Int) throws -> Data {
        var out = Data()
        try stream(chunked: chunked) { piece in
            out.append(piece)
            if out.count > limit { throw DockerClient.Failure(status: 0, message: "a response over \(limit) bytes") }
        }
        return out
    }

    private func readLine() throws -> String {
        let crlf = Data("\r\n".utf8)
        while buffer.firstRange(of: crlf) == nil {
            if eof { throw DockerClient.Failure(status: 0, message: "the response ended inside a chunk's head") }
            try fill()
        }
        let r = buffer.firstRange(of: crlf)!
        let line = String(decoding: buffer[buffer.startIndex..<r.lowerBound], as: UTF8.self)
        buffer = Data(buffer[r.upperBound...])
        return line
    }
}
