import FevomAPI
import FevomDocker
import Foundation

public struct Activity: Sendable {
    public static let historyLimit = 150

    public struct Figures: Sendable, Equatable {
        public var kind: String
        public var name: String
        public var id: String
        public var memoryBytes: UInt64
        public var pids: Int
        public var cpuPercent: Double?
        public var netRx: Double?
        public var netTx: Double?
        public var diskRead: Double?
        public var diskWrite: Double?
    }

    public struct Point: Sendable, Equatable, Identifiable {
        public var time: Date
        public var cpuPercent: Double
        public var memoryBytes: UInt64
        public var netRx: Double
        public var netTx: Double
        public var diskRead: Double
        public var diskWrite: Double
        public var footprintBytes: UInt64
        public var id: Date { time }
    }

    public private(set) var history: [Point] = []
    public private(set) var groups: [Figures] = []
    public private(set) var state = ""
    public private(set) var footprintBytes: UInt64?
    public private(set) var heldBytes: UInt64?
    public private(set) var guestMemTotalKB: Int64 = 0
    public private(set) var guestMemAvailableKB: Int64 = 0
    public private(set) var samples = 0

    private var previous: (time: Date, groups: [String: DaemonAPI.StatsGroup], vmmCPU: Double?)?

    public init() {}

    public static func key(_ g: DaemonAPI.StatsGroup) -> String {
        g.kind + "/" + (g.id.isEmpty ? g.name : g.id)
    }

    public mutating func reset() { self = Activity() }

    public mutating func ingest(_ s: DaemonAPI.StatsSample) {
        samples += 1
        state = s.state
        footprintBytes = s.vmm?.footprintBytes
        heldBytes = s.vmm?.heldBytes
        guard let time = Self.parseTime(s.time) else { return }
        guard let guest = s.guest else {
            groups = []
            previous = nil
            return
        }
        guestMemTotalKB = guest.memTotalKB
        guestMemAvailableKB = guest.memAvailableKB
        var now: [String: DaemonAPI.StatsGroup] = [:]
        var figures: [Figures] = []
        let dt = previous.map { time.timeIntervalSince($0.time) } ?? 0
        for g in guest.groups {
            let k = Self.key(g)
            now[k] = g
            var f = Figures(kind: g.kind, name: g.name, id: g.id, memoryBytes: g.memoryBytes, pids: g.piDs)
            if dt > 0, let p = previous?.groups[k] {
                f.cpuPercent = Self.rate(g.cpuUsec, p.cpuUsec, dt).map { $0 / 10_000 }
                f.netRx = Self.rate(g.netRxBytes, p.netRxBytes, dt)
                f.netTx = Self.rate(g.netTxBytes, p.netTxBytes, dt)
                f.diskRead = Self.rate(g.diskReadBytes, p.diskReadBytes, dt)
                f.diskWrite = Self.rate(g.diskWriteBytes, p.diskWriteBytes, dt)
            }
            figures.append(f)
        }
        groups = figures
        if dt > 0, let sys = figures.first(where: { $0.kind == DaemonAPI.Const.statsSystem }), sys.cpuPercent != nil {
            history.append(Point(time: time, cpuPercent: sys.cpuPercent ?? 0, memoryBytes: sys.memoryBytes, netRx: sys.netRx ?? 0, netTx: sys.netTx ?? 0,
                                 diskRead: sys.diskRead ?? 0, diskWrite: sys.diskWrite ?? 0, footprintBytes: s.vmm?.footprintBytes ?? 0))
            if history.count > Self.historyLimit { history.removeFirst(history.count - Self.historyLimit) }
        }
        previous = (time, now, s.vmm?.cpums)
    }

    static func rate(_ now: UInt64, _ before: UInt64, _ dt: TimeInterval) -> Double? {
        guard now >= before, dt > 0 else { return nil }
        return Double(now - before) / dt
    }

    static func parseTime(_ s: String) -> Date? {
        var whole = s
        var fraction = 0.0
        if let dot = s.firstIndex(of: ".") {
            var end = s.index(after: dot)
            while end < s.endIndex, s[end].isNumber { end = s.index(after: end) }
            fraction = Double("0" + s[dot..<end]) ?? 0
            whole = String(s[..<dot]) + String(s[end...])
        }
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime]
        return f.date(from: whole).map { $0.addingTimeInterval(fraction) }
    }

    public struct Row: Sendable, Equatable, Identifiable {
        public enum Kind: String, Sendable { case system, service, project, container, machine }
        public var id: String
        public var kind: Kind
        public var name: String
        public var cpuPercent: Double?
        public var memoryBytes: UInt64
        public var netRx: Double?
        public var netTx: Double?
        public var diskRead: Double?
        public var diskWrite: Double?
        public var pids: Int
        public var children: [Row]?
    }

    public func rows(containers: [Container]) -> [Row] {
        let byID = Dictionary(containers.map { ($0.id, $0) }, uniquingKeysWith: { a, _ in a })
        var out: [Row] = []
        var projects: [String: [Row]] = [:]
        var projectOrder: [String] = []
        var lone: [Row] = []
        var machines: [Row] = []
        for f in groups {
            var r = Row(id: f.kind + "/" + (f.id.isEmpty ? f.name : f.id), kind: Row.Kind(rawValue: f.kind) ?? .service, name: f.name,
                        cpuPercent: f.cpuPercent, memoryBytes: f.memoryBytes, netRx: f.netRx, netTx: f.netTx,
                        diskRead: f.diskRead, diskWrite: f.diskWrite, pids: f.pids)
            switch f.kind {
            case DaemonAPI.Const.statsSystem:
                r.name = "All of Linux"
                out.append(r)
            case DaemonAPI.Const.statsService:
                r.name = f.name == "engine" ? "Docker engine" : f.name
                out.append(r)
            case DaemonAPI.Const.statsContainer:
                let c = byID[f.id]
                if let c { r.name = c.name }
                if let p = c?.composeProject {
                    if projects[p] == nil { projectOrder.append(p) }
                    if let s = c?.composeService { r.name = s }
                    projects[p, default: []].append(r)
                } else {
                    lone.append(r)
                }
            case DaemonAPI.Const.statsMachine:
                machines.append(r)
            default:
                out.append(r)
            }
        }
        for p in projectOrder.sorted() {
            let members = projects[p] ?? []
            out.append(Row(id: "project/" + p, kind: .project, name: p,
                           cpuPercent: Self.sum(members.map(\.cpuPercent)), memoryBytes: members.reduce(0) { $0 + $1.memoryBytes },
                           netRx: Self.sum(members.map(\.netRx)), netTx: Self.sum(members.map(\.netTx)),
                           diskRead: Self.sum(members.map(\.diskRead)), diskWrite: Self.sum(members.map(\.diskWrite)),
                           pids: members.reduce(0) { $0 + $1.pids }, children: members))
        }
        out += lone.sorted { $0.name < $1.name }
        out += machines
        return out
    }

    static func sum(_ xs: [Double?]) -> Double? {
        var t = 0.0
        for x in xs {
            guard let x else { return nil }
            t += x
        }
        return t
    }
}
