import FevomAPI
import FevomDocker
import Foundation
import Observation
import Synchronization

@MainActor @Observable
public final class LogFeed {
    public struct Source: Sendable, Equatable {
        public var id: String
        public var label: String
        public init(id: String, label: String = "") {
            self.id = id
            self.label = label
        }
    }

    public let title: String
    public let sources: [Source]
    public private(set) var buffer: LogBuffer
    public private(set) var rows = LogRows()
    public private(set) var version = 0
    public private(set) var deliveries = 0
    public private(set) var ended: String?
    public private(set) var following = false
    public var filterText = "" {
        didSet { if filterText != oldValue { refilter() } }
    }

    public static let displayLimit = 4096
    public nonisolated static let interval: UInt64 = 33_000_000

    private let socketPath: String
    private let tail: Int
    private var streams: [DockerLogStream] = []
    private let daemonPlace: DaemonPlace?
    public let daemonSources: [String]
    public let pod: DaemonAPI.K8sLogsParams?
    private var daemonTask: Task<Void, Never>?
    private var daemonClient: DaemonClient?
    private nonisolated let pending = Mutex(Pending())
    private var filterGen = 0

    struct Pending: Sendable {
        var lines: [Item] = []
        var scheduled = false
        var lastDelivery: UInt64 = 0
    }

    struct Item: Sendable {
        var key: TimeKey
        var bytes: [UInt8]
        var stream: UInt8 = 1
        var ended = true
    }

    public init(title: String, socketPath: String, sources: [Source], tail: Int = 2000,
                capacity: Int = LogBuffer.defaultCapacity, byteCapacity: Int = LogBuffer.defaultByteCapacity) {
        self.title = title
        self.socketPath = socketPath
        self.sources = sources
        self.tail = tail
        daemonPlace = nil
        daemonSources = []
        pod = nil
        buffer = LogBuffer(capacity: capacity, byteCapacity: byteCapacity)
    }

    public init(title: String, place: DaemonPlace, daemonSources: [String], tail: Int = 2000,
                capacity: Int = LogBuffer.defaultCapacity, byteCapacity: Int = LogBuffer.defaultByteCapacity) {
        self.title = title
        socketPath = ""
        sources = []
        self.tail = tail
        daemonPlace = place
        self.daemonSources = daemonSources
        pod = nil
        buffer = LogBuffer(capacity: capacity, byteCapacity: byteCapacity)
    }

    public init(title: String, place: DaemonPlace, pod: DaemonAPI.K8sLogsParams, tail: Int = 2000,
                capacity: Int = LogBuffer.defaultCapacity, byteCapacity: Int = LogBuffer.defaultByteCapacity) {
        self.title = title
        socketPath = ""
        sources = []
        self.tail = tail
        daemonPlace = place
        daemonSources = []
        var p = pod
        p.timestamps = true
        if p.tailLines == 0 { p.tailLines = tail }
        self.pod = p
        buffer = LogBuffer(capacity: capacity, byteCapacity: byteCapacity)
    }

    public var merged: Bool { sources.count > 1 }

    public func start() {
        if pod != nil {
            startPod()
            return
        }
        if daemonPlace != nil {
            startDaemon()
            return
        }
        guard streams.isEmpty, !following else { return }
        following = true
        ended = nil
        let n = sources.count
        let backlog = Mutex([[Item]](repeating: [], count: n))
        let left = Mutex(n)
        for (i, src) in sources.enumerated() {
            do {
                let s = try DockerLogStream(socketPath: socketPath, container: src.id, options: .init(follow: false, tail: tail, timestamps: true),
                    onLines: { [label = src.label] lines in
                        let items = lines.map { Self.item($0, label: label) }
                        backlog.withLock { $0[i] += items }
                    },
                    onEnd: { [weak self] why in
                        let done = left.withLock { n in n -= 1; return n == 0 }
                        guard done else { return }
                        let each = backlog.withLock { $0 }
                        DispatchQueue.main.async {
                            MainActor.assumeIsolated { self?.backlogDone(each, error: why) }
                        }
                    })
                streams.append(s)
            } catch {
                ended = String(describing: error)
                stop()
                return
            }
        }
    }

