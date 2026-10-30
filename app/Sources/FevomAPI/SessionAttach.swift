import Darwin
import Foundation

public enum SessionAttach {
    public enum Failure: Error, Equatable, CustomStringConvertible {
        case connect(Int32)
        case refused(code: String, text: String)
        case malformed(String)

        public var description: String {
            switch self {
            case .connect(let e): "the session socket: \(String(cString: strerror(e)))"
            case .refused(let code, let text): "the daemon refused the session's stream: \(code) \(text)"
            case .malformed(let what): "the session socket: \(what)"
            }
        }
    }

    private static let lock = NSLock()

    public static func spawning<T>(_ body: () throws -> T) rethrows -> T {
        lock.lock()
        defer { lock.unlock() }
        return try body()
    }

    public static func attachSync(home: String, token: String) throws -> Int32 {
        guard token.count == 32, token.allSatisfy({ $0.isHexDigit && !$0.isUppercase }) else {
            throw Failure.malformed("a token is 32 lower-case hex digits")
        }
        let sock: Int32
        do {
            sock = try unixConnect(path: DaemonAPI.sessionSocketPath(home: home))
        } catch RPCConnection.Failure.connect(_, let e) {
            throw Failure.connect(e)
        }
        defer { close(sock) }
        let req = Array("exec \(token)\n".utf8)
        guard req.withUnsafeBytes({ write(sock, $0.baseAddress, $0.count) }) == req.count else {
            throw Failure.connect(errno)
        }
        var line: [UInt8] = []
        var passed: Int32 = -1
        func closePassed() {
            if passed >= 0 { close(passed); passed = -1 }
        }
        let controlLen = MemoryLayout<cmsghdr>.size + MemoryLayout<Int32>.size
        let controlSpace = (controlLen + 7) & ~7
        while true {
            var buf = [UInt8](repeating: 0, count: 512)
            var control = [UInt8](repeating: 0, count: controlSpace)
            var flags: Int32 = 0
            var p = pollfd(fd: sock, events: Int16(POLLIN), revents: 0)
            let ready = poll(&p, 1, 30_000)
            if ready == 0 { closePassed(); throw Failure.malformed("no reply within 30 s") }
            if ready < 0 {
                if errno == EINTR { continue }
                let e = errno
                closePassed()
                throw Failure.connect(e)
            }
            let n: Int = lock.withLock {
                let n = buf.withUnsafeMutableBytes { b in
                    control.withUnsafeMutableBytes { c in
                        var iov = iovec(iov_base: b.baseAddress, iov_len: b.count)
                        return withUnsafeMutablePointer(to: &iov) { iovp in
                            var msg = msghdr(msg_name: nil, msg_namelen: 0, msg_iov: iovp, msg_iovlen: 1,
                                             msg_control: c.baseAddress, msg_controllen: socklen_t(c.count), msg_flags: 0)
                            let n = recvmsg(sock, &msg, MSG_DONTWAIT)
                            flags = msg.msg_flags
                            if n >= 0, msg.msg_controllen >= socklen_t(MemoryLayout<cmsghdr>.size) {
                                let h = c.baseAddress!.assumingMemoryBound(to: cmsghdr.self).pointee
                                if h.cmsg_level == SOL_SOCKET, h.cmsg_type == SCM_RIGHTS {
                                    let count = (Int(h.cmsg_len) - MemoryLayout<cmsghdr>.size) / MemoryLayout<Int32>.size
                                    let fds = (c.baseAddress! + MemoryLayout<cmsghdr>.size).assumingMemoryBound(to: Int32.self)
                                    for i in 0..<max(0, count) {
                                        if passed < 0 { passed = fds[i] } else { close(fds[i]) }
                                    }
                                    if passed >= 0 { _ = fcntl(passed, F_SETFD, FD_CLOEXEC) }
                                }
                            }
                            return n
                        }
                    }
                }
                return n
            }
            if flags & MSG_CTRUNC != 0 {
                closePassed()
                throw Failure.malformed("the reply carried more than one descriptor")
            }
            if n < 0 {
                if errno == EINTR || errno == EAGAIN { continue }
                let e = errno
                closePassed()
                throw Failure.connect(e)
            }
            line += buf[0..<n]
            if let nl = line.firstIndex(of: 0x0a) {
                let text = String(decoding: line[..<nl], as: UTF8.self)
                if text != "ok" {
                    closePassed()
                    guard text.hasPrefix("err ") else { throw Failure.malformed("a reply that is neither ok nor err") }
                    let rest = text.dropFirst(4)
                    let parts = rest.split(separator: " ", maxSplits: 1)
                    throw Failure.refused(code: parts.first.map(String.init) ?? "", text: parts.count > 1 ? String(parts[1]) : "")
                }
                guard passed >= 0 else { throw Failure.malformed("the daemon answered ok without the session's stream") }
                guard nl == line.count - 1 else {
                    closePassed()
                    throw Failure.malformed("the daemon sent bytes after its reply")
                }
                return passed
            }
            if n == 0 {
                closePassed()
                throw Failure.malformed("the daemon closed the session socket before its reply")
            }
        }
    }

    public static func attach(home: String, token: String) async throws -> Int32 {
        try await withCheckedThrowingContinuation { k in
            DispatchQueue.global(qos: .userInitiated).async {
                k.resume(with: Result { try attachSync(home: home, token: token) })
            }
        }
    }
}
