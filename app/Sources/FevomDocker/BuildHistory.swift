import Foundation

public struct BuildRecord: Sendable, Hashable, Identifiable {
    public var ref: String
    public var name: String
    public var status: String
    public var created: Date?
    public var completed: Date?
    public var totalSteps: Int
    public var completedSteps: Int
    public var cachedSteps: Int
    public var id: String { ref }

    public var duration: TimeInterval? {
        guard let a = created, let b = completed else { return nil }
        return max(0, b.timeIntervalSince(a))
    }

    public var state: String {
        switch status.lowercased() {
        case "completed": "completed"
        case "running": "running"
        case "canceled": "canceled"
        default: "failed"
        }
    }

    public init(ref: String, name: String, status: String, created: Date?, completed: Date?, totalSteps: Int = 0, completedSteps: Int = 0, cachedSteps: Int = 0) {
        self.ref = ref
        self.name = name
        self.status = status
        self.created = created
        self.completed = completed
        self.totalSteps = totalSteps
        self.completedSteps = completedSteps
        self.cachedSteps = cachedSteps
    }

    public static func parseList(_ data: Data) -> [BuildRecord] {
        struct Line: Decodable {
            let ref: String
            let name: String?
            let status: String?
            let created_at: String?
            let completed_at: String?
            let total_steps: Int?
            let completed_steps: Int?
            let cached_steps: Int?
        }
        return String(decoding: data, as: UTF8.self).split(separator: "\n").compactMap { line in
            guard let l = try? JSONDecoder().decode(Line.self, from: Data(line.utf8)) else { return nil }
            return BuildRecord(ref: String(l.ref.split(separator: "/").last ?? Substring(l.ref)), name: l.name ?? "",
                               status: l.status ?? "", created: BuildTime.parse(l.created_at), completed: BuildTime.parse(l.completed_at),
                               totalSteps: l.total_steps ?? 0, completedSteps: l.completed_steps ?? 0, cachedSteps: l.cached_steps ?? 0)
        }
    }
}

public struct BuildStep: Sendable, Hashable, Identifiable {
    public var digest: String
    public var name: String
    public var started: Date?
    public var completed: Date?
    public var cached: Bool
    public var error: String
    public var log: String
    public var id: String { digest }

    public init(digest: String, name: String, started: Date? = nil, completed: Date? = nil, cached: Bool = false, error: String = "", log: String = "") {
        self.digest = digest
        self.name = name
        self.started = started
        self.completed = completed
        self.cached = cached
        self.error = error
        self.log = log
    }

    public var isInternal: Bool { name.hasPrefix("[internal]") || name.hasPrefix("[auth]") }

    public var duration: TimeInterval? {
        guard let a = started, let b = completed else { return nil }
        return max(0, b.timeIntervalSince(a))
    }

    public var state: String {
        if !error.isEmpty { return "failed" }
        if cached { return "cached" }
        if completed != nil { return "completed" }
        return started != nil ? "running" : "waiting"
    }
}

public struct BuildSteps: Sendable, Hashable {
    public var steps: [BuildStep]
    public var tags: [String]

    public init(steps: [BuildStep], tags: [String]) {
        self.steps = steps
        self.tags = tags
    }

