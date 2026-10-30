import FevomDocker
import Foundation
import Observation

@MainActor @Observable
public final class FileBrowser {
    public struct Entry: Sendable, Hashable, Identifiable {
        public var name: String
        public var isDirectory: Bool
        public var isLink: Bool
        public var size: Int64
        public var modified: Date?
        public var id: String { name }
    }

    public let root: URL
    public private(set) var directory: URL
    public private(set) var entries: [Entry] = []
    public private(set) var error: String?
    public private(set) var loading = false
    public var showHidden = false {
        didSet { if showHidden != oldValue { reload() } }
    }
    public private(set) var reads = 0
    private var generation = 0

    public init(root: URL) {
        self.root = root.standardizedFileURL
        directory = self.root
    }

    public var parts: [String] {
        let r = root.pathComponents
        let d = directory.pathComponents
        return d.count > r.count ? Array(d[r.count...]) : []
    }

    public var atRoot: Bool { parts.isEmpty }

    public func go(_ parts: [String]) {
        var u = root
        for p in parts where !p.isEmpty && p != "." {
            if p == ".." || p.contains("/") { u = root; break }
            u.appendPathComponent(p, isDirectory: true)
        }
        directory = u.standardizedFileURL
        reload()
    }

    public func open(_ e: Entry) {
        guard e.isDirectory else { return }
        go(parts + [e.name])
    }

    public func up() {
        guard !atRoot else { return }
        go(Array(parts.dropLast()))
    }

    public func url(_ e: Entry) -> URL { directory.appendingPathComponent(e.name, isDirectory: e.isDirectory) }

    public func openable(_ e: Entry) -> URL? {
        let u = url(e)
        guard e.isLink else { return u }
        let to = u.resolvingSymlinksInPath().standardizedFileURL.path
        let top = root.resolvingSymlinksInPath().standardizedFileURL.path
        return to == top || to.hasPrefix(top + "/") ? u : nil
    }

    public func linkTarget(_ e: Entry) -> String {
        (try? FileManager.default.destinationOfSymbolicLink(atPath: url(e).path)) ?? "?"
    }

    public func reload() {
        generation += 1
        let gen = generation
        let dir = directory
        let hidden = showHidden
        loading = true
        reads += 1
        Task.detached(priority: .userInitiated) {
            let result = Result { try Self.read(dir, hidden: hidden) }
            await MainActor.run { [weak self] in
                guard let self, self.generation == gen else { return }
                self.loading = false
                switch result {
                case .success(let list):
                    self.entries = list
                    self.error = nil
                case .failure(let e):
                    self.entries = []
                    self.error = (e as NSError).localizedDescription
                }
            }
        }
    }

    nonisolated static func read(_ dir: URL, hidden: Bool) throws -> [Entry] {
        let keys: [URLResourceKey] = [.isDirectoryKey, .isSymbolicLinkKey, .fileSizeKey, .contentModificationDateKey]
        let urls = try FileManager.default.contentsOfDirectory(at: dir, includingPropertiesForKeys: keys, options: hidden ? [] : [.skipsHiddenFiles])
        return urls.map { u in
            let v = try? u.resourceValues(forKeys: Set(keys))
            return Entry(name: u.lastPathComponent, isDirectory: v?.isDirectory ?? false, isLink: v?.isSymbolicLink ?? false,
                         size: Int64(v?.fileSize ?? 0), modified: v?.contentModificationDate)
        }.sorted {
            $0.isDirectory != $1.isDirectory ? $0.isDirectory : $0.name.localizedStandardCompare($1.name) == .orderedAscending
        }
    }

    public nonisolated static func machine(_ name: String) -> [String] { [name] }
    public nonisolated static func volume(_ name: String) -> [String] { ["docker", "volumes", name] }
    public nonisolated static func container(_ c: Container) -> [String] { ["docker", "containers", c.name] }
    public nonisolated static func image(_ i: DockerImage) -> [String] { ["docker", "images", i.name.replacingOccurrences(of: "/", with: "_")] }
}
