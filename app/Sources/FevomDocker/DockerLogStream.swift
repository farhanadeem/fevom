import Darwin
import Foundation

public final class DockerLogStream: @unchecked Sendable {
    public struct Line: Sendable {
        public var stream: UInt8
        public var bytes: [UInt8]
        public var ended = true
    }

    public struct Options: Sendable {
        public var follow = true
        public var tail: Int?
        public var timestamps = false
        public var since: String?
        public var maxLine = 1 << 20
        public init(follow: Bool = true, tail: Int? = nil, timestamps: Bool = false, since: String? = nil) {
            self.follow = follow
            self.tail = tail
            self.timestamps = timestamps
            self.since = since
        }
    }

    private let fd: Int32
    private let queue: DispatchQueue
    private let source: any DispatchSourceRead
    private let onLines: @Sendable ([Line]) -> Void
    private let onEnd: @Sendable (String?) -> Void
    private let maxLine: Int
    private let timestamps: Bool

    static let logPart = 16 << 10

    private var head = [UInt8]()
    private var inBody = false
    private var chunked = false
    private var raw = false
    private var status = 0
    private var chunkLeft = 0
    private var chunkState = ChunkState.size
    private var sizeLine = [UInt8]()
    private var frame = [UInt8]()
    private var frameLeft = 0
    private var frameStream: UInt8 = 1
    private var partial: [UInt8: [UInt8]] = [:]
    private var timed: [UInt8: Timed] = [:]

    struct Timed {
        var inLead = true
        var lead: [UInt8] = []
        var continues = false
        var lineTS: [UInt8] = []
        var count = 0
        var open = false
    }
    private var errorBody = [UInt8]()
    private var ended = false
    private(set) var sawEnd = false

    enum ChunkState { case size, data, crlf }

    public init(socketPath: String, container: String, apiVersion: String = "1.44", options: Options = .init(),
                onLines: @escaping @Sendable ([Line]) -> Void, onEnd: @escaping @Sendable (String?) -> Void) throws {
        self.onLines = onLines
        self.onEnd = onEnd
        maxLine = options.maxLine
        timestamps = options.timestamps
        fd = try connectUnix(socketPath)
        var q = "stdout=1&stderr=1"
        if options.follow { q += "&follow=1" }
        if let t = options.tail { q += "&tail=\(t)" }
        if options.timestamps { q += "&timestamps=1" }
        if let s = options.since { q += "&since=\(s)" }
        let req = "GET /v\(apiVersion)/containers/\(container)/logs?\(q) HTTP/1.1\r\nHost: docker\r\nUser-Agent: fev-app\r\nConnection: close\r\n\r\n"
        try writeAll(fd, Data(req.utf8))
        queue = DispatchQueue(label: "dev.fevom.logs.\(container.prefix(12))")
        source = DispatchSource.makeReadSource(fileDescriptor: fd, queue: queue)
        source.setEventHandler { [weak self] in self?.readable() }
        source.setCancelHandler { [fd] in close(fd) }
        source.resume()
    }

    public func cancel() {
        queue.async {
            self.ended = true
            self.source.cancel()
        }
    }

    deinit { source.cancel() }

    private func readable() {
        guard !ended else { return }
        var buf = [UInt8](repeating: 0, count: 64 << 10)
        let n = read(fd, &buf, buf.count)
        if n < 0 && (errno == EAGAIN || errno == EINTR) { return }
        if n <= 0 {
            finish(n < 0 ? "reading the log: \(String(cString: strerror(errno)))" : nil)
            return
        }
        var lines: [Line] = []
        consume(buf[0..<n], &lines)
        if !lines.isEmpty { onLines(lines) }
        if sawEnd { finish(nil) }
    }

    private func finish(_ why: String?) {
        guard !ended else { return }
        ended = true
        let lines = rest()
        if !lines.isEmpty { onLines(lines) }
        var reason = why
        if status != 0 && !(200..<300).contains(status) {
            let text = String(decoding: errorBody, as: UTF8.self)
            struct M: Decodable { let message: String }
            reason = "Docker API \(status): " + ((try? JSONDecoder().decode(M.self, from: Data(errorBody)))?.message ?? text)
        }
        source.cancel()
        onEnd(reason)
    }

