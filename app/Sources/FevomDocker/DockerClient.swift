import Foundation

public struct DockerClient: Sendable {
    public let socketPath: String
    public var apiVersion = "1.44"

    public init(socketPath: String) { self.socketPath = socketPath }

    public struct Failure: Error, Sendable, CustomStringConvertible {
        public let status: Int
        public let message: String
        public var description: String { status == 0 ? message : "Docker API \(status): \(message)" }
    }

    public func containers(all: Bool = true) async throws -> [Container] {
        let data = try await request("GET", "/containers/json" + (all ? "?all=1" : ""))
        return try JSONDecoder().decode([Container].self, from: data)
    }

    public func containerAction(_ id: String, _ action: String) async throws {
        _ = try await request("POST", "/containers/\(id)/\(action)")
    }

    public func removeContainer(_ id: String, force: Bool) async throws {
        _ = try await request("DELETE", "/containers/\(id)" + (force ? "?force=1" : ""))
    }

    public func inspect(_ id: String) async throws -> ContainerDetail {
        try JSONDecoder().decode(ContainerDetail.self, from: try await request("GET", "/containers/\(id)/json"))
    }

    public func images() async throws -> [DockerImage] {
        try JSONDecoder().decode([DockerImage].self, from: try await request("GET", "/images/json"))
    }

    public func removeImage(_ ref: String, force: Bool) async throws {
        _ = try await request("DELETE", "/images/\(ref)" + (force ? "?force=1" : ""))
    }

    public func pruneImages() async throws -> String {
        struct Pruned: Decodable { let SpaceReclaimed: Int64? }
        let r = try JSONDecoder().decode(Pruned.self, from: try await request("POST", "/images/prune"))
        return ByteCountFormatter.string(fromByteCount: r.SpaceReclaimed ?? 0, countStyle: .file) + " reclaimed"
    }

    public func volumes() async throws -> [DockerVolume] {
        struct List: Decodable { let Volumes: [DockerVolume]? }
        return try JSONDecoder().decode(List.self, from: try await request("GET", "/volumes")).Volumes ?? []
    }

    public func createVolume(_ name: String) async throws {
        _ = try await request("POST", "/volumes/create", body: try JSONEncoder().encode(["Name": name]))
    }

    public func removeVolume(_ name: String) async throws {
        _ = try await request("DELETE", "/volumes/\(name)")
    }

    public func request(_ method: String, _ path: String, body: Data? = nil) async throws -> Data {
        let socketPath = self.socketPath
        let fullPath = "/v\(apiVersion)" + path
        return try await withCheckedThrowingContinuation { k in
            DispatchQueue.global(qos: .userInitiated).async {
                k.resume(with: Result { try Self.roundTrip(socketPath: socketPath, method: method, path: fullPath, body: body) })
            }
        }
    }

    static func roundTrip(socketPath: String, method: String, path: String, body: Data?) throws -> Data {
        let fd = try connectUnix(socketPath)
        defer { close(fd) }
        var head = "\(method) \(path) HTTP/1.1\r\nHost: docker\r\nConnection: close\r\nUser-Agent: fev-app\r\n"
        if let body {
            head += "Content-Type: application/json\r\nContent-Length: \(body.count)\r\n"
        } else if method == "POST" || method == "DELETE" {
            head += "Content-Length: 0\r\n"
        }
        head += "\r\n"
        try writeAll(fd, Data(head.utf8) + (body ?? Data()))
        var response = Data()
        var buf = [UInt8](repeating: 0, count: 64 << 10)
        while true {
            var p = pollfd(fd: fd, events: Int16(POLLIN), revents: 0)
            if poll(&p, 1, -1) < 0 {
                if errno == EINTR { continue }
                throw Failure(status: 0, message: "waiting on \(socketPath): \(String(cString: strerror(errno)))")
            }
            let n = read(fd, &buf, buf.count)
            if n < 0 {
                if errno == EINTR { continue }
                throw Failure(status: 0, message: "reading \(socketPath): \(String(cString: strerror(errno)))")
            }
            if n == 0 { break }
            response.append(contentsOf: buf[0..<n])
        }
        return try parse(response)
    }

    static func parse(_ response: Data) throws -> Data {
        guard let sep = response.firstRange(of: Data("\r\n\r\n".utf8)) else {
            throw Failure(status: 0, message: "no HTTP response")
        }
        let headText = String(decoding: response[..<sep.lowerBound], as: UTF8.self)
        var body = Data(response[sep.upperBound...])
        let lines = headText.components(separatedBy: "\r\n")
        let status = Int(lines.first?.split(separator: " ").dropFirst().first ?? "") ?? 0
        let chunked = lines.dropFirst().contains { $0.lowercased().hasPrefix("transfer-encoding:") && $0.lowercased().contains("chunked") }
        if chunked { body = try dechunk(body) }
        guard (200..<300).contains(status) else {
            struct Message: Decodable { let message: String }
            let message = (try? JSONDecoder().decode(Message.self, from: body).message) ?? String(decoding: body, as: UTF8.self)
            throw Failure(status: status, message: message)
        }
        return body
    }

