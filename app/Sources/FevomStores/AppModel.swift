import FevomAPI
import FevomDocker
import Foundation
import Observation

public struct AppNotice: Sendable, Equatable {
    public var title: String
    public var body: String
    public init(title: String, body: String) {
        self.title = title
        self.body = body
    }
}

@MainActor @Observable
public final class AppModel {
    public enum Link: Equatable, Sendable {
        case idle
        case connecting
        case startingDaemon
        case connected
        case stoppedByUser
        case failed(String)
    }

    public private(set) var link: Link = .idle {
        didSet {
            switch link {
            case .connecting, .startingDaemon: break
            default:
                let w = linkWaiters
                linkWaiters = []
                for k in w { k.resume() }
            }
        }
    }
    private var linkWaiters: [CheckedContinuation<Void, Never>] = []
    public private(set) var status: DaemonAPI.Status?
    public private(set) var machines: [DaemonAPI.Machine] = []
    public private(set) var defaultMachine = ""
    public private(set) var containers: [Container] = []
    public private(set) var images: [DockerImage] = []
    public private(set) var volumes: [DockerVolume] = []
    public private(set) var networks: [DockerNetwork] = []
    public internal(set) var builds: [BuildRecord] = []
    public internal(set) var buildTags: [String: [String]] = [:]
    public internal(set) var buildsError: String?
    var taggingBuilds = false
    @ObservationIgnored var buildSteps = BuildStepsCache(budget: 4 << 20)
    var buildFollower: (ref: String, token: UUID, task: Task<Void, Never>)?
    var followedBuilds: Set<String> = []
    public private(set) var networkEvents: [String: Int] = [:]
    public private(set) var operations: [String: DaemonAPI.Operation] = [:]
    public var lastError: String?
    public var lastInfo: String?
    public internal(set) var debug: DaemonAPI.DebugStatus?

    public internal(set) var contexts: [DaemonAPI.RemoteContext]?
    public internal(set) var contextContainers: [String: [DaemonAPI.RemoteContainer]] = [:]
    public internal(set) var contextImages: [String: [DaemonAPI.RemoteImage]] = [:]
    public internal(set) var contextErrors: [String: String] = [:]

    public let place: DaemonPlace
    public var notify: @MainActor (AppNotice) -> Void = { _ in }
    public var terminate: @Sendable (pid_t) -> Int32 = { Darwin.kill($0, SIGTERM) == 0 ? 0 : errno }
    public var otherBuildStopBound: Duration = .seconds(45)

    var client: DaemonClient?
    private var session: Task<Void, Never>?
    private var viewers = 0
    private var watch: RunDirWatch?
    private var retryPending = false
    private var listViewers: [DockerList: Int] = [:]
    private var refreshing: Set<DockerList> = []
    private var dirty: Set<DockerList> = []

    public internal(set) var activity = Activity()
    var statsViewers = 0
    var mainWindowShown = true
    var statsSubscription: UInt64?
    var statsSubscribing = false
    public internal(set) var configKeys: [DaemonAPI.ConfigKey] = []
    public internal(set) var configValues: [String: DaemonAPI.ConfigValue] = [:]
    var configViewers = 0
    public internal(set) var forwards: [DaemonAPI.Forward] = []
    var forwardsViewers = 0
    public let k8s = K8sModel()

    public internal(set) var counters = Counters()
    public struct Counters: Sendable, Equatable {
        public var connects = 0
        public var notifications = 0
        public var containerFetches = 0
        public var imageFetches = 0
        public var volumeFetches = 0
        public var networkFetches = 0
        public var buildFetches = 0
        public var watchEvents = 0
        public var daemonStarts = 0
        public var opens = 0
        public var statsSubscribes = 0
        public var statsUnsubscribes = 0
        public var statsSamples = 0
        public var configFetches = 0
        public var forwardsFetches = 0
        public var unseenFailures = 0
    }

    public init(place: DaemonPlace) {
        self.place = place
    }

