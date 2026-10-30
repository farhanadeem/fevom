import FevomAPI
import Foundation
import Observation

@MainActor @Observable
public final class K8sModel {
    public internal(set) var status: DaemonAPI.K8sStatus?
    public internal(set) var objects: [String: DaemonAPI.K8sObject] = [:]
    public var namespace = ""
    public internal(set) var subscription: UInt64?
    var subscribing = false
    var viewers = 0
    public internal(set) var applied: [DaemonAPI.K8sApplied] = []

    public struct Counters: Sendable, Equatable {
        public var watches = 0
        public var unwatches = 0
        public var statusFetches = 0
        public var changes = 0
    }
    public internal(set) var counters = Counters()

    public init() {}

    nonisolated public static func key(_ o: DaemonAPI.K8sObject) -> String { "\(o.kind)/\(o.namespace)/\(o.name)" }

    public var watched: Bool { viewers > 0 }

    public var on: Bool {
        guard let s = status else { return false }
        return s.enabled || s.state == DaemonAPI.Const.k8sStateRunning || s.state == DaemonAPI.Const.k8sStateStarting
    }

    public var namespaces: [String] {
        var names = Set(objects.values.filter { $0.kind == DaemonAPI.Const.k8sKindNamespace }.map(\.name))
        for o in objects.values where !o.namespace.isEmpty { names.insert(o.namespace) }
        return names.sorted()
    }

    public func list(_ kind: String) -> [DaemonAPI.K8sObject] {
        objects.values
            .filter { $0.kind == kind && (namespace.isEmpty || $0.namespace == namespace) }
            .sorted { ($0.namespace, $0.name) < ($1.namespace, $1.name) }
    }

    public func pods(of deployment: DaemonAPI.K8sObject) -> [DaemonAPI.K8sObject] {
        list(DaemonAPI.Const.k8sKindPod).filter { $0.namespace == deployment.namespace && $0.name.hasPrefix(deployment.name + "-") }
    }

    func take(_ list: [DaemonAPI.K8sObject]) {
        var next: [String: DaemonAPI.K8sObject] = [:]
        for o in list { next[Self.key(o)] = o }
        objects = next
        if !namespace.isEmpty, !namespaces.contains(namespace) { namespace = "" }
    }

    public func change(_ c: DaemonAPI.K8sChanged) {
        guard c.subscription != 0, c.subscription == subscription || (subscription == nil && subscribing) else { return }
        counters.changes += 1
        let k = Self.key(c.object)
        if c.type == DaemonAPI.Const.k8sChangeDeleted {
            objects[k] = nil
        } else {
            objects[k] = c.object
        }
    }

    func connectionEnded() {
        subscription = nil
        subscribing = false
    }

    public static func progressText(_ op: DaemonAPI.Operation) -> String {
        switch op.phase {
        case DaemonAPI.Const.k8sPhaseFetching where op.totalBytes > 0:
            return "Downloading Kubernetes: \(op.doneBytes / 1_000_000) of \(op.totalBytes / 1_000_000) MB"
        case DaemonAPI.Const.k8sPhaseFetching: return "Downloading Kubernetes"
        case DaemonAPI.Const.k8sPhaseChecking: return "Checking the space and the addresses"
        case DaemonAPI.Const.k8sPhaseVerifying: return "Checking the download and unpacking k3s"
        case DaemonAPI.Const.k8sPhaseImporting: return "Importing the images"
        case DaemonAPI.Const.k8sPhaseStarting: return "Starting the cluster"
        case DaemonAPI.Const.k8sPhaseReady: return "Ready"
        case DaemonAPI.Const.k8sPhaseStopping: return "Stopping the cluster"
        case DaemonAPI.Const.k8sPhaseDeleting: return "Deleting the cluster's data"
        default: return op.phase
        }
    }