    static func dechunk(_ data: Data) throws -> Data {
        var out = Data()
        var i = data.startIndex
        while i < data.endIndex {
            guard let crlf = data[i...].firstRange(of: Data("\r\n".utf8)) else { break }
            let sizeText = String(decoding: data[i..<crlf.lowerBound], as: UTF8.self).split(separator: ";").first ?? ""
            guard let size = Int(sizeText.trimmingCharacters(in: .whitespaces), radix: 16) else {
                throw Failure(status: 0, message: "bad chunk size")
            }
            if size == 0 { break }
            let start = crlf.upperBound
            guard data.distance(from: start, to: data.endIndex) >= size else { throw Failure(status: 0, message: "short chunk") }
            let end = data.index(start, offsetBy: size)
            out.append(data[start..<end])
            i = data.index(end, offsetBy: 2, limitedBy: data.endIndex) ?? data.endIndex
        }
        return out
    }
}

func connectUnix(_ path: String) throws -> Int32 {
    var addr = sockaddr_un()
    addr.sun_family = sa_family_t(AF_UNIX)
    let bytes = Array(path.utf8)
    guard bytes.count < MemoryLayout.size(ofValue: addr.sun_path) else {
        throw DockerClient.Failure(status: 0, message: "socket path too long: \(path)")
    }
    withUnsafeMutableBytes(of: &addr.sun_path) { dst in
        dst.copyBytes(from: bytes)
        dst[bytes.count] = 0
    }
    let fd = socket(AF_UNIX, SOCK_STREAM, 0)
    guard fd >= 0 else { throw DockerClient.Failure(status: 0, message: "socket: \(String(cString: strerror(errno)))") }
    var one: Int32 = 1
    setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &one, socklen_t(MemoryLayout<Int32>.size))
    let rc = withUnsafePointer(to: &addr) {
        $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { connect(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) }
    }
    if rc != 0 {
        let e = errno
        close(fd)
        throw DockerClient.Failure(status: 0, message: "cannot connect to \(path): \(String(cString: strerror(e)))")
    }
    return fd
}

func writeAll(_ fd: Int32, _ data: Data) throws {
    try data.withUnsafeBytes { p in
        var off = 0
        while off < p.count {
            let n = write(fd, p.baseAddress! + off, p.count - off)
            if n < 0 {
                if errno == EINTR { continue }
                throw DockerClient.Failure(status: 0, message: "write: \(String(cString: strerror(errno)))")
            }
            off += n
        }
    }
}

public struct Container: Codable, Sendable, Hashable, Identifiable {
    public var id: String
    public var names: [String]
    public var image: String
    public var imageID: String
    public var state: String
    public var status: String
    public var created: Int64
    public var ports: [Port]
    public var labels: [String: String]
    public var volumeNames: [String]

    public struct Port: Codable, Sendable, Hashable {
        public var ip: String?
        public var privatePort: Int
        public var publicPort: Int?
        public var type: String
        enum CodingKeys: String, CodingKey {
            case ip = "IP", privatePort = "PrivatePort", publicPort = "PublicPort", type = "Type"
        }
    }

    enum CodingKeys: String, CodingKey {
        case id = "Id", names = "Names", image = "Image", imageID = "ImageID", state = "State", status = "Status"
        case created = "Created", ports = "Ports", labels = "Labels", mounts = "Mounts"
    }

    struct Mount: Codable { let `Type`: String?; let Name: String? }

    public func encode(to encoder: any Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(id, forKey: .id)
        try c.encode(names, forKey: .names)
        try c.encode(image, forKey: .image)
        try c.encode(imageID, forKey: .imageID)
        try c.encode(state, forKey: .state)
        try c.encode(status, forKey: .status)
        try c.encode(created, forKey: .created)
        try c.encode(ports, forKey: .ports)
        try c.encode(labels, forKey: .labels)
        try c.encode(volumeNames.map { Mount(Type: "volume", Name: $0) }, forKey: .mounts)
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        names = try c.decodeIfPresent([String].self, forKey: .names) ?? []
        image = try c.decodeIfPresent(String.self, forKey: .image) ?? ""
        imageID = try c.decodeIfPresent(String.self, forKey: .imageID) ?? ""
        state = try c.decodeIfPresent(String.self, forKey: .state) ?? ""
        status = try c.decodeIfPresent(String.self, forKey: .status) ?? ""
        created = try c.decodeIfPresent(Int64.self, forKey: .created) ?? 0
        ports = try c.decodeIfPresent([Port].self, forKey: .ports) ?? []
        labels = try c.decodeIfPresent([String: String].self, forKey: .labels) ?? [:]
        volumeNames = (try c.decodeIfPresent([Mount].self, forKey: .mounts) ?? []).compactMap { $0.Type == "volume" ? $0.Name : nil }
    }

