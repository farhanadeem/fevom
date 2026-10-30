import FevomAPI
import FevomDocker
import Foundation

extension AppModel {

    func report(_ error: any Error, _ title: String) {
        lastError = String(describing: error)
        if !isActive {
            counters.unseenFailures += 1
            notify(AppNotice(title: title, body: String(describing: error)))
        }
    }

    func followViews(_ client: DaemonClient) async {
        if statsViewers > 0, mainWindowShown { await subscribeStats(client) }
        if configViewers > 0 { await fetchConfig(client) }
        if forwardsViewers > 0 { await fetchForwards(client) }
        if k8s.viewers > 0, mainWindowShown { await followK8s(client) }
    }

    public func watchStats() {
        statsViewers += 1
        guard statsViewers == 1, mainWindowShown, let client, link == .connected else { return }
        Task { await subscribeStats(client) }
    }

    public func mainWindow(shown: Bool) {
        guard shown != mainWindowShown else { return }
        mainWindowShown = shown
        if shown {
            if statsViewers > 0, let client, link == .connected { Task { await subscribeStats(client) } }
            if k8s.viewers > 0, let client, link == .connected { Task { await followK8s(client) } }
        } else {
            endStats()
            endK8sWatch()
        }
    }

    public func mainWindowClosed() {
        statsViewers = 0
        k8s.viewers = 0
        mainWindow(shown: false)
    }

    private func endStats() {
        guard let id = statsSubscription else { return }
        statsSubscription = nil
        counters.statsUnsubscribes += 1
        if let client {
            Task { _ = try? await client.call(DaemonAPI.Methods.statsUnsubscribe, .init(subscription: id)) }
        }
    }

    public func unwatchStats() {
        statsViewers = max(0, statsViewers - 1)
        guard statsViewers == 0, let id = statsSubscription else { return }
        statsSubscription = nil
        counters.statsUnsubscribes += 1
        if let client {
            Task { _ = try? await client.call(DaemonAPI.Methods.statsUnsubscribe, .init(subscription: id)) }
        }
    }

    func subscribeStats(_ client: DaemonClient) async {
        guard statsSubscription == nil, !statsSubscribing else { return }
        statsSubscribing = true
        activity.reset()
        do {
            let sub = try await client.call(DaemonAPI.Methods.statsSubscribe, .init())
            statsSubscribing = false
            counters.statsSubscribes += 1
            if statsViewers > 0, mainWindowShown, self.client === client {
                statsSubscription = sub.subscription
            } else {
                counters.statsUnsubscribes += 1
                _ = try? await client.call(DaemonAPI.Methods.statsUnsubscribe, sub)
            }
        } catch {
            statsSubscribing = false
            lastError = String(describing: error)
        }
    }

    func applyStats(_ s: DaemonAPI.StatsSample) {
        guard statsViewers > 0, mainWindowShown, s.subscription == statsSubscription || (statsSubscription == nil && statsSubscribing) else { return }
        counters.statsSamples += 1
        activity.ingest(s)
    }

    public func watchConfig() {
        configViewers += 1
        guard configViewers == 1, let client, link == .connected else { return }
        Task { await fetchConfig(client) }
    }

    public func unwatchConfig() {
        configViewers = max(0, configViewers - 1)
    }

    func fetchConfig(_ client: DaemonClient) async {
        counters.configFetches += 1
        do {
            async let keys = client.call(DaemonAPI.Methods.configDescribe)
            async let values = client.call(DaemonAPI.Methods.configList)
            configKeys = try await keys.keys
            configValues = try await values.values
        } catch {
            lastError = String(describing: error)
        }
    }

    func applyConfig(_ c: DaemonAPI.ConfigChanged) {
        guard configViewers > 0 else { return }
        let def = configKeys.first { $0.key == c.key }?.default
        configValues[c.key] = DaemonAPI.ConfigValue(value: c.value, isDefault: def == c.value, pending: c.pending)
    }

    @discardableResult
    public func setConfig(_ key: String, _ value: JSONValue) async -> [DaemonAPI.ConfigChange]? {
        lastError = nil
        do {
            let r = try await withClient(starting: false) { try await $0.call(DaemonAPI.Methods.configSet, .init(values: [key: value])) }
            took(r.changed)
            return r.changed
        } catch {
            lastError = Self.configFailure(error)
            return nil
        }
    }

    @discardableResult
    public func resetConfig(_ key: String) async -> [DaemonAPI.ConfigChange]? {
        lastError = nil
        do {
            let r = try await withClient(starting: false) { try await $0.call(DaemonAPI.Methods.configReset, .init(keys: [key])) }
            took(r.changed)
            if var v = configValues[key] { v.isDefault = true; configValues[key] = v }
            return r.changed
        } catch {
            lastError = Self.configFailure(error)
            return nil
        }
    }

    @discardableResult
    public func changeList(_ key: String, add: [String] = [], remove: [String] = []) async -> Bool {
        lastError = nil
        do {
            try await withClient(starting: false) { c in
                if !add.isEmpty { self.took(try await c.call(DaemonAPI.Methods.configAdd, .init(key: key, values: add)).changed) }
                if !remove.isEmpty { self.took(try await c.call(DaemonAPI.Methods.configRemove, .init(key: key, values: remove)).changed) }
            }
            return true
        } catch {
            lastError = Self.configFailure(error)
            return false
        }
    }

    private func took(_ changed: [DaemonAPI.ConfigChange]) {
        for c in changed {
            let def = configKeys.first { $0.key == c.key }?.default
            configValues[c.key] = DaemonAPI.ConfigValue(value: c.value, isDefault: def == c.value, pending: c.pending)
        }
    }