    public static func parse(_ data: Data) -> BuildSteps {
        struct Vertex: Decodable { let digest: String; let name: String?; let started: String?; let completed: String?; let cached: Bool?; let error: String? }
        struct Status: Decodable { let id: String? }
        struct Log: Decodable { let vertex: String?; let data: Data? }
        struct Update: Decodable { let vertexes: [Vertex]?; let statuses: [Status]?; let logs: [Log]? }
        var byDigest: [String: BuildStep] = [:]
        var order: [String] = []
        var logs: [String: Data] = [:]
        var tags: [String] = []
        for line in String(decoding: data, as: UTF8.self).split(separator: "\n") {
            guard let u = try? JSONDecoder().decode(Update.self, from: Data(line.utf8)) else { continue }
            for v in u.vertexes ?? [] {
                var s = byDigest[v.digest] ?? BuildStep(digest: v.digest, name: "", started: nil, completed: nil, cached: false, error: "", log: "")
                if byDigest[v.digest] == nil { order.append(v.digest) }
                if let n = v.name, !n.isEmpty { s.name = n }
                if let t = BuildTime.parse(v.started), s.started.map({ t < $0 }) ?? true { s.started = t }
                if let t = BuildTime.parse(v.completed), s.completed.map({ t > $0 }) ?? true { s.completed = t }
                if v.cached == true { s.cached = true }
                if let e = v.error, !e.isEmpty { s.error = e }
                byDigest[v.digest] = s
            }
            for st in u.statuses ?? [] {
                if let id = st.id, id.hasPrefix("naming to ") {
                    let t = String(id.dropFirst("naming to ".count)).trimmingCharacters(in: .whitespaces)
                    if !t.isEmpty, !tags.contains(t) { tags.append(t) }
                }
            }
            for l in u.logs ?? [] {
                if let v = l.vertex, let d = l.data { logs[v, default: Data()].append(d) }
            }
        }
        var steps = order.compactMap { byDigest[$0] }
        for i in steps.indices { steps[i].log = String(decoding: logs[steps[i].digest] ?? Data(), as: UTF8.self) }
        let rank = Dictionary(uniqueKeysWithValues: order.enumerated().map { ($1, $0) })
        steps.sort { a, b in
            switch (a.started, b.started) {
            case let (x?, y?): x != y ? x < y : rank[a.digest, default: 0] < rank[b.digest, default: 0]
            case (.some, nil): true
            case (nil, .some): false
            default: rank[a.digest, default: 0] < rank[b.digest, default: 0]
            }
        }
        return BuildSteps(steps: steps, tags: tags)
    }
}

public enum BuildTime {
    public static func parse(_ s: String?) -> Date? {
        guard var s, !s.isEmpty else { return nil }
        if let dot = s.firstIndex(of: "."), let end = s[dot...].firstIndex(where: { $0 == "Z" || $0 == "+" || $0 == "-" }) {
            let frac = s[s.index(after: dot)..<end]
            s.replaceSubrange(s.index(after: dot)..<end, with: String((frac + "000").prefix(3)))
        }
        let f = ISO8601DateFormatter()
        f.formatOptions = s.contains(".") ? [.withInternetDateTime, .withFractionalSeconds] : [.withInternetDateTime]
        return f.date(from: s)
    }
}

public struct BuildStepsCache: Sendable {
    public let budget: Int
    private var entries: [String: BuildSteps] = [:]
    private var order: [String] = []
    public private(set) var bytes = 0

    public init(budget: Int) { self.budget = budget }

    public static func size(_ s: BuildSteps) -> Int {
        s.steps.reduce(0) { $0 + $1.log.utf8.count + $1.error.utf8.count + $1.name.utf8.count } + s.tags.reduce(0) { $0 + $1.utf8.count }
    }

    public var count: Int { entries.count }

    public mutating func get(_ ref: String) -> BuildSteps? {
        guard let s = entries[ref] else { return nil }
        order.removeAll { $0 == ref }
        order.append(ref)
        return s
    }

    public mutating func put(_ ref: String, _ s: BuildSteps) {
        remove(ref)
        let n = Self.size(s)
        guard n <= budget else { return }
        entries[ref] = s
        order.append(ref)
        bytes += n
        while bytes > budget, let oldest = order.first { remove(oldest) }
    }

    public mutating func remove(_ ref: String) {
        guard let s = entries.removeValue(forKey: ref) else { return }
        order.removeAll { $0 == ref }
        bytes -= Self.size(s)
    }

    public mutating func removeAll() {
        entries = [:]
        order = []
        bytes = 0
    }
}
