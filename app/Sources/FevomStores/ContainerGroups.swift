import FevomDocker
import Foundation

public struct ContainerRow: Identifiable, Hashable, Sendable {
    public enum Kind: Hashable, Sendable {
        case project(String)
        case container(Container)
    }

    public var id: String
    public var kind: Kind
    public var children: [ContainerRow]?

    public var name: String {
        switch kind {
        case .project(let p): p
        case .container(let c): c.composeService.map { s in c.composeProject != nil ? s : c.name } ?? c.name
        }
    }

    public var container: Container? { if case .container(let c) = kind { c } else { nil } }
    public var project: String? { if case .project(let p) = kind { p } else { nil } }

    public var state: String {
        switch kind {
        case .container(let c): return c.state
        case .project:
            let all = children ?? []
            let up = all.filter { $0.container?.state == "running" }.count
            return up == all.count ? "running" : up == 0 ? "exited" : "\(up)/\(all.count) running"
        }
    }

    public static func rows(_ containers: [Container], filter: String = "") -> [ContainerRow] {
        let f = filter.trimmingCharacters(in: .whitespaces)
        func hit(_ c: Container) -> Bool {
            f.isEmpty || [c.name, c.image, c.composeProject ?? "", c.composeService ?? ""].contains { $0.localizedCaseInsensitiveContains(f) }
        }
        var projects: [String: [Container]] = [:]
        var lone: [Container] = []
        for c in containers {
            if let p = c.composeProject { projects[p, default: []].append(c) } else if hit(c) { lone.append(c) }
        }
        var out: [ContainerRow] = []
        for p in projects.keys.sorted() {
            let all = projects[p]!.map { c -> (String, String, Container) in let n = c.name; return (c.composeService ?? n, n, c) }
                .sorted { ($0.0, $0.1) < ($1.0, $1.1) }.map(\.2)
            let shown = (f.isEmpty || p.localizedCaseInsensitiveContains(f)) ? all : all.filter(hit)
            if shown.isEmpty { continue }
            out.append(ContainerRow(id: "project:" + p, kind: .project(p), children: shown.map { ContainerRow(id: $0.id, kind: .container($0), children: nil) }))
        }
        for c in lone.map({ ($0.name, $0) }).sorted(by: { $0.0 < $1.0 }).map(\.1) {
            out.append(ContainerRow(id: c.id, kind: .container(c), children: nil))
        }
        return out
    }
}

public enum MachineCatalog {
    public static let defaults: [(distro: String, release: String)] = [
        ("almalinux", "10"), ("alpine", "3.24"), ("archlinux", "current"), ("centos", "10-Stream"),
        ("debian", "trixie"), ("devuan", "excalibur"), ("fedora", "44"), ("gentoo", "current"),
        ("kali", "current"), ("opensuse", "16.0"), ("oracle", "10"), ("rockylinux", "10"),
        ("ubuntu", "resolute"), ("voidlinux", "current"),
    ]
    public static let architectures = ["arm64", "amd64"]

    public static func displayName(_ distro: String) -> String {
        ["almalinux": "AlmaLinux", "alpine": "Alpine", "archlinux": "Arch Linux", "centos": "CentOS Stream",
         "debian": "Debian", "devuan": "Devuan", "fedora": "Fedora", "gentoo": "Gentoo", "kali": "Kali Linux",
         "opensuse": "openSUSE", "oracle": "Oracle Linux", "rockylinux": "Rocky Linux", "ubuntu": "Ubuntu", "voidlinux": "Void Linux"][distro] ?? distro
    }

    public static func defaultRelease(_ distro: String) -> String? { defaults.first { $0.distro == distro }?.release }
}