    public func opened() {
        counters.opens += 1
        do {
            try FileManager.default.removeItem(atPath: DaemonAPI.stopMarkPath(home: place.home))
        } catch let e as CocoaError where e.code == .fileNoSuchFile {
        } catch {
            lastError = "Could not remove the stop mark: \(error)"
        }
        startOnConnect = true
        replaceOnConnect = replacesOtherBuildOnOpen
        guard isActive else { return }
        if let client {
            startIfStopped(client)
        } else if session == nil {
            watch?.cancel()
            watch = nil
            activate()
        }
    }

    private var startOnConnect = false

    public var replacesOtherBuildOnOpen = !DaemonPlace.honoursEnvironment
    private var replaceOnConnect = false

    public var onStartAtLogin: (@MainActor (Bool) -> Void)?

    private func tellStartAtLogin(_ v: JSONValue?) {
        if case .bool(let on)? = v { onStartAtLogin?(on) }
    }

    private func startIfStopped(_ client: DaemonClient) {
        startOnConnect = false
        guard status?.state == DaemonAPI.Const.stateStopped else { return }
        Task { [weak self] in
            do {
                let st = try await client.call(DaemonAPI.Methods.vmStart, .init(noWait: true))
                if let self, let s = self.status, st.seq >= s.seq { self.status = st }
            } catch {
                self?.lastError = String(describing: error)
            }
        }
    }

    public var isActive: Bool { viewers > 0 }
    public var isWatching: Bool { watch != nil }

    public func show() {
        viewers += 1
        if viewers == 1 { activate() }
    }

    public func hide() {
        viewers = max(0, viewers - 1)
        if viewers == 0 { deactivate() }
    }

    private func activate() {
        guard session == nil else { return }
        session = Task { [weak self] in await self?.run(mayStart: true) }
    }

    private func deactivate() {
        retryPending = false
        session?.cancel()
        session = nil
        client?.close()
        client = nil
        watch?.cancel()
        watch = nil
        statsSubscription = nil
        statsSubscribing = false
        k8s.connectionEnded()
        startOnConnect = false
        link = .idle
    }

    private func run(mayStart: Bool) async {
        if mayStart { link = .connecting }
        let client: DaemonClient
        do {
            client = try await connect(mayStart: mayStart)
        } catch is StoppedByUser {
            link = .stoppedByUser
            status = nil
            session = nil
            armWatch()
            return
        } catch let e as RPCConnection.Failure where !mayStart && Self.notThere(e) {
            session = nil
            armWatch()
            if retryPending, isActive {
                retryPending = false
                session = Task { [weak self] in await self?.run(mayStart: false) }
                return
            }
            let w = linkWaiters
            linkWaiters = []
            for k in w { k.resume() }
            return
        } catch {
            link = .failed(String(describing: error))
            session = nil
            armWatch()
            return
        }
        if Task.isCancelled || !isActive { client.close(); return }
        watch?.cancel()
        watch = nil
        retryPending = false
        self.client = client
        counters.connects += 1
        do {
            let sub = try await client.call(DaemonAPI.Methods.eventsSubscribe)
            status = sub.status
            link = .connected
            if startOnConnect { startIfStopped(client) }
            async let ms = client.call(DaemonAPI.Methods.machinesList)
            refreshContainers()
            for list in listViewers.keys { refresh(list) }
            Task { await self.followViews(client) }
            let list = try await ms
            machines = list.machines
            defaultMachine = list.default
            await settleOperations(client)
            if onStartAtLogin != nil {
                let key = DaemonAPI.Const.keyStartAtLogin
                let got = try? await client.call(DaemonAPI.Methods.configGet, .init(keys: [key]))
                tellStartAtLogin(got?.values[key]?.value)
            }
        } catch {
            link = .failed(String(describing: error))
        }
        for await note in client.connection.notes {
            if Task.isCancelled { break }
            apply(note)
        }
        if self.client === client {
            self.client = nil
            if isActive && !Task.isCancelled {
                link = FileManager.default.fileExists(atPath: DaemonAPI.stopMarkPath(home: place.home))
                    ? .stoppedByUser : .failed("the daemon's connection ended")
                status = nil
                armWatch()
            }
            session = nil
        }
    }