    private func rest() -> [Line] {
        var lines: [Line] = []
        for s in partial.keys.sorted() where !(partial[s] ?? []).isEmpty { lines.append(Line(stream: s, bytes: partial[s]!, ended: false)) }
        partial = [:]
        return lines
    }

    func consume(_ bytes: ArraySlice<UInt8>, _ lines: inout [Line]) {
        var rest = bytes[...]
        if !inBody {
            head += rest
            guard let end = findHeaderEnd(head) else { return }
            let text = String(decoding: head[0..<end], as: UTF8.self).lowercased()
            let first = text.split(separator: "\r\n").first ?? ""
            status = Int(first.split(separator: " ").dropFirst().first ?? "") ?? 0
            chunked = text.contains("transfer-encoding: chunked")
            raw = text.contains("application/vnd.docker.raw-stream")
            inBody = true
            rest = ArraySlice(Array(head[(end + 4)...]))
            head = []
        }
        if chunked { dechunk(rest, &lines) } else { body(rest, &lines) }
    }

    private func findHeaderEnd(_ b: [UInt8]) -> Int? {
        guard b.count >= 4 else { return nil }
        for i in 0...(b.count - 4) where b[i] == 13 && b[i + 1] == 10 && b[i + 2] == 13 && b[i + 3] == 10 { return i }
        return nil
    }

    private func dechunk(_ bytes: ArraySlice<UInt8>, _ lines: inout [Line]) {
        var i = bytes.startIndex
        while i < bytes.endIndex {
            switch chunkState {
            case .size:
                let b = bytes[i]
                i += 1
                if b == 10 {
                    let t = String(decoding: sizeLine, as: UTF8.self).split(separator: ";").first ?? ""
                    chunkLeft = Int(t.trimmingCharacters(in: .whitespacesAndNewlines), radix: 16) ?? 0
                    sizeLine = []
                    if chunkLeft == 0 { sawEnd = true; return }
                    chunkState = .data
                } else {
                    sizeLine.append(b)
                }
            case .data:
                let n = min(chunkLeft, bytes.endIndex - i)
                body(bytes[i..<(i + n)], &lines)
                i += n
                chunkLeft -= n
                if chunkLeft == 0 { chunkState = .crlf }
            case .crlf:
                if bytes[i] == 10 { chunkState = .size }
                i += 1
            }
        }
    }

    private func body(_ bytes: ArraySlice<UInt8>, _ lines: inout [Line]) {
        if !(200..<300).contains(status) {
            errorBody += bytes
            return
        }
        if raw {
            if timestamps { splitTimed(1, bytes, &lines) } else { split(1, bytes, &lines) }
            return
        }
        var i = bytes.startIndex
        while i < bytes.endIndex {
            if frameLeft == 0 {
                let need = 8 - frame.count
                let n = min(need, bytes.endIndex - i)
                frame += bytes[i..<(i + n)]
                i += n
                if frame.count == 8 {
                    frameStream = frame[0] == 2 ? 2 : 1
                    frameLeft = Int(frame[4]) << 24 | Int(frame[5]) << 16 | Int(frame[6]) << 8 | Int(frame[7])
                    frame = []
                    if timestamps { messageBegins(frameStream) }
                }
                continue
            }
            let n = min(frameLeft, bytes.endIndex - i)
            if timestamps { splitTimed(frameStream, bytes[i..<(i + n)], &lines) } else { split(frameStream, bytes[i..<(i + n)], &lines) }
            i += n
            frameLeft -= n
        }
    }

    private func split(_ stream: UInt8, _ bytes: ArraySlice<UInt8>, _ lines: inout [Line]) {
        var start = bytes.startIndex
        var p = partial[stream] ?? []
        while let nl = bytes[start...].firstIndex(of: 10) {
            p += bytes[start..<nl]
            while p.count > maxLine {
                lines.append(Line(stream: stream, bytes: Array(p[0..<maxLine]), ended: false))
                p.removeFirst(maxLine)
            }
            lines.append(Line(stream: stream, bytes: p))
            p = []
            start = nl + 1
        }
        p += bytes[start...]
        while p.count >= maxLine {
            lines.append(Line(stream: stream, bytes: Array(p[0..<maxLine]), ended: false))
            p.removeFirst(maxLine)
        }
        partial[stream] = p
    }

