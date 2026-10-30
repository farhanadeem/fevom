import FevomAPI
import Darwin
import Foundation

@MainActor
public final class DebugSession: TerminalStream {
    public let place: DaemonPlace
    public let target: String
    public let kind: String
    public let readOnly: Bool
    public let context: String
    public let argv: [String]
    public private(set) var started: DaemonAPI.DebugStarted?
    public private(set) var exit: DaemonAPI.SessionExit?

    public var onOutput: ([UInt8]) -> Void = { _ in }
    public var onExit: (String) -> Void = { _ in }
    public var onBanner: ([String]) -> Void = { _ in }

    private var client: DaemonClient?
    private var fd: Int32 = -1
    private var source: (any DispatchSourceRead)?
    private var notes: Task<Void, Never>?
    private var streamOver = false
    private var told = false

    public init(place: DaemonPlace, target: String, kind: String = "", readOnly: Bool = false, context: String = "", argv: [String] = []) {
        self.place = place
        self.target = target
        self.kind = kind
        self.readOnly = readOnly
        self.context = context
        self.argv = argv
    }

    public func start(rows: Int, cols: Int) async throws {
        let c = try await DaemonClient.connect(home: place.home, build: place.build)
        client = c
        do {
            let p = DaemonAPI.DebugStartParams(target: target, context: context, kind: kind, readOnly: readOnly, argv: argv,
                                               env: ["TERM=xterm-256color", "COLORTERM=truecolor"],
                                               tty: true, rows: rows, cols: cols)
            let s = try await c.call(DaemonAPI.Methods.debugStart, p)
            started = s
            feedBanner(s)
            notes = Task { [weak self] in
                for await note in c.connection.notes {
                    if let e = try? note.decode(DaemonAPI.Methods.sessionExit), e.id == s.id {
                        self?.ended(e)
                    }
                }
                self?.connectionEnded()
            }
            follow(try await SessionAttach.attach(home: place.home, token: s.token))
        } catch {
            close()
            throw error
        }
    }

    private func feedBanner(_ s: DaemonAPI.DebugStarted) {
        let lines = Self.banner(from: s)
        onBanner(lines)
        if !lines.isEmpty {
            onOutput(Array(lines.map { $0 + "\r\n" }.joined().utf8))
        }
    }

    static func banner(from s: DaemonAPI.DebugStarted) -> [String] {
        var lines: [String] = []
        if s.kind == "remote" {
            lines.append("fev debug: \(s.name) — remote context")
            lines.append(contentsOf: s.notes)
            return lines
        }
        let where_ = s.image.isEmpty ? s.name : "\(s.name) (\(s.image))"
        let state = s.kind == "running" ? "running" : s.kind == "stopped" ? "stopped" : "image"
        let ro = s.readOnly && s.kind != "image" ? ", read-only" : ""
        lines.append("fev debug: \(where_), \(state)\(ro); tools in /nix")
        lines.append(contentsOf: s.notes)
        return lines
    }

    func follow(_ stream: Int32) {
        fd = stream
        _ = fcntl(fd, F_SETFL, fcntl(fd, F_GETFL) | O_NONBLOCK)
        let src = DispatchSource.makeReadSource(fileDescriptor: fd, queue: .main)
        let fd = self.fd
        src.setEventHandler { [weak self] in MainActor.assumeIsolated { self?.readable() } }
        src.setCancelHandler { Darwin.close(fd) }
        source = src
        src.resume()
    }

    private func readable() {
        var buf = [UInt8](repeating: 0, count: 64 << 10)
        let n = read(fd, &buf, buf.count)
        if n > 0 {
            onOutput(Array(buf[0..<n]))
        } else if n == 0 || (errno != EAGAIN && errno != EINTR) {
            source?.cancel()
            source = nil
            fd = -1
            streamOver = true
            if let e = exit { tell(Self.why(e)) }
        }
    }

    func ended(_ e: DaemonAPI.SessionExit) {
        exit = e
        if streamOver || !e.streamEnded { tell(Self.why(e)) }
    }

    func connectionEnded() {
        if let e = exit {
            tell(Self.why(e))
        } else {
            tell(streamOver ? "the session's stream ended" : "the connection to Fevom ended")
        }
    }

    private func tell(_ why: String) {
        guard !told else { return }
        told = true
        onExit(why)
    }

    static func why(_ e: DaemonAPI.SessionExit) -> String {
        if !e.note.isEmpty {
            return e.note.replacingOccurrences(of: "\n", with: "; ") + "; " + whyEnded(e)
        }
        return whyEnded(e)
    }

    static func whyEnded(_ e: DaemonAPI.SessionExit) -> String {
        if e.reason == DaemonAPI.Const.debugReasonToolsUnavailable {
            return "the debug tools could not be fetched; run `fev debug -f` to open the target's own shell instead"
        }
        if e.reason == DaemonAPI.Const.debugReasonTargetGone {
            return "the target stopped or was removed"
        }
        return !e.error.isEmpty ? e.error : e.signal != 0 ? "ended by signal \(e.signal)" : "exited with status \(e.exitCode)"
    }

    public func send(_ bytes: [UInt8]) {
        guard fd >= 0, source != nil else { return }
        var off = 0
        while off < bytes.count {
            let n = bytes[off...].withUnsafeBytes { write(fd, $0.baseAddress, $0.count) }
            if n < 0 {
                if errno == EINTR { continue }
                if errno == EAGAIN {
                    var p = pollfd(fd: fd, events: Int16(POLLOUT), revents: 0)
                    _ = poll(&p, 1, 1000)
                    continue
                }
                return
            }
            off += n
        }
    }

    public func resize(rows: Int, cols: Int) {
        guard let c = client, let s = started else { return }
        Task { _ = try? await c.call(DaemonAPI.Methods.sessionResize, .init(id: s.id, rows: rows, cols: cols)) }
    }

    public func signal(_ number: Int) {
        guard let c = client, let s = started else { return }
        Task { _ = try? await c.call(DaemonAPI.Methods.sessionSignal, .init(id: s.id, signal: number)) }
    }

    public func close() {
        told = true
        if let s = source {
            s.cancel()
        } else if fd >= 0 {
            Darwin.close(fd)
        }
        source = nil
        fd = -1
        notes?.cancel()
        notes = nil
        if let c = client, let s = started, exit == nil {
            Task { _ = try? await c.call(DaemonAPI.Methods.sessionClose, .init(id: s.id)); c.close() }
        } else {
            client?.close()
        }
        client = nil
    }
}