    private func armWatch() {
        guard isActive, client == nil, watch == nil else { return }
        watch = RunDirWatch(home: place.home) { [weak self] in
            guard let self, self.isActive, self.client == nil else { return }
            guard self.session == nil else {
                self.retryPending = true
                return
            }
            self.counters.watchEvents += 1
            self.session = Task { [weak self] in await self?.run(mayStart: false) }
        }
    }

    struct StoppedByUser: Error {}

    static func notThere(_ e: RPCConnection.Failure) -> Bool {
        if case .connect(_, let code) = e { return code == ENOENT || code == ECONNREFUSED }
        return false
    }

    private func connect(mayStart: Bool) async throws -> DaemonClient {
        do {
            return try await DaemonClient.connect(home: place.home, build: place.build)
        } catch let e as RPCConnection.Failure where Self.notThere(e) {
            if !mayStart { throw e }
        } catch let e as DaemonError where mayStart && replaceOnConnect && Self.replaceable(e) != nil {
            replaceOnConnect = false
            let other = Self.replaceable(e)!
            if let why = await stopOther(other) {
                throw RPCConnection.Failure.protocolError(why)
            }
            lastInfo = "Replaced Fevom \(other.daemonBuild) with this app's build \(place.build)."
        }
        if FileManager.default.fileExists(atPath: DaemonAPI.stopMarkPath(home: place.home)) {
            throw StoppedByUser()
        }
        try await startDaemon()
        return try await DaemonClient.connect(home: place.home, build: place.build)
    }

    private func startDaemon() async throws {
        guard let fevd = place.fevd else {
            throw RPCConnection.Failure.protocolError(DaemonPlace.honoursEnvironment
                ? "no fevd to start: set FEV_DAEMON or install fev"
                : "no fevd to start: this app has no Contents/Helpers/fevd")
        }
        link = .startingDaemon
        counters.daemonStarts += 1
        let args = ["--home", place.home] + place.daemonArgs
        let env = place.daemonEnvironment
        let status: Int32 = try await withCheckedThrowingContinuation { k in
            let p = Process()
            p.executableURL = URL(fileURLWithPath: fevd)
            p.arguments = args
            if let env { p.environment = env }
            p.standardInput = FileHandle.nullDevice
            p.terminationHandler = { k.resume(returning: $0.terminationStatus) }
            do { try SessionAttach.spawning { try p.run() } } catch { k.resume(throwing: error) }
        }
        if status != 0 {
            throw RPCConnection.Failure.protocolError("fevd exited with status \(status) as it started; see \(place.home)/log")
        }
    }

    public func apply(_ note: RPCConnection.Note) {
        counters.notifications += 1
        do {
            if let p = try note.decode(DaemonAPI.Methods.stateChanged) {
                if let s = status, p.status.seq < s.seq { return }
                let was = status
                status = p.status
                if p.status.engine != DaemonAPI.Const.engineReady {
                    containers = []
                    networks = []
                } else {
                    refreshContainers()
                    if was?.engine != DaemonAPI.Const.engineReady { for list in listViewers.keys { refresh(list) } }
                }
                if p.status.k8s != was?.k8s { k8sLineChanged() }
                if p.status.state == DaemonAPI.Const.stateStopped, !p.status.error.isEmpty, was?.error != p.status.error {
                    notify(AppNotice(title: "Fevom stopped", body: p.status.error))
                }
            } else if let p = try note.decode(DaemonAPI.Methods.machinesChanged) {
                applyMachine(p)
            } else if let p = try note.decode(DaemonAPI.Methods.dockerEvent) {
                switch p.type {
                case "container": refreshContainers()
                case "image":
                    refresh(.images)
                    refresh(.builds)
                case "volume": refresh(.volumes)
                case "network":
                    if p.action == "destroy" { networkEvents[p.id] = nil } else { networkEvents[p.id, default: 0] += 1 }
                    refresh(.networks)
                default: break
                }
            } else if let p = try note.decode(DaemonAPI.Methods.operationProgress) {
                applyOperation(p.operation)
            } else if let p = try note.decode(DaemonAPI.Methods.statsSample) {
                applyStats(p)
            } else if let p = try note.decode(DaemonAPI.Methods.configChanged) {
                applyConfig(p)
                if p.key == DaemonAPI.Const.keyStartAtLogin { tellStartAtLogin(p.value) }
            } else if let p = try note.decode(DaemonAPI.Methods.forwardsChanged) {
                applyForwards(p)
            } else if let p = try note.decode(DaemonAPI.Methods.k8sChanged) {
                k8s.change(p)
            }
        } catch {
            lastError = "a \(note.method) the app could not read: \(error)"
        }
    }