    static func configFailure(_ error: any Error) -> String {
        if let e = error as? DaemonError, let d = e.data(as: DaemonAPI.ConfigError.self) {
            return "\(d.key): \(e.message)"
        }
        return String(describing: error)
    }

    public var pendingRestart: [String] {
        configValues.filter { $0.value.pending }.map(\.key).sorted()
    }

    public static func configCommand(_ key: String, _ value: JSONValue?) -> String {
        guard let value else { return "fev config reset \(key)" }
        return "fev config set \(key) \(shellWord(Self.text(value)))"
    }

    public static func text(_ v: JSONValue) -> String {
        switch v {
        case .null: return ""
        case .bool(let b): return b ? "true" : "false"
        case .int(let i): return String(i)
        case .uint(let u): return String(u)
        case .double(let d): return String(d)
        case .string(let s): return s
        case .array(let a): return a.map(text).joined(separator: ",")
        case .object: return ""
        }
    }

    static func shellWord(_ s: String) -> String {
        if !s.isEmpty, s.allSatisfy({ $0.isLetter || $0.isNumber || "-_./:,@%+=".contains($0) }) { return s }
        return "'" + s.replacingOccurrences(of: "'", with: "'\\''") + "'"
    }

    public struct EngineConfig: Sendable, Equatable {
        public var path: String
        public var text: String
        public var state: String
        public var error: String
    }

    public func loadEngineConfig() async -> EngineConfig? {
        lastError = nil
        do {
            let ds = try await withClient(starting: false) { try await $0.call(DaemonAPI.Methods.dockerStatus) }
            guard !ds.configFile.isEmpty else {
                lastError = "Fevom has no engine configuration file"
                return nil
            }
            let text = (try? String(contentsOfFile: ds.configFile, encoding: .utf8)) ?? ""
            return EngineConfig(path: ds.configFile, text: text, state: ds.configState, error: ds.configError)
        } catch {
            lastError = String(describing: error)
            return nil
        }
    }

    public static func engineConfigProblem(_ text: String) -> String? {
        let data = Data(text.utf8)
        if data.count > 1 << 20 { return "it is \(data.count) bytes; an engine configuration is at most \(1 << 20)" }
        if text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty { return nil }
        do {
            let v = try JSONSerialization.jsonObject(with: data, options: [.fragmentsAllowed])
            if v is NSNull { return "it must hold a JSON object, not null" }
            if !(v is [String: Any]) { return "it must hold a JSON object" }
            return nil
        } catch {
            return "it is not valid JSON: \((error as NSError).userInfo[NSDebugDescriptionErrorKey] as? String ?? error.localizedDescription)"
        }
    }

    public func saveEngineConfig(_ text: String, path: String) async -> String? {
        lastError = nil
        if let problem = Self.engineConfigProblem(text) {
            lastError = "\(path): \(problem). Nothing was saved."
            return nil
        }
        do {
            try Data(text.utf8).write(to: URL(fileURLWithPath: path), options: .atomic)
        } catch {
            lastError = "Could not save \(path): \(error.localizedDescription)"
            return nil
        }
        do {
            return try await withClient(starting: false) { c in
                let st = try await c.call(DaemonAPI.Methods.daemonStatus)
                guard st.state == DaemonAPI.Const.stateRunning || st.state == DaemonAPI.Const.statePaused else {
                    return "Saved. It applies the next time the Docker engine starts."
                }
                let ds = try await c.call(DaemonAPI.Methods.dockerRestart)
                if ds.configState == DaemonAPI.Const.engineConfigRefused {
                    throw RPCConnection.Failure.protocolError("The Docker engine refused \(path): \(ds.configError). It keeps its last working configuration.")
                }
                return "Saved; the Docker engine restarted with it."
            }
        } catch {
            lastError = String(describing: error)
            return nil
        }
    }

    @discardableResult
    public func trustHTTPS(_ action: String) async -> DaemonAPI.HTTPSStatus? {
        lastError = nil
        do {
            return try await withClient(starting: false) { try await $0.call(DaemonAPI.Methods.httpsTrust, .init(action: action)) }
        } catch {
            lastError = String(describing: error)
            return nil
        }
    }

    public func watchForwards() {
        forwardsViewers += 1
        guard forwardsViewers == 1, let client, link == .connected else { return }
        Task { await fetchForwards(client) }
    }

    public func unwatchForwards() {
        forwardsViewers = max(0, forwardsViewers - 1)
    }

    func fetchForwards(_ client: DaemonClient) async {
        counters.forwardsFetches += 1
        do {
            forwards = try await client.call(DaemonAPI.Methods.forwardsList).forwards
        } catch {
            lastError = String(describing: error)
        }
    }

    func applyForwards(_ f: DaemonAPI.ForwardsChanged) {
        guard forwardsViewers > 0 else { return }
        forwards.removeAll { $0.id == f.forward.id }
        if !f.removed { forwards.append(f.forward) }
    }

    public func loadConfig(keys: [DaemonAPI.ConfigKey], values: [String: DaemonAPI.ConfigValue]) {
        configKeys = keys
        configValues = values
    }
}

extension AppModel {

    public func loadDebugStatus() async {
        do {
            debug = try await withClient(starting: false) { try await $0.call(DaemonAPI.Methods.debugStatus) }
        } catch {
            debug = nil
        }
    }

    @discardableResult
    public func resetDebugStore() async -> Int64? {
        lastError = nil
        do {
            let r = try await withClient(starting: false) { try await $0.call(DaemonAPI.Methods.debugReset, .init(force: false)) }
            await loadDebugStatus()
            lastInfo = "Debug tools reset; \(ByteCountFormatter.string(fromByteCount: r.freedBytes, countStyle: .file)) freed."
            return r.freedBytes
        } catch {
            lastError = String(describing: error)
            return nil
        }
    }
}