    public static func fraction(_ op: DaemonAPI.Operation) -> Double? {
        guard op.phase == DaemonAPI.Const.k8sPhaseFetching, op.totalBytes > 0 else { return nil }
        return min(1, Double(op.doneBytes) / Double(op.totalBytes))
    }

    public static func podState(_ o: DaemonAPI.K8sObject) -> String {
        guard let p = o.pod else { return "" }
        if !p.reason.isEmpty { return p.reason }
        return p.ready ? "\(p.phase), ready" : p.phase
    }

    public static func urls(_ o: DaemonAPI.K8sObject) -> [String] {
        if let s = o.service { return s.urLs }
        if let i = o.ingress { return i.urLs }
        return []
    }

    nonisolated public static func documents(_ urls: [URL]) -> [DaemonAPI.K8sDocument] {
        var out: [DaemonAPI.K8sDocument] = []
        for url in urls {
            var isDir: ObjCBool = false
            FileManager.default.fileExists(atPath: url.path, isDirectory: &isDir)
            let files = isDir.boolValue
                ? ((try? FileManager.default.contentsOfDirectory(at: url, includingPropertiesForKeys: nil)) ?? [])
                    .filter { ["yaml", "yml", "json"].contains($0.pathExtension.lowercased()) }.sorted { $0.path < $1.path }
                : [url]
            for f in files {
                if let s = try? String(contentsOf: f, encoding: .utf8) { out.append(.init(source: f.path, yaml: s)) }
            }
        }
        return out
    }

    public var kubeconfigLine: String {
        guard let k = status?.kubeconfig, !k.path.isEmpty else { return "" }
        let home = FileManager.default.homeDirectoryForCurrentUser.path
        let path = k.path.hasPrefix(home + "/") ? "~" + k.path.dropFirst(home.count) : k.path
        return k.present ? "Context \(k.context) in \(path)\(k.current ? " (current)" : "")" : "Context \(k.context) is not in \(path)"
    }
}

extension AppModel {
    public var k8sOperation: DaemonAPI.Operation? {
        operations.values.first { [DaemonAPI.Const.methodK8sEnable, DaemonAPI.Const.methodK8sDisable, DaemonAPI.Const.methodK8sReset].contains($0.method) }
    }

    public func watchK8s() {
        k8s.viewers += 1
        guard k8s.viewers == 1, mainWindowShown, let client, link == .connected else { return }
        Task { await followK8s(client) }
    }

    public func unwatchK8s() {
        k8s.viewers = max(0, k8s.viewers - 1)
        if k8s.viewers == 0 { endK8sWatch() }
    }

    func endK8sWatch() {
        guard let id = k8s.subscription else { return }
        k8s.subscription = nil
        k8s.counters.unwatches += 1
        if let client {
            Task { _ = try? await client.call(DaemonAPI.Methods.k8sUnwatch, .init(subscription: id)) }
        }
    }

    func followK8s(_ client: DaemonClient) async {
        await fetchK8sStatus(client)
        if k8s.status?.state == DaemonAPI.Const.k8sStateRunning { await subscribeK8s(client) }
    }

    func fetchK8sStatus(_ client: DaemonClient) async {
        k8s.counters.statusFetches += 1
        do {
            k8s.status = try await client.call(DaemonAPI.Methods.k8sStatus)
        } catch {
            lastError = String(describing: error)
        }
    }

    func subscribeK8s(_ client: DaemonClient) async {
        guard k8s.subscription == nil, !k8s.subscribing else { return }
        k8s.subscribing = true
        do {
            let w = try await client.call(DaemonAPI.Methods.k8sWatch, .init())
            k8s.subscribing = false
            k8s.counters.watches += 1
            if k8s.viewers > 0, mainWindowShown, self.client === client {
                k8s.subscription = w.subscription
                k8s.take(w.objects)
            } else {
                k8s.counters.unwatches += 1
                _ = try? await client.call(DaemonAPI.Methods.k8sUnwatch, .init(subscription: w.subscription))
            }
        } catch {
            k8s.subscribing = false
            lastError = String(describing: error)
        }
    }