    func applyMachine(_ p: DaemonAPI.MachinesChanged) {
        if p.removed {
            machines.removeAll { $0.name == p.machine.name }
        } else if let i = machines.firstIndex(where: { $0.name == p.machine.name }) {
            machines[i] = p.machine
        } else {
            machines.append(p.machine)
            machines.sort { $0.name < $1.name }
        }
        if p.machine.default {
            defaultMachine = p.machine.name
            for i in machines.indices where machines[i].name != p.machine.name { machines[i].default = false }
        }
    }

    func began(_ op: DaemonAPI.Operation) {
        guard operations[op.id] == nil else { return }
        operations[op.id] = op
        show()
        if Self.ended(op) { applyOperation(op) }
    }

    static func ended(_ op: DaemonAPI.Operation) -> Bool {
        op.state == DaemonAPI.Const.operationDone || op.state == DaemonAPI.Const.operationFailed
            || op.state == DaemonAPI.Const.operationCancelled
    }

    func applyOperation(_ op: DaemonAPI.Operation) {
        guard operations[op.id] != nil else { return }
        guard Self.ended(op) else {
            operations[op.id] = op
            return
        }
        operations[op.id] = nil
        notify(Self.notice(op))
        hide()
    }

    static func notice(_ op: DaemonAPI.Operation) -> AppNotice {
        let what: String
        switch op.method {
        case DaemonAPI.Const.methodMachinesCreate: what = "creating \(op.target)"
        case DaemonAPI.Const.methodMachinesExport: what = "exporting \(op.target)"
        case DaemonAPI.Const.methodMachinesImport: what = "importing \(op.target)"
        case DaemonAPI.Const.methodVolumesExport: what = "exporting the volume \(op.target)"
        case DaemonAPI.Const.methodVolumesImport: what = "importing the volume \(op.target)"
        case DaemonAPI.Const.methodK8sEnable: what = "turning Kubernetes on"
        case DaemonAPI.Const.methodK8sDisable: what = "turning Kubernetes off"
        case DaemonAPI.Const.methodK8sReset: what = "resetting Kubernetes"
        default: what = "\(op.method) of \(op.target)"
        }
        switch op.state {
        case DaemonAPI.Const.operationDone:
            if op.method == DaemonAPI.Const.methodMachinesCreate { return AppNotice(title: "\(op.target) is ready", body: "The machine is created and running.") }
            if op.method == DaemonAPI.Const.methodK8sEnable { return AppNotice(title: "Kubernetes is running", body: "kubectl --context fev reaches it.") }
            return AppNotice(title: "Done \(what)", body: "")
        case DaemonAPI.Const.operationCancelled:
            return AppNotice(title: "Cancelled \(what)", body: "")
        default:
            return AppNotice(title: "Failed \(what)", body: op.error)
        }
    }

    private func settleOperations(_ client: DaemonClient) async {
        for id in operations.keys {
            do {
                applyOperation(try await client.call(DaemonAPI.Methods.operationGet, .init(id: id)))
            } catch let e as DaemonError where e.code == DaemonAPI.Const.codeFailed {
                if var op = operations[id] {
                    op.state = DaemonAPI.Const.operationFailed
                    op.error = "fev no longer knows this operation (\(e.message))"
                    applyOperation(op)
                }
            } catch {}
        }
    }

    public enum DockerList: Sendable, Hashable { case images, volumes, networks, builds }

    public func watchList(_ list: DockerList) {
        listViewers[list, default: 0] += 1
        if listViewers[list] == 1 { refresh(list) }
    }

    func isWatching(_ list: DockerList) -> Bool { isActive && listViewers[list] != nil }

