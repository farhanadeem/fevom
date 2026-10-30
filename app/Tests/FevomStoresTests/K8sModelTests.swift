import Foundation
import Testing

@testable import FevomAPI
@testable import FevomStores

@MainActor
@Suite struct K8sModelTests {
    func model() -> AppModel {
        AppModel(place: DaemonPlace(home: "/nonexistent-fev-home", build: "test", fevd: nil))
    }

    static func pod(_ ns: String, _ name: String, ready: Bool = true) -> DaemonAPI.K8sObject {
        .init(kind: DaemonAPI.Const.k8sKindPod, namespace: ns, name: name, resourceVersion: "1",
              pod: .init(phase: "Running", ready: ready, ip: "198.19.16.9", containers: [.init(name: "web", image: "web:dev", ready: ready, state: "running")]))
    }

    static func deployment(_ ns: String, _ name: String, wanted: Int = 2) -> DaemonAPI.K8sObject {
        .init(kind: DaemonAPI.Const.k8sKindDeployment, namespace: ns, name: name, workload: .init(wanted: wanted, ready: wanted, images: ["web:dev"]))
    }

    @Test func theWatchsChangesMakeTheLists() throws {
        let m = model()
        m.k8s.subscription = 7
        m.k8s.take([.init(kind: DaemonAPI.Const.k8sKindNamespace, name: "default"), .init(kind: DaemonAPI.Const.k8sKindNamespace, name: "kube-system"),
                    Self.deployment("default", "web"), Self.pod("default", "web-1"), Self.deployment("kube-system", "coredns", wanted: 1)])
        #expect(m.k8s.namespaces == ["default", "kube-system"])
        #expect(m.k8s.list(DaemonAPI.Const.k8sKindDeployment).map(\.name) == ["web", "coredns"])
        m.apply(try note(DaemonAPI.Methods.k8sChanged, .init(subscription: 7, type: DaemonAPI.Const.k8sChangeAdded, object: Self.pod("default", "web-2"))))
        #expect(m.k8s.pods(of: Self.deployment("default", "web")).map(\.name) == ["web-1", "web-2"])
        m.apply(try note(DaemonAPI.Methods.k8sChanged, .init(subscription: 6, type: DaemonAPI.Const.k8sChangeAdded, object: Self.pod("default", "stale"))))
        #expect(m.k8s.objects[K8sModel.key(Self.pod("default", "stale"))] == nil)
        m.apply(try note(DaemonAPI.Methods.k8sChanged, .init(subscription: 7, type: DaemonAPI.Const.k8sChangeModified, object: Self.deployment("default", "web", wanted: 3))))
        #expect(m.k8s.list(DaemonAPI.Const.k8sKindDeployment).first?.workload?.wanted == 3)
        m.apply(try note(DaemonAPI.Methods.k8sChanged, .init(subscription: 7, type: DaemonAPI.Const.k8sChangeDeleted, object: Self.pod("default", "web-1"))))
        #expect(m.k8s.list(DaemonAPI.Const.k8sKindPod).map(\.name) == ["web-2"])
        #expect(m.k8s.counters.changes == 3)
        m.k8s.namespace = "kube-system"
        #expect(m.k8s.list(DaemonAPI.Const.k8sKindDeployment).map(\.name) == ["coredns"])
        #expect(m.k8s.list(DaemonAPI.Const.k8sKindPod).isEmpty)
        m.k8s.take([Self.pod("default", "web-2")])
        #expect(m.k8s.namespace == "")
        m.k8s.connectionEnded()
        #expect(m.k8s.subscription == nil)
    }