    private func backlogDone(_ each: [[Item]], error: String?) {
        guard following else { return }
        streams = []
        let cuts = each.map { FollowCut($0) }
        append(cuts.flatMap(\.shown).sorted { $0.key < $1.key })
        if let error {
            ended = error
            following = false
            return
        }
        for (i, src) in sources.enumerated() {
            let cut = Mutex(cuts[i])
            let since = cuts[i].since
            do {
                let s = try DockerLogStream(socketPath: socketPath, container: src.id, options: .init(follow: true, tail: nil, timestamps: true, since: since),
                    onLines: { [weak self, label = src.label] lines in
                        let items = cut.withLock { c in lines.map { Self.item($0, label: label) }.filter { c.keep($0) } }
                        if !items.isEmpty { self?.arrived(items) }
                    },
                    onEnd: { [weak self] why in
                        DispatchQueue.main.async { MainActor.assumeIsolated { self?.streamEnded(why) } }
                    })
                streams.append(s)
            } catch {
                ended = String(describing: error)
            }
        }
    }

    private func streamEnded(_ why: String?) {
        if let why { ended = why }
    }

    public func stop() {
        following = false
        for s in streams { s.cancel() }
        streams = []
        daemonTask?.cancel()
        daemonTask = nil
        daemonClient?.close()
        daemonClient = nil
    }

    private func startDaemon() {
        guard let place = daemonPlace, daemonTask == nil, !following else { return }
        following = true
        ended = nil
        let sources = daemonSources
        let backlog = tail
        let labels = Self.labels(for: sources)
        daemonTask = Task { [weak self] in
            let client: DaemonClient
            do {
                client = try await DaemonClient.connect(home: place.home, build: place.build, client: "fev-app-logs")
            } catch {
                self?.daemonEnded(String(describing: error))
                return
            }
            guard let self, self.following, !Task.isCancelled else {
                client.close()
                return
            }
            self.daemonClient = client
            let reader = Task.detached { [weak self] in
                for await note in client.connection.notes {
                    guard let e = try? note.decode(DaemonAPI.Methods.logEntry) else { continue }
                    var items: [Item] = []
                    if e.dropped > 0 {
                        items.append(Self.daemonItem(time: e.time, label: "", text: "[\(e.dropped) lines dropped: they came faster than this view could show them]"))
                    }
                    items.append(Self.daemonItem(time: e.time, label: labels[e.source] ?? "", text: e.text))
                    self?.arrived(items)
                }
            }
            do {
                _ = try await client.call(DaemonAPI.Methods.logSubscribe, .init(sources: sources, backlog: backlog))
            } catch {
                reader.cancel()
                client.close()
                self.daemonEnded(String(describing: error))
                return
            }
            await reader.value
            if self.following, self.daemonClient === client {
                self.daemonEnded("the connection to Fevom ended")
            }
        }
    }

    private func startPod() {
        guard let place = daemonPlace, let params = pod, daemonTask == nil, !following else { return }
        following = true
        ended = nil
        daemonTask = Task { [weak self] in
            let client: DaemonClient
            do {
                client = try await DaemonClient.connect(home: place.home, build: place.build, client: "fev-app-k8s-logs")
            } catch {
                self?.daemonEnded(String(describing: error))
                return
            }
            guard let self, self.following, !Task.isCancelled else {
                client.close()
                return
            }
            self.daemonClient = client
            let reader = Task.detached { [weak self] () -> String? in
                var partial: [UInt8] = []
                for await note in client.connection.notes {
                    if let l = try? note.decode(DaemonAPI.Methods.k8sLog) {
                        partial += Array(l.text.utf8)
                        var items: [Item] = []
                        while let nl = partial.firstIndex(of: 0x0A) {
                            items.append(Self.podItem(Array(partial[..<nl])))
                            partial.removeSubrange(...nl)
                        }
                        if !items.isEmpty { self?.arrived(items) }
                    } else if let e = try? note.decode(DaemonAPI.Methods.k8sLogEnd) {
                        if !partial.isEmpty { self?.arrived([Self.podItem(partial)]) }
                        return e.error.isEmpty ? "the log ended" : e.error
                    }
                }
                return nil
            }
            do {
                _ = try await client.call(DaemonAPI.Methods.k8sLogs, params)
            } catch {
                reader.cancel()
                client.close()
                self.daemonEnded(String(describing: error))
                return
            }
            let why = await reader.value
            if self.following, self.daemonClient === client {
                client.close()
                self.daemonEnded(why ?? "the connection to Fevom ended")
            }
        }
    }

    nonisolated static func podItem(_ b: [UInt8]) -> Item {
        guard let sp = b.firstIndex(of: 0x20), let key = TimeKey(b[0..<sp]) else {
            return Item(key: TimeKey(seconds: 0, nanos: 0), bytes: b)
        }
        return Item(key: key, bytes: b)
    }