    public func refreshList(_ list: DockerList) { refresh(list) }

    public func unwatchList(_ list: DockerList) {
        let n = (listViewers[list] ?? 1) - 1
        listViewers[list] = n > 0 ? n : nil
        if list == .builds, listViewers[list] == nil {
            stopFollowingBuild()
            buildSteps.removeAll()
        }
    }

    public func refreshContainers() {
        guard isActive else { return }
        if refreshingContainers {
            containersDirty = true
            return
        }
        refreshingContainers = true
        Task { [weak self] in
            guard let self else { return }
            repeat {
                containersDirty = false
                guard isActive, status?.engine == DaemonAPI.Const.engineReady else { break }
                counters.containerFetches += 1
                do {
                    containers = try await docker.containers(all: true)
                    listSucceeded(.containers)
                } catch {
                    listFailed(.containers, error)
                }
            } while containersDirty
            refreshingContainers = false
        }
    }
    private var refreshingContainers = false
    private var containersDirty = false

    enum ListKey: Hashable { case containers, list(DockerList) }
    private var listErrors: [ListKey: String] = [:]
    func listFailed(_ key: ListKey, _ error: any Error) {
        let said = String(describing: error)
        listErrors[key] = said
        lastError = said
    }
    func listSucceeded(_ key: ListKey) {
        guard let said = listErrors.removeValue(forKey: key) else { return }
        if lastError == said, !listErrors.values.contains(said) { lastError = nil }
    }

    func refresh(_ list: DockerList) {
        guard isActive, listViewers[list] != nil else { return }
        if refreshing.contains(list) {
            dirty.insert(list)
            return
        }
        refreshing.insert(list)
        Task { [weak self] in
            guard let self else { return }
            repeat {
                dirty.remove(list)
                guard isActive, listViewers[list] != nil, status?.engine == DaemonAPI.Const.engineReady else { break }
                do {
                    switch list {
                    case .images:
                        counters.imageFetches += 1
                        images = try await docker.images()
                    case .volumes:
                        counters.volumeFetches += 1
                        volumes = try await docker.volumes()
                    case .networks:
                        counters.networkFetches += 1
                        networks = try await docker.networks()
                    case .builds:
                        counters.buildFetches += 1
                        do {
                            builds = try await buildHistory.list()
                            buildsError = nil
                            tagBuilds()
                            followRunningBuild()
                        } catch {
                            buildsError = String(describing: error)
                        }
                    }
                    listSucceeded(.list(list))
                } catch {
                    listFailed(.list(list), error)
                }
            } while dirty.contains(list)
            refreshing.remove(list)
        }
    }

    public var docker: DockerClient { DockerClient(socketPath: DaemonAPI.dockerSocketPath(home: place.home)) }

    func withClient<T>(starting: Bool, _ body: (DaemonClient) async throws -> T) async throws -> T {
        if let client { return try await body(client) }
        if starting {
            do {
                try FileManager.default.removeItem(atPath: DaemonAPI.stopMarkPath(home: place.home))
            } catch let e as CocoaError where e.code == .fileNoSuchFile {}
        }
        let own = try await connect(mayStart: starting)
        defer {
            own.close()
            if isActive {
                if session == nil { activate() }
            } else {
                link = .idle
            }
        }
        return try await body(own)
    }

    public func startService() async {
        lastError = nil
        lastInfo = nil
        do {
            status = try await withClient(starting: true) { try await $0.call(DaemonAPI.Methods.vmStart, .init()) }
        } catch {
            report(error, "Fevom could not start")
        }
    }