    func k8sLineChanged() {
        guard k8s.viewers > 0, mainWindowShown, let client, link == .connected else { return }
        Task {
            await fetchK8sStatus(client)
            if k8s.status?.state == DaemonAPI.Const.k8sStateRunning {
                await subscribeK8s(client)
            } else if k8s.subscription != nil {
                endK8sWatch()
                k8s.objects = [:]
            }
        }
    }

    @discardableResult
    public func startK8s() async -> DaemonAPI.Operation? {
        await startOperation { try await $0.call(DaemonAPI.Methods.k8sEnable) }
    }

    @discardableResult
    public func stopK8s() async -> DaemonAPI.Operation? {
        await startOperation { try await $0.call(DaemonAPI.Methods.k8sDisable) }
    }

    @discardableResult
    public func resetK8s(all: Bool) async -> DaemonAPI.Operation? {
        await startOperation { try await $0.call(DaemonAPI.Methods.k8sReset, .init(all: all)) }
    }

    @discardableResult
    public func applyK8s(_ documents: [DaemonAPI.K8sDocument], dryRun: Bool = false) async -> [DaemonAPI.K8sApplied]? {
        lastError = nil
        do {
            let r = try await withClient(starting: false) {
                try await $0.call(DaemonAPI.Methods.k8sApply, .init(documents: documents, namespace: k8s.namespace, dryRun: dryRun))
            }
            k8s.applied = r.objects
            return r.objects
        } catch {
            lastError = String(describing: error)
            return nil
        }
    }

    public func manifestK8s(_ o: DaemonAPI.K8sObject) async -> DaemonAPI.K8sManifest? {
        lastError = nil
        do {
            return try await withClient(starting: false) { try await $0.call(DaemonAPI.Methods.k8sGet, Self.ref(o)) }
        } catch {
            lastError = String(describing: error)
            return nil
        }
    }

    public enum K8sSaved: Equatable, Sendable {
        case saved([DaemonAPI.K8sApplied])
        case conflict(String)
        case failed(String)
    }

    public func updateK8s(_ ref: DaemonAPI.K8sRef, yaml: String) async -> K8sSaved {
        do {
            let r = try await withClient(starting: false) { try await $0.call(DaemonAPI.Methods.k8sUpdate, .init(ref: ref, yaml: yaml)) }
            if let bad = r.objects.first(where: { $0.action == DaemonAPI.Const.k8sActionFailed }) { return .failed(bad.error) }
            return .saved(r.objects)
        } catch let e as DaemonError where e.code == DaemonAPI.Const.codeConflict {
            return .conflict(e.message)
        } catch {
            return .failed(String(describing: error))
        }
    }

    public func scaleK8s(_ o: DaemonAPI.K8sObject, replicas: Int) async {
        await k8sCall { _ = try await $0.call(DaemonAPI.Methods.k8sScale, .init(ref: Self.ref(o), replicas: replicas)) }
    }

    public func restartK8s(_ o: DaemonAPI.K8sObject) async {
        await k8sCall { _ = try await $0.call(DaemonAPI.Methods.k8sRestart, Self.ref(o)) }
    }

    public func deleteK8s(_ o: DaemonAPI.K8sObject) async {
        await k8sCall { _ = try await $0.call(DaemonAPI.Methods.k8sDelete, Self.ref(o)) }
    }

    private func k8sCall(_ body: (DaemonClient) async throws -> Void) async {
        lastError = nil
        do {
            try await withClient(starting: false) { try await body($0) }
        } catch {
            lastError = String(describing: error)
        }
    }

    nonisolated public static func ref(_ o: DaemonAPI.K8sObject) -> DaemonAPI.K8sRef {
        DaemonAPI.K8sRef(kind: o.kind, namespace: o.namespace, name: o.name)
    }
}

extension DaemonAPI.K8sObject: Identifiable {
    public var id: String { K8sModel.key(self) }
}
