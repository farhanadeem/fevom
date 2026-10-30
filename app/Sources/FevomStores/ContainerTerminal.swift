import FevomAPI
import FevomDocker
import Darwin
import Foundation

@MainActor
public protocol TerminalStream: AnyObject {
    var onOutput: ([UInt8]) -> Void { get set }
    var onExit: (String) -> Void { get set }
    func start(rows: Int, cols: Int) async throws
    func send(_ bytes: [UInt8])
    func resize(rows: Int, cols: Int)
    func close()
}

extension TerminalSession: TerminalStream {
    public func start(rows: Int, cols: Int) async throws {
        try await start(rows: rows, cols: cols, argv: [])
    }
}

@MainActor
public final class ContainerTerminal: TerminalStream {
    public let place: DaemonPlace
    public let container: String
    public let user: String
    public private(set) var execID: String?
    public private(set) var exitStatus: Int?

    public var onOutput: ([UInt8]) -> Void = { _ in }
    public var onExit: (String) -> Void = { _ in }

    public static let shell = ["/bin/sh", "-c", "if command -v bash >/dev/null 2>&1; then exec bash; else exec sh; fi"]

    private var fd: Int32 = -1
    private var source: (any DispatchSourceRead)?
    private var told = false
    private var closed = false
    var docker: DockerClient { DockerClient(socketPath: DaemonAPI.dockerSocketPath(home: place.home)) }

    public init(place: DaemonPlace, container: String, user: String = "") {
        self.place = place
        self.container = container
        self.user = user
    }

    public func start(rows: Int, cols: Int) async throws {
        let id = try await docker.createExec(container: container, cmd: Self.shell, env: ["TERM=xterm-256color", "COLORTERM=truecolor"],
                                             user: user, rows: rows, cols: cols)
        execID = id
        let socketPath = DaemonAPI.dockerSocketPath(home: place.home)
        let sock = try SessionAttach.spawning { try DockerClient.openStream(socketPath) }
        let first = try await docker.startExec(id, on: sock)
        if closed {
            Darwin.close(sock)
            return
        }
        if !first.isEmpty { onOutput(first) }
        follow(sock)
    }

    private func follow(_ stream: Int32) {
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
            Task { await self.ended() }
        }
    }

    func ended() async {
        guard let id = execID, !told else { return }
        for attempt in 0..<10 {
            if closed { return }
            do {
                let st = try await docker.inspectExec(id)
                if !st.Running, let code = st.ExitCode {
                    exitStatus = code
                    tell(code == 137 ? "killed (status 137): the container stopped, or the process was killed" : "exited with status \(code)")
                    return
                }
            } catch {
                tell("the terminal's stream ended; its status could not be read: \(error)")
                return
            }
            if attempt < 9 { try? await Task.sleep(for: .milliseconds(100)) }
        }
        tell("the terminal's stream ended; the engine has not recorded an exit")
    }

    private func tell(_ why: String) {
        guard !told, !closed else { return }
        told = true
        onExit(why)
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
        guard let id = execID, !closed else { return }
        let d = docker
        Task { _ = try? await d.resizeExec(id, rows: rows, cols: cols) }
    }

    public func close() {
        closed = true
        if let s = source {
            s.cancel()
        } else if fd >= 0 {
            Darwin.close(fd)
        }
        source = nil
        fd = -1
    }
}