    public func stopService(force: Bool = false) async {
        lastError = nil
        lastInfo = nil
        let mark = DaemonAPI.stopMarkPath(home: place.home)
        let had = FileManager.default.fileExists(atPath: mark)
        let began = clock_gettime_nsec_np(CLOCK_UPTIME_RAW)
        do {
            try FileManager.default.createDirectory(atPath: place.home, withIntermediateDirectories: true)
            if !had, !FileManager.default.createFile(atPath: mark, contents: nil) {
                throw RPCConnection.Failure.protocolError("the mark that keeps docker commands from starting fev could not be left at \(mark)")
            }
        } catch {
            lastError = String(describing: error)
            return
        }
        let own: DaemonClient
        do {
            own = try await DaemonClient.connect(home: place.home, build: place.build)
        } catch let e as RPCConnection.Failure where Self.notThere(e) {
            if client == nil, isActive {
                link = .stoppedByUser
                status = nil
            }
            return
        } catch let e as DaemonError where Self.replaceable(e) != nil {
            let other = Self.replaceable(e)!
            if let why = await stopOther(other) {
                lastError = why
                return
            }
            if !FileManager.default.fileExists(atPath: mark), !FileManager.default.createFile(atPath: mark, contents: nil) {
                lastError = "Fevom stopped, but could not leave the mark that keeps docker commands from starting it again (\(mark))"
            }
            lastInfo = "Stopped fev of another build (\(other.daemonBuild), pid \(other.pid)); this app is build \(place.build)."
            if client == nil, isActive {
                link = .stoppedByUser
                status = nil
            }
            return
        } catch {
            if !had { try? FileManager.default.removeItem(atPath: mark) }
            lastError = String(describing: error)
            return
        }
        defer { own.close() }
        func gone(_ e: any Error) -> Bool {
            if let d = e as? DaemonError { return d.code == DaemonAPI.Const.codeShuttingDown }
            return (e as? RPCConnection.Failure) == .closed
        }
        do {
            do {
                _ = try await own.call(DaemonAPI.Methods.vmStop, .init(force: force, stopBeganNS: began))
            } catch where gone(error) {}
            let res: DaemonAPI.ShutdownResult
            do {
                res = try await own.call(DaemonAPI.Methods.daemonShutdown, .init(force: force, stopRequested: true, stopBeganNS: began))
            } catch where gone(error) {
                res = .init()
            }
            if res.superseded {
                lastInfo = "Started again by a request that came after this stop began (fev start, a machine's session, an ssh login)."
                status = try await own.call(DaemonAPI.Methods.daemonStatus)
                return
            }
            for await _ in own.connection.notes {}
            if !FileManager.default.fileExists(atPath: mark), !FileManager.default.createFile(atPath: mark, contents: nil) {
                lastError = "Fevom stopped, but could not leave the mark that keeps docker commands from starting it again (\(mark))"
            }
        } catch {
            lastError = String(describing: error)
        }
    }

    static func replaceable(_ e: DaemonError) -> DaemonAPI.BuildMismatch? {
        guard e.code == DaemonAPI.Const.codeBuildMismatch, let d = e.data(as: DaemonAPI.BuildMismatch.self),
              d.pid > 1, d.pid != Int(getpid()) else { return nil }
        return d
    }

    func stopOther(_ other: DaemonAPI.BuildMismatch) async -> String? {
        let watch: RPCConnection
        do {
            watch = try RPCConnection.connect(path: DaemonAPI.socketPath(home: place.home))
        } catch {
            return nil
        }
        defer { watch.close() }
        let rc = terminate(pid_t(other.pid))
        if rc != 0 && rc != ESRCH {
            return "the running daemon (build \(other.daemonBuild), pid \(other.pid)) could not be asked to stop: \(String(cString: strerror(rc)))"
        }
        let bound = otherBuildStopBound
        let gone = await withTaskGroup(of: Bool.self) { g in
            g.addTask {
                for await _ in watch.notes {}
                return true
            }
            g.addTask {
                try? await Task.sleep(for: bound)
                return false
            }
            let first = await g.next() ?? false
            g.cancelAll()
            watch.close()
            return first
        }
        return gone ? nil : "the running daemon (build \(other.daemonBuild), pid \(other.pid)) was asked to stop and had not gone after \(bound)"
    }

    public func restartService() async {
        lastError = nil
        lastInfo = nil
        do {
            status = try await withClient(starting: true) { try await $0.call(DaemonAPI.Methods.vmRestart, .init()) }
        } catch {
            report(error, "Fevom could not restart")
        }
    }

