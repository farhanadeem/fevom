import FevomAPI
import Darwin
import Foundation

@MainActor
public final class TerminalSession {
    public let place: DaemonPlace
    public let machine: String
    public let user: String
    public private(set) var started: DaemonAPI.SessionStarted?
    public private(set) var exit: DaemonAPI.SessionExit?

    public var onOutput: ([UInt8]) -> Void = { _ in }
    public var onExit: (String) -> Void = { _ in }

    private var client: DaemonClient?
    private var fd: Int32 = -1
    private var source: (any DispatchSourceRead)?
    private var notes: Task<Void, Never>?
    private var streamOver = false
    private var told = false

    private var opener: ((DaemonClient, Int, Int, [String]) async throws -> DaemonAPI.SessionStarted)?

    public init(place: DaemonPlace, machine: String, user: String = "") {
        self.place = place
        self.machine = machine
        self.user = user
    }

    public init(place: DaemonPlace, k8s exec: DaemonAPI.K8sExecParams) {
        self.place = place
        machine = exec.pod
        user = exec.container
        opener = { c, rows, cols, argv in
            var p = exec
            p.tty = true
            p.rows = rows
            p.cols = cols
            if !argv.isEmpty { p.argv = argv }
            let s = try await c.call(DaemonAPI.Methods.k8sExec, p)
            return DaemonAPI.SessionStarted(id: s.id, token: s.token, machine: s.pod, user: s.container)
        }
    }

    public func start(rows: Int, cols: Int, argv: [String] = []) async throws {
        let c = try await DaemonClient.connect(home: place.home, build: place.build)
        client = c
        do {
            let s: DaemonAPI.SessionStarted
            if let opener {
                s = try await opener(c, rows, cols, argv)
            } else {
                let p = DaemonAPI.SessionStartParams(machine: machine, user: user, argv: argv, env: ["TERM=xterm-256color", "COLORTERM=truecolor"],
                                                     tty: true, rows: rows, cols: cols, login: argv.isEmpty)
                s = try await c.call(DaemonAPI.Methods.sessionStart, p)
            }
            started = s
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

    func follow(_ stream: Int32) {
        fd = stream
        let flags = fcntl(fd, F_GETFL)
        _ = fcntl(fd, F_SETFL, flags | O_NONBLOCK)
        let src = DispatchSource.makeReadSource(fileDescriptor: fd, queue: .main)
        let fd = self.fd
        src.setEventHandler { [weak self] in
            MainActor.assumeIsolated { self?.readable() }
        }
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
        !e.error.isEmpty ? e.error : e.signal != 0 ? "ended by signal \(e.signal)" : "exited with status \(e.exitCode)"
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