    public init(id: String, names: [String], image: String, imageID: String = "", state: String, status: String = "", created: Int64 = 0, ports: [Port] = [], labels: [String: String] = [:], volumeNames: [String] = []) {
        self.volumeNames = volumeNames
        self.id = id
        self.names = names
        self.image = image
        self.imageID = imageID
        self.state = state
        self.status = status
        self.created = created
        self.ports = ports
        self.labels = labels
    }

    public var name: String {
        let n = names.first ?? id
        guard n.first == "/" || n.last == "/" else { return n }
        var s = Substring(n)
        while s.first == "/" { s.removeFirst() }
        while s.last == "/" { s.removeLast() }
        return String(s)
    }
    public var composeProject: String? { labels["com.docker.compose.project"] }
    public var composeService: String? { labels["com.docker.compose.service"] }
    public var domains: [String] {
        var out = ["\(name.lowercased()).fev.local"]
        if let p = composeProject, let s = composeService { out.insert("\(s).\(p).fev.local", at: 0) }
        if let extra = labels["dev.fevom.domains"] {
            out += extra.split(separator: ",").map { $0.trimmingCharacters(in: .whitespaces) }.filter { !$0.isEmpty }
        }
        return out
    }
}

public struct DockerImage: Codable, Sendable, Hashable, Identifiable {
    public var id: String
    public var repoTags: [String]
    public var size: Int64
    public var created: Int64

    enum CodingKeys: String, CodingKey { case id = "Id", repoTags = "RepoTags", size = "Size", created = "Created" }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        repoTags = try c.decodeIfPresent([String].self, forKey: .repoTags) ?? []
        size = try c.decodeIfPresent(Int64.self, forKey: .size) ?? 0
        created = try c.decodeIfPresent(Int64.self, forKey: .created) ?? 0
    }

    public init(id: String, repoTags: [String], size: Int64 = 0, created: Int64 = 0) {
        self.id = id
        self.repoTags = repoTags
        self.size = size
        self.created = created
    }

    public var name: String { repoTags.first(where: { $0 != "<none>:<none>" }) ?? String(id.replacingOccurrences(of: "sha256:", with: "").prefix(12)) }

    public func inUse(by containers: [Container]) -> Bool { !users(in: containers).isEmpty }

    public func users(in containers: [Container]) -> [String] {
        containers.filter { c in
            c.imageID.isEmpty
                ? repoTags.contains(c.image) || c.image == id || (!c.image.contains(":") && repoTags.contains(c.image + ":latest"))
                : c.imageID == id
        }.map(\.userLabel).sorted()
    }
}

public struct DockerVolume: Codable, Sendable, Hashable, Identifiable {
    public var name: String
    public var driver: String
    public var mountpoint: String
    public var createdAt: String
    public var id: String { name }

    enum CodingKeys: String, CodingKey { case name = "Name", driver = "Driver", mountpoint = "Mountpoint", createdAt = "CreatedAt" }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        name = try c.decode(String.self, forKey: .name)
        driver = try c.decodeIfPresent(String.self, forKey: .driver) ?? ""
        mountpoint = try c.decodeIfPresent(String.self, forKey: .mountpoint) ?? ""
        createdAt = try c.decodeIfPresent(String.self, forKey: .createdAt) ?? ""
    }

    public init(name: String, driver: String = "local") {
        self.name = name
        self.driver = driver
        mountpoint = ""
        createdAt = ""
    }

    public func inUse(by containers: [Container]) -> Bool { containers.contains { $0.volumeNames.contains(name) } }

    public func users(in containers: [Container]) -> [String] {
        containers.filter { $0.volumeNames.contains(name) }.map(\.userLabel).sorted()
    }
}

extension Container {
    public var userLabel: String {
        if let p = composeProject, let s = composeService { return "\(p)/\(s)" }
        return name
    }
}

public struct ContainerDetail: Decodable, Sendable {
    public struct State: Decodable, Sendable { public let Status: String?; public let Running: Bool?; public let Paused: Bool?; public let ExitCode: Int?; public let StartedAt: String? }
    public struct Config: Decodable, Sendable { public let Image: String?; public let Env: [String]?; public let Cmd: [String]?; public let Labels: [String: String]?; public let WorkingDir: String? }
    public struct MountPoint: Decodable, Sendable { public let `Type`: String?; public let Name: String?; public let Source: String?; public let Destination: String?; public let RW: Bool? }
    public struct PortBinding: Decodable, Sendable { public let HostIp: String?; public let HostPort: String? }
    public struct Network: Decodable, Sendable { public let IPAddress: String? }
    public struct Networking: Decodable, Sendable { public let Ports: [String: [PortBinding]?]?; public let Networks: [String: Network]? }
    public let Id: String
    public let Name: String?
    public let Created: String?
    public let State: State?
    public let Config: Config?
    public let Mounts: [MountPoint]?
    public let NetworkSettings: Networking?

    public var address: String? { NetworkSettings?.Networks?.values.compactMap { $0.IPAddress }.first { !$0.isEmpty } }
}