    public func machine(_ name: String, start: Bool) async {
        lastError = nil
        do {
            let m = try await withClient(starting: start) { c in
                start
                    ? try await c.call(DaemonAPI.Methods.machinesStart, .init(name: name))
                    : try await c.call(DaemonAPI.Methods.machinesStop, .init(name: name))
            }
            applyMachine(.init(machine: m))
        } catch {
            report(error, start ? "\(name) could not start" : "\(name) could not stop")
        }
    }

    public func restartMachine(_ name: String) async {
        await run { c in self.applyMachine(.init(machine: try await c.call(DaemonAPI.Methods.machinesRestart, .init(name: name)))) }
    }

    public func deleteMachine(_ name: String) async {
        await run { c in
            _ = try await c.call(DaemonAPI.Methods.machinesDelete, .init(name: name))
            self.machines.removeAll { $0.name == name }
        }
    }

    public func renameMachine(_ name: String, to newName: String) async {
        await run { c in
            let m = try await c.call(DaemonAPI.Methods.machinesRename, .init(name: name, newName: newName))
            self.machines.removeAll { $0.name == name }
            self.applyMachine(.init(machine: m))
        }
    }

    public func cloneMachine(_ name: String, to newName: String) async {
        await run { c in self.applyMachine(.init(machine: try await c.call(DaemonAPI.Methods.machinesClone, .init(name: name, newName: newName)))) }
    }

    public func setDefaultMachine(_ name: String) async {
        await run { c in
            let list = try await c.call(DaemonAPI.Methods.machinesSetDefault, .init(name: name))
            self.machines = list.machines
            self.defaultMachine = list.default
        }
    }

    @discardableResult
    public func createMachine(_ p: DaemonAPI.MachinesCreateParams) async -> DaemonAPI.Operation? {
        await startOperation { try await $0.call(DaemonAPI.Methods.machinesCreate, p) }
    }

    @discardableResult
    public func exportMachine(_ name: String, to path: String) async -> DaemonAPI.Operation? {
        await startOperation { try await $0.call(DaemonAPI.Methods.machinesExport, .init(name: name, path: path)) }
    }

    @discardableResult
    public func importMachine(from path: String, name: String) async -> DaemonAPI.Operation? {
        await startOperation { try await $0.call(DaemonAPI.Methods.machinesImport, .init(path: path, name: name)) }
    }

    @discardableResult
    public func cloneVolume(_ name: String, to newName: String) async -> DaemonAPI.Volume? {
        var made: DaemonAPI.Volume?
        await run { c in made = try await c.call(DaemonAPI.Methods.volumesClone, .init(name: name, newName: newName)) }
        if let made { lastInfo = "Cloned \(name) to \(made.name)" }
        return made
    }

    @discardableResult
    public func exportVolume(_ name: String, to path: String, replace: Bool = false) async -> DaemonAPI.Operation? {
        await startOperation { try await $0.call(DaemonAPI.Methods.volumesExport, .init(name: name, path: path, replace: replace)) }
    }

    @discardableResult
    public func importVolume(from path: String, name: String = "") async -> DaemonAPI.Operation? {
        await startOperation { try await $0.call(DaemonAPI.Methods.volumesImport, .init(path: path, name: name)) }
    }

    public func cancelOperation(_ id: String) async {
        await run { c in self.applyOperation(try await c.call(DaemonAPI.Methods.operationCancel, .init(id: id))) }
    }

    func startOperation(_ call: (DaemonClient) async throws -> DaemonAPI.Operation) async -> DaemonAPI.Operation? {
        lastError = nil
        show()
        defer { hide() }
        do {
            let c = try await viewersClient()
            let op = try await call(c)
            began(op)
            return op
        } catch {
            lastError = String(describing: error)
            return nil
        }
    }

    private func viewersClient() async throws -> DaemonClient {
        if session == nil, client == nil { activate() }
        if link == .connecting || link == .startingDaemon || (client == nil && session != nil) {
            await withCheckedContinuation { linkWaiters.append($0) }
        }
        if let client, link == .connected { return client }
        throw RPCConnection.Failure.protocolError("Fevom is not running (\(ServiceLine.describe(link: link, status: status)))")
    }