    @Test func theFirstStartsDownloadIsShownAsItGoes() {
        var op = DaemonAPI.Operation(id: "op-1", method: DaemonAPI.Const.methodK8sEnable, target: "kubernetes", state: DaemonAPI.Const.operationRunning,
                                     phase: DaemonAPI.Const.k8sPhaseFetching, doneBytes: 84_017_152, totalBytes: 250_217_791)
        #expect(K8sModel.progressText(op) == "Downloading Kubernetes: 84 of 250 MB")
        #expect(abs((K8sModel.fraction(op) ?? 0) - 0.336) < 0.001)
        op.phase = DaemonAPI.Const.k8sPhaseImporting
        #expect(K8sModel.progressText(op) == "Importing the images")
        #expect(K8sModel.fraction(op) == nil)
        let m = model()
        m.began(op)
        #expect(m.k8sOperation?.id == "op-1")
        var end = op
        end.state = DaemonAPI.Const.operationDone
        m.applyOperation(end)
        #expect(m.k8sOperation == nil)
        #expect(AppModel.notice(end).title == "Kubernetes is running")
        end.method = DaemonAPI.Const.methodK8sReset
        end.state = DaemonAPI.Const.operationFailed
        end.error = "the guest said no"
        #expect(AppModel.notice(end).title == "Failed resetting Kubernetes")
    }

    @Test func nothingIsAskedWhileNotShownOrNotConnected() {
        let m = model()
        m.watchK8s()
        #expect(m.k8s.watched)
        #expect(m.k8s.counters == K8sModel.Counters())
        m.unwatchK8s()
        #expect(!m.k8s.watched && m.k8s.subscription == nil)
        m.watchK8s()
        m.mainWindowClosed()
        #expect(!m.k8s.watched)
        #expect(m.k8s.counters.watches == 0 && m.k8s.counters.statusFetches == 0)
    }

    @Test func theClustersStateLines() {
        let k = K8sModel()
        #expect(!k.on && k.kubeconfigLine.isEmpty)
        k.status = .init(enabled: true, state: DaemonAPI.Const.k8sStateRunning, ready: true,
                         kubeconfig: .init(path: FileManager.default.homeDirectoryForCurrentUser.path + "/.kube/config", context: "fev", present: true, current: true))
        #expect(k.on)
        #expect(k.kubeconfigLine == "Context fev in ~/.kube/config (current)")
        k.status?.kubeconfig.present = false
        #expect(k.kubeconfigLine == "Context fev is not in ~/.kube/config")
        let svc = DaemonAPI.K8sObject(kind: DaemonAPI.Const.k8sKindService, namespace: "default", name: "web",
                                      service: .init(type: "LoadBalancer", loadBalancer: "198.19.8.1", urLs: ["https://web.k8s.fev.local", "http://localhost:30080"]))
        #expect(K8sModel.urls(svc).first == "https://web.k8s.fev.local")
        #expect(K8sModel.podState(Self.pod("default", "p", ready: false)) == "Running")
        #expect(K8sModel.podState(.init(kind: "Pod", name: "q", pod: .init(phase: "Pending", reason: "ErrImageNeverPull"))) == "ErrImageNeverPull")
    }

    @Test func anApplyTakesFilesAndAFoldersManifests() throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("k8s-docs-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: dir.appendingPathComponent("sub"), withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: dir) }
        try "kind: Service".write(to: dir.appendingPathComponent("b.yaml"), atomically: true, encoding: .utf8)
        try "kind: Deployment".write(to: dir.appendingPathComponent("a.yml"), atomically: true, encoding: .utf8)
        try "{}".write(to: dir.appendingPathComponent("c.json"), atomically: true, encoding: .utf8)
        try "not one".write(to: dir.appendingPathComponent("README.md"), atomically: true, encoding: .utf8)
        try "kind: Pod".write(to: dir.appendingPathComponent("sub/d.yaml"), atomically: true, encoding: .utf8)
        let one = dir.appendingPathComponent("sub/d.yaml")
        let docs = K8sModel.documents([dir, one])
        #expect(docs.map { ($0.source as NSString).lastPathComponent } == ["a.yml", "b.yaml", "c.json", "d.yaml"])
        #expect(docs.first?.yaml == "kind: Deployment")
    }
}