    private func daemonEnded(_ why: String) {
        guard following else { return }
        ended = why
        following = false
        daemonTask = nil
        daemonClient = nil
    }

    nonisolated static func labels(for sources: [String]) -> [String: String] {
        let all = sources.isEmpty ? [DaemonAPI.Const.logDaemon, DaemonAPI.Const.logVMM, DaemonAPI.Const.logConsole, DaemonAPI.Const.logKernel,
                                     DaemonAPI.Const.logInit, DaemonAPI.Const.logEngine] : sources
        guard all.count > 1 else { return [:] }
        func short(_ s: String) -> String {
            guard s.hasPrefix(DaemonAPI.Const.logMachinePrefix) else { return s }
            return s.hasSuffix(":runtime") ? "runtime" : "console"
        }
        let width = all.map { short($0).count }.max() ?? 0
        var out: [String: String] = [:]
        for s in all { out[s] = short(s).padding(toLength: width, withPad: " ", startingAt: 0) + " | " }
        return out
    }

    nonisolated static func daemonItem(time: String, label: String, text: String) -> Item {
        let lead = Array(time.utf8)
        let key = TimeKey(lead[...]) ?? TimeKey(seconds: 0, nanos: 0)
        return Item(key: key, bytes: lead + [0x20] + Array(label.utf8) + Array(text.utf8))
    }

    nonisolated func arrived(_ items: [Item]) {
        let schedule: UInt64? = pending.withLock { p in
            p.lines += items
            if p.scheduled { return nil }
            p.scheduled = true
            let now = clock_gettime_nsec_np(CLOCK_UPTIME_RAW)
            let due = p.lastDelivery + Self.interval
            return due > now ? due - now : 0
        }
        guard let wait = schedule else { return }
        let deliver: @Sendable () -> Void = { [weak self] in MainActor.assumeIsolated { self?.deliver() } }
        if wait == 0 {
            DispatchQueue.main.async(execute: deliver)
        } else {
            DispatchQueue.main.asyncAfter(deadline: .now() + .nanoseconds(Int(wait)), execute: deliver)
        }
    }

    private func deliver() {
        let items = pending.withLock { p -> [Item] in
            defer {
                p.lines = []
                p.scheduled = false
                p.lastDelivery = clock_gettime_nsec_np(CLOCK_UPTIME_RAW)
            }
            return p.lines
        }
        guard following else { return }
        append(merged ? items.sorted { $0.key < $1.key } : items)
    }

    private func append(_ items: [Item]) {
        guard !items.isEmpty else { return }
        let from = buffer.endSeq
        for it in items { buffer.append(it.bytes) }
        rows.appended(from: max(from, buffer.firstSeq), buffer)
        rows.dropped(buffer)
        deliveries += 1
        version += 1
    }

    private func refilter() {
        filterGen += 1
        let gen = filterGen
        let f = LogFilter(filterText)
        if f.isEmpty {
            rows = LogRows()
            version += 1
            return
        }
        let snapshot = buffer
        Task.detached(priority: .userInitiated) {
            let (r, upTo) = LogRows.scanned(f, snapshot)
            await MainActor.run { [weak self] in
                guard let self, self.filterGen == gen else { return }
                var r = r
                r.appended(from: upTo, self.buffer)
                r.dropped(self.buffer)
                self.rows = r
                self.version += 1
            }
        }
    }

    public func clear() {
        buffer.clear()
        rows.dropped(buffer)
        if !rows.filter.isEmpty { rows = LogRows.scanned(rows.filter, buffer).0 }
        version += 1
    }

    public var rowCount: Int { rows.count(buffer) }

    public func display(row: Int, timestamps: Bool) -> (text: String, more: Int) {
        let seq = rows.seq(row: row, buffer)
        guard let d = buffer.display(seq, limit: Self.displayLimit) else { return ("", 0) }
        return (timestamps ? d.text : Self.withoutTimestamp(d.text), d.more)
    }

    public func line(row: Int, timestamps: Bool) -> String {
        let s = buffer.line(rows.seq(row: row, buffer)) ?? ""
        return timestamps ? s : Self.withoutTimestamp(s)
    }