    private func run(_ body: (DaemonClient) async throws -> Void) async {
        lastError = nil
        do {
            try await withClient(starting: false) { try await body($0) }
        } catch {
            lastError = String(describing: error)
        }
    }

    public func container(_ id: String, _ action: String) async {
        lastError = nil
        do {
            try await docker.containerAction(id, action)
        } catch {
            report(error, "Could not \(action) a container")
        }
    }

    public func removeContainer(_ id: String, force: Bool) async {
        lastError = nil
        do {
            try await docker.removeContainer(id, force: force)
        } catch {
            lastError = String(describing: error)
        }
    }

    public func project(_ name: String, _ action: String) async {
        lastError = nil
        var members = containers.filter { $0.composeProject == name }.sorted { $0.created < $1.created }
        if action != "start" { members.reverse() }
        var failures: [String] = []
        for c in members {
            do {
                switch action {
                case "delete": try await docker.removeContainer(c.id, force: true)
                case "start" where c.state == "running", "stop" where c.state != "running" && c.state != "paused": continue
                default: try await docker.containerAction(c.id, action)
                }
            } catch {
                failures.append("\(c.name): \(error)")
            }
        }
        if !failures.isEmpty {
            lastError = failures.joined(separator: "; ")
            if !isActive {
                counters.unseenFailures += 1
                notify(AppNotice(title: "Could not \(action) \(name)", body: lastError ?? ""))
            }
        }
    }

    public func removeImage(_ ref: String, force: Bool) async {
        lastError = nil
        do { try await docker.removeImage(ref, force: force) } catch { lastError = String(describing: error) }
    }

    public func pruneImages() async -> String? {
        lastError = nil
        do { return try await docker.pruneImages() } catch { lastError = String(describing: error); return nil }
    }

    public func createVolume(_ name: String) async {
        lastError = nil
        do { try await docker.createVolume(name) } catch { lastError = String(describing: error) }
    }

    public func removeVolume(_ name: String) async {
        lastError = nil
        do { try await docker.removeVolume(name) } catch { lastError = String(describing: error) }
    }

    @discardableResult
    public func exportImage(_ ref: String, to url: URL) async -> Int64? {
        lastError = nil
        do {
            let n = try await docker.saveImages([ref], to: url)
            lastInfo = "Exported \(ref) to \(url.path) (\(ByteCountFormatter.string(fromByteCount: n, countStyle: .file)))"
            return n
        } catch {
            lastError = String(describing: error)
            return nil
        }
    }

    @discardableResult
    public func importImages(from url: URL) async -> [String]? {
        lastError = nil
        do {
            let loaded = try await docker.loadImages(from: url)
            lastInfo = loaded.isEmpty ? "Imported \(url.lastPathComponent)" : loaded.joined(separator: "; ")
            return loaded
        } catch {
            lastError = String(describing: error)
            return nil
        }
    }

    public func load(status: DaemonAPI.Status?, machines: [DaemonAPI.Machine], default def: String = "", containers: [Container]? = nil) {
        self.status = status
        self.machines = machines
        self.defaultMachine = def
        if let containers { self.containers = containers }
    }
}

public enum ServiceLine {
    @MainActor public static func describe(link: AppModel.Link, status: DaemonAPI.Status?) -> String {
        switch link {
        case .idle: return "Idle"
        case .connecting: return "Connecting…"
        case .startingDaemon: return "Starting fev…"
        case .stoppedByUser: return "Stopped"
        case .failed(let why): return "Not running: \(why)"
        case .connected: break
        }
        guard let s = status else { return "Connected" }
        switch s.state {
        case DaemonAPI.Const.stateStarting: return "Starting (\(s.phase))"
        case DaemonAPI.Const.stateRunning: return s.engine == DaemonAPI.Const.engineReady ? "Running" : "Running (engine \(s.engine))"
        case DaemonAPI.Const.statePaused: return "Running (paused for sleep)"
        case DaemonAPI.Const.stateStopped: return s.error.isEmpty ? "Stopped" : "Stopped: \(s.error)"
        default: return s.state.capitalized
        }
    }
}
