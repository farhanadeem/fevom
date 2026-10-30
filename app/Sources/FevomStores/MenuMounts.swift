import FevomAPI
import FevomDocker
import Foundation

public struct MenuMount: Sendable, Equatable {
    public var title: String
    public var path: String
    public init(title: String, path: String) {
        self.title = title
        self.path = path
    }
}

public enum MenuMounts {
    public static func items(view: DaemonAPI.FilesView?, machines: [DaemonAPI.Machine], containers: [Container], limit: Int = 20) -> [MenuMount] {
        guard let v = view, !v.path.isEmpty else { return [] }
        let shown = (v.path as NSString).abbreviatingWithTildeInPath
        guard v.state == DaemonAPI.Const.viewMounted else {
            let why = v.error.isEmpty ? v.state : "\(v.state): \(v.error)"
            return [MenuMount(title: "\(shown) (\(why))", path: "")]
        }
        var out = [MenuMount(title: shown, path: v.path)]
        for m in machines.sorted(by: { $0.name < $1.name }) where m.state == DaemonAPI.Const.machineRunning {
            out.append(MenuMount(title: m.name, path: v.path + "/" + FileBrowser.machine(m.name).joined(separator: "/")))
        }
        for c in containers.filter({ $0.state == "running" }).sorted(by: { $0.name < $1.name }).prefix(limit) {
            out.append(MenuMount(title: c.name, path: v.path + "/" + FileBrowser.container(c).joined(separator: "/")))
        }
        return out
    }
}