    public func cells(row: Int, timestamps: Bool) -> Int {
        let seq = rows.seq(row: row, buffer)
        let all = buffer.cells(seq, limit: Self.displayLimit)
        guard !timestamps, let b = buffer.bytes(seq), let sp = b.prefix(40).firstIndex(of: 0x20), b.count > 20, b[b.startIndex + 4] == 0x2D else { return all }
        return max(0, all - (sp - b.startIndex + 1))
    }

    nonisolated static func withoutTimestamp(_ s: String) -> String {
        guard s.count > 20, s.dropFirst(4).first == "-", let sp = s.firstIndex(of: " "), s.distance(from: s.startIndex, to: sp) <= 36 else { return s }
        return String(s[s.index(after: sp)...])
    }

    nonisolated static func item(_ line: DockerLogStream.Line, label: String) -> Item {
        let b = line.bytes
        guard let sp = b.firstIndex(of: 0x20), let key = TimeKey(b[0..<sp]) else {
            return Item(key: TimeKey(seconds: 0, nanos: 0), bytes: label.isEmpty ? b : Array(label.utf8) + b, stream: line.stream, ended: line.ended)
        }
        if label.isEmpty { return Item(key: key, bytes: b, stream: line.stream, ended: line.ended) }
        return Item(key: key, bytes: Array(b[0...sp]) + Array(label.utf8) + Array(b[(sp + 1)...]), stream: line.stream, ended: line.ended)
    }
}

struct FollowCut: Sendable {
    var shown: [LogFeed.Item] = []
    var last: TimeKey?
    var open: [UInt8: (key: TimeKey, skip: Int)] = [:]

    init(_ items: [LogFeed.Item]) {
        var drop = Set<Int>()
        for stream in Set(items.map(\.stream)) {
            guard let end = items.lastIndex(where: { $0.stream == stream }), !items[end].ended else { continue }
            let key = items[end].key
            drop.insert(end)
            var skip = 0
            for i in items[..<end].indices.reversed() where items[i].stream == stream {
                guard !items[i].ended, items[i].key == key else { break }
                skip += 1
            }
            open[stream] = (key, skip)
        }
        for (i, it) in items.enumerated() where !drop.contains(i) {
            shown.append(it)
            if it.ended, last.map({ $0 < it.key }) ?? true { last = it.key }
        }
    }

    var since: String? {
        let keys = [last].compactMap { $0 } + open.values.map(\.key)
        return keys.min().map { "\($0.seconds)" }
    }

    mutating func keep(_ item: LogFeed.Item) -> Bool {
        if var o = open[item.stream], o.key == item.key {
            if o.skip == 0 { return true }
            o.skip -= 1
            open[item.stream] = o
            return false
        }
        if let last, item.key <= last { return false }
        return true
    }
}

struct TimeKey: Comparable, Sendable {
    var seconds: Int64
    var nanos: Int32

    init(seconds: Int64, nanos: Int32) {
        self.seconds = seconds
        self.nanos = nanos
    }

    init?(_ b: ArraySlice<UInt8>) {
        let a = Array(b)
        guard a.count >= 20, a[4] == 0x2D, a[7] == 0x2D, a[10] == 0x54, a[13] == 0x3A, a[16] == 0x3A, a.last == 0x5A else { return nil }
        func num(_ r: Range<Int>) -> Int? {
            var n = 0
            for i in r {
                guard a[i] >= 48 && a[i] <= 57 else { return nil }
                n = n * 10 + Int(a[i] - 48)
            }
            return n
        }
        guard let y = num(0..<4), let mo = num(5..<7), let d = num(8..<10), let h = num(11..<13), let mi = num(14..<16), let s = num(17..<19) else { return nil }
        var ns = 0
        if a[19] == 0x2E {
            var digits = 0
            for i in 20..<(a.count - 1) {
                guard a[i] >= 48 && a[i] <= 57 else { return nil }
                if digits < 9 { ns = ns * 10 + Int(a[i] - 48); digits += 1 }
            }
            while digits < 9 { ns *= 10; digits += 1 }
        }
        let yy = mo <= 2 ? y - 1 : y
        let era = (yy >= 0 ? yy : yy - 399) / 400
        let yoe = yy - era * 400
        let doy = (153 * (mo + (mo > 2 ? -3 : 9)) + 2) / 5 + d - 1
        let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy
        let days = era * 146097 + doe - 719468
        seconds = Int64(days) * 86400 + Int64(h * 3600 + mi * 60 + s)
        nanos = Int32(ns)
    }

    static func < (a: TimeKey, b: TimeKey) -> Bool { (a.seconds, a.nanos) < (b.seconds, b.nanos) }
}