    private func messageBegins(_ stream: UInt8) {
        var t = timed[stream] ?? Timed()
        t.inLead = true
        t.lead = []
        t.continues = t.open
        t.count = 0
        timed[stream] = t
    }

    private func splitTimed(_ stream: UInt8, _ bytes: ArraySlice<UInt8>, _ lines: inout [Line]) {
        var t = timed[stream] ?? Timed()
        var p = partial[stream] ?? []
        var i = bytes.startIndex
        func give(_ piece: [UInt8], ended: Bool = false) { lines.append(Line(stream: stream, bytes: piece, ended: ended)) }
        func lead() -> [UInt8] { t.lineTS.isEmpty ? [] : t.lineTS + [0x20] }
        while i < bytes.endIndex {
            if t.inLead {
                let b = bytes[i]
                if b == 0x20, t.lead.count >= 20 {
                    if t.continues && t.lead == t.lineTS {
                    } else if t.continues {
                        p += t.lead + [0x20]
                    } else {
                        t.lineTS = t.lead
                        p += t.lead + [0x20]
                    }
                    i += 1
                } else if t.lead.count < 40, (b >= 0x30 && b <= 0x3A) || b == 0x2D || b == 0x2E || b == 0x2B || b == 0x54 || b == 0x5A {
                    t.lead.append(b)
                    i += 1
                    continue
                } else {
                    if !t.continues { t.lineTS = [] }
                    p += t.lead
                    t.count = t.lead.count
                }
                t.inLead = false
                t.lead = []
                t.open = true
                continue
            }
            var end = bytes.endIndex
            if raw { end = min(end, i + max(0, Self.logPart - t.count)) }
            let nl = bytes[i..<end].firstIndex(of: 10)
            let stop = nl ?? end
            p += bytes[i..<stop]
            t.count += stop - i
            t.open = true
            while p.count > maxLine || (nl == nil && p.count == maxLine) {
                give(Array(p[0..<maxLine]))
                let l = lead()
                p = (l.count * 2 < maxLine ? l : []) + p[maxLine...]
            }
            if let nl {
                give(p, ended: true)
                p = []
                t.open = false
                t.continues = false
                t.lineTS = []
                t.inLead = true
                t.lead = []
                t.count = 0
                i = nl + 1
            } else {
                i = stop
                if raw && t.count >= Self.logPart {
                    t.inLead = true
                    t.lead = []
                    t.continues = true
                    t.count = 0
                }
            }
        }
        partial[stream] = p
        timed[stream] = t
    }

    static func parser(maxLine: Int = 1 << 20, timestamps: Bool = false) -> Parser { Parser(maxLine: maxLine, timestamps: timestamps) }

    final class Parser {
        let s: DockerLogStream
        init(maxLine: Int, timestamps: Bool) {
            var fds: [Int32] = [0, 0]
            socketpair(AF_UNIX, SOCK_STREAM, 0, &fds)
            close(fds[1])
            s = DockerLogStream(fd: fds[0], maxLine: maxLine, timestamps: timestamps)
        }
        func feed(_ bytes: [UInt8]) -> [Line] {
            var out: [Line] = []
            s.consume(bytes[...], &out)
            return out
        }
        func end() -> [Line] { s.rest() }
        var status: Int { s.status }
    }

    private init(fd: Int32, maxLine: Int, timestamps: Bool) {
        self.fd = fd
        self.maxLine = maxLine
        self.timestamps = timestamps
        onLines = { _ in }
        onEnd = { _ in }
        queue = DispatchQueue(label: "dev.fevom.logs.test")
        source = DispatchSource.makeReadSource(fileDescriptor: fd, queue: queue)
        source.setCancelHandler { [fd] in close(fd) }
        source.resume()
    }
}
