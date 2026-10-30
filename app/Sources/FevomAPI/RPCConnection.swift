import Foundation
import Synchronization

public final class RPCConnection: Sendable {
    public struct Note: Sendable {
        public let method: String
        public let line: Data
    }

    public enum Failure: Error, Sendable, Equatable, CustomStringConvertible {
        case closed
        case connect(String, Int32)
        case pathTooLong(String)
        case protocolError(String)

        public var description: String {
            switch self {
            case .closed: "the daemon's connection has ended"
            case .connect(let path, let errno): "cannot connect to \(path): \(String(cString: strerror(errno)))"
            case .pathTooLong(let path): "socket path too long for macOS (103 bytes): \(path)"
            case .protocolError(let what): "Daemon API: \(what)"
            }
        }
    }

    public static let maxMessage = 4 << 20

    private struct State {
        var nextID: UInt64 = 1
        var pending: [UInt64: CheckedContinuation<Data, any Error>] = [:]
        var buffer = Data()
        var closed = false
    }

    private let fd: Int32
    private let queue: DispatchQueue
    private let state = Mutex(State())
    private let source: any DispatchSourceRead
    private let noteContinuation: AsyncStream<Note>.Continuation

    public let notes: AsyncStream<Note>

    public static func connect(path: String) throws -> RPCConnection {
        RPCConnection(fd: try unixConnect(path: path))
    }

    public init(fd: Int32) {
        self.fd = fd
        queue = DispatchQueue(label: "dev.fevom.rpc.\(fd)")
        let (stream, cont) = AsyncStream<Note>.makeStream(bufferingPolicy: .unbounded)
        notes = stream
        noteContinuation = cont
        source = DispatchSource.makeReadSource(fileDescriptor: fd, queue: queue)
        source.setEventHandler { [weak self] in self?.readable() }
        source.setCancelHandler { [fd] in Darwin.close(fd) }
        source.resume()
    }

    deinit {
        source.cancel()
    }

    public func close() {
        queue.async { self.end() }
    }

    public var isClosed: Bool { state.withLock { $0.closed } }

    public func call(method: String, params: some Encodable & Sendable) async throws -> Data {
        let line: Data
        let id: UInt64 = try state.withLock { s in
            if s.closed { throw Failure.closed }
            defer { s.nextID += 1 }
            return s.nextID
        }
        line = try JSONEncoder.daemon.encode(Request(id: id, method: method, params: params)) + Data([0x0a])
        let reply = try await withCheckedThrowingContinuation { (k: CheckedContinuation<Data, any Error>) in
            let ok = state.withLock { s -> Bool in
                if s.closed { return false }
                s.pending[id] = k
                return true
            }
            guard ok else { return k.resume(throwing: Failure.closed) }
            queue.async { self.write(line) }
        }
        let head = try JSONDecoder().decode(ReplyHead.self, from: reply)
        if let error = head.error { throw error }
        return reply
    }

    public func notify(method: String, params: some Encodable & Sendable) throws {
        let line = try JSONEncoder.daemon.encode(Request<_>(id: nil, method: method, params: params)) + Data([0x0a])
        queue.async { self.write(line) }
    }

    private func readable() {
        var chunk = [UInt8](repeating: 0, count: 64 << 10)
        let n = chunk.withUnsafeMutableBytes { Darwin.read(fd, $0.baseAddress, $0.count) }
        if n <= 0 {
            if n < 0 && (errno == EAGAIN || errno == EINTR) { return }
            end()
            return
        }
        var lines: [Data] = []
        let tooLong = state.withLock { s -> Bool in
            s.buffer.append(contentsOf: chunk[0..<n])
            var start = s.buffer.startIndex
            while let nl = s.buffer[start...].firstIndex(of: 0x0a) {
                lines.append(s.buffer[start..<nl])
                start = s.buffer.index(after: nl)
            }
            if start != s.buffer.startIndex { s.buffer = Data(s.buffer[start...]) }
            return s.buffer.count > Self.maxMessage
        }
        for line in lines where !line.isEmpty {
            dispatch(line)
        }
        if tooLong { end() }
    }

    private func dispatch(_ line: Data) {
        guard let head = try? JSONDecoder().decode(MessageHead.self, from: line) else { return }
        switch (head.id, head.method) {
        case (let id?, nil):
            let k = state.withLock { $0.pending.removeValue(forKey: id) }
            k?.resume(returning: line)
        case (nil, let method?):
            noteContinuation.yield(Note(method: method, line: line))
        case (let id?, let method?):
            let err = #"{"jsonrpc":"2.0","id":\#(id),"error":{"code":-32601,"message":"method not found: \#(method)"}}"#
            write(Data(err.utf8) + Data([0x0a]))
        default:
            break
        }
    }

    private func write(_ data: Data) {
        data.withUnsafeBytes { buf in
            var off = 0
            while off < buf.count {
                let n = Darwin.write(fd, buf.baseAddress! + off, buf.count - off)
                if n < 0 {
                    if errno == EINTR { continue }
                    return
                }
                off += n
            }
        }
    }

    private func end() {
        let pending = state.withLock { s -> [CheckedContinuation<Data, any Error>] in
            if s.closed { return [] }
            s.closed = true
            defer { s.pending.removeAll() }
            return Array(s.pending.values)
        }
        for k in pending { k.resume(throwing: Failure.closed) }
        noteContinuation.finish()
        source.cancel()
    }
}

struct Request<P: Encodable>: Encodable {
    let id: UInt64?
    let method: String
    let params: P

    enum CodingKeys: String, CodingKey { case jsonrpc, id, method, params }

    func encode(to encoder: any Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode("2.0", forKey: .jsonrpc)
        try c.encodeIfPresent(id, forKey: .id)
        try c.encode(method, forKey: .method)
        try c.encode(params, forKey: .params)
    }
}

struct MessageHead: Decodable {
    let id: UInt64?
    let method: String?
}

struct ReplyHead: Decodable {
    let error: DaemonError?
}

struct Reply<R: Decodable>: Decodable {
    let result: R
}

struct NoteBody<P: Decodable>: Decodable {
    let params: P
}

extension RPCConnection.Note {
    public func decode<P>(_ notice: Notice<P>) throws -> P? {
        guard method == notice.method else { return nil }
        return try JSONDecoder().decode(NoteBody<P>.self, from: line).params
    }
}

func unixConnect(path: String) throws -> Int32 {
    var addr = sockaddr_un()
    addr.sun_family = sa_family_t(AF_UNIX)
    let bytes = Array(path.utf8)
    guard bytes.count < MemoryLayout.size(ofValue: addr.sun_path) else { throw RPCConnection.Failure.pathTooLong(path) }
    withUnsafeMutableBytes(of: &addr.sun_path) { dst in
        dst.copyBytes(from: bytes)
        dst[bytes.count] = 0
    }
    addr.sun_len = UInt8(MemoryLayout<sockaddr_un>.size)
    let fd = socket(AF_UNIX, SOCK_STREAM, 0)
    if fd < 0 { throw RPCConnection.Failure.connect(path, errno) }
    var one: Int32 = 1
    setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &one, socklen_t(MemoryLayout<Int32>.size))
    _ = fcntl(fd, F_SETFD, FD_CLOEXEC)
    let rc = withUnsafePointer(to: &addr) {
        $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
            Darwin.connect(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
        }
    }
    if rc != 0 {
        let e = errno
        Darwin.close(fd)
        throw RPCConnection.Failure.connect(path, e)
    }
    return fd
}
