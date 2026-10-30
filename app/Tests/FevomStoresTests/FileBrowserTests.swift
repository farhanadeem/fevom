import Foundation
import Testing

@testable import FevomDocker
@testable import FevomStores

@MainActor
@Suite struct FileBrowserTests {
    func tree() throws -> URL {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent("fev-files-\(UUID().uuidString.prefix(8))/Fevom")
        let fm = FileManager.default
        try fm.createDirectory(at: root.appendingPathComponent("docker/volumes/data"), withIntermediateDirectories: true)
        try fm.createDirectory(at: root.appendingPathComponent("dev/etc"), withIntermediateDirectories: true)
        try Data("fev is running\n".utf8).write(to: root.appendingPathComponent("README.txt"))
        try Data("x".utf8).write(to: root.appendingPathComponent(".hidden"))
        try Data(count: 1234).write(to: root.appendingPathComponent("docker/volumes/data/blob"))
        try fm.createSymbolicLink(atPath: root.appendingPathComponent("dev/etc/localtime").path, withDestinationPath: "/usr/share/zoneinfo/UTC")
        return root
    }

    func settle(_ b: FileBrowser) async throws {
        try await until { !b.loading }
    }

    @Test func aFolderAtATimeUnderTheRoot() async throws {
        let root = try tree()
        defer { try? FileManager.default.removeItem(at: root.deletingLastPathComponent()) }
        let b = FileBrowser(root: root)
        b.reload()
        try await settle(b)
        #expect(b.entries.map(\.name) == ["dev", "docker", "README.txt"], "folders first, no dot files")
        #expect(b.atRoot && b.parts.isEmpty)
        b.showHidden = true
        try await settle(b)
        #expect(b.entries.map(\.name) == ["dev", "docker", ".hidden", "README.txt"])
        b.showHidden = false
        try await settle(b)

        b.go(FileBrowser.volume("data"))
        try await settle(b)
        #expect(b.parts == ["docker", "volumes", "data"])
        #expect(b.entries.map(\.name) == ["blob"] && b.entries.first?.size == 1234 && b.entries.first?.isDirectory == false)
        b.up()
        try await settle(b)
        #expect(b.parts == ["docker", "volumes"] && b.entries.map(\.name) == ["data"])
        if let data = b.entries.first { b.open(data) }
        try await settle(b)
        #expect(b.parts == ["docker", "volumes", "data"])

        b.go(FileBrowser.machine("dev") + ["etc"])
        try await settle(b)
        #expect(b.entries.first?.isLink == true && b.entries.first?.name == "localtime", "a link is shown as one, not followed")

        b.go(["docker", "..", ".."])
        try await settle(b)
        #expect(b.atRoot && b.directory.path == root.standardizedFileURL.path)
        b.up()
        #expect(b.atRoot)

        b.go(["docker", "containers", "gone"])
        try await settle(b)
        #expect(b.error != nil && b.entries.isEmpty)
    }

    @Test func aLinkOutOfTheViewIsNotOpened() async throws {
        let root = try tree()
        defer { try? FileManager.default.removeItem(at: root.deletingLastPathComponent()) }
        let fm = FileManager.default
        let secret = root.deletingLastPathComponent().appendingPathComponent("outside-secret")
        try Data("the Mac's own".utf8).write(to: secret)
        let vol = root.appendingPathComponent("docker/volumes/data")
        try fm.createSymbolicLink(atPath: vol.appendingPathComponent("abs").path, withDestinationPath: secret.path)
        try fm.createSymbolicLink(atPath: vol.appendingPathComponent("climb").path, withDestinationPath: "../../../../outside-secret")
        try fm.createSymbolicLink(atPath: vol.appendingPathComponent("inside").path, withDestinationPath: "blob")
        try fm.createSymbolicLink(atPath: vol.appendingPathComponent("up-inside").path, withDestinationPath: "../../../README.txt")
        let b = FileBrowser(root: root)
        b.go(FileBrowser.volume("data"))
        try await settle(b)
        func entry(_ n: String) throws -> FileBrowser.Entry { try #require(b.entries.first { $0.name == n }) }
        let abs = try entry("abs"), climb = try entry("climb"), inside = try entry("inside")
        let upInside = try entry("up-inside"), blob = try entry("blob")
        #expect(b.openable(abs) == nil, "an absolute link to the Mac's file")
        #expect(b.openable(climb) == nil, "a link that climbs out of the view")
        #expect(b.openable(inside) != nil && b.openable(upInside) != nil, "links inside the view")
        #expect(b.openable(blob) == b.url(blob), "a file is itself")
        #expect(b.linkTarget(abs) == secret.path)
        b.go(FileBrowser.machine("dev") + ["etc"])
        try await settle(b)
        let localtime = try entry("localtime")
        #expect(b.openable(localtime) == nil, "/usr/share/zoneinfo/UTC is the Mac's")
    }

    @Test func whereEachObjectIs() {
        let c = Container(id: "abc", names: ["/web-1"], image: "nginx", state: "running")
        #expect(FileBrowser.container(c) == ["docker", "containers", "web-1"])
        #expect(FileBrowser.volume("db") == ["docker", "volumes", "db"])
        #expect(FileBrowser.machine("dev") == ["dev"])
        #expect(FileBrowser.image(DockerImage(id: "sha256:0123456789abcdef", repoTags: ["ghcr.io/org/app:1"])) == ["docker", "images", "ghcr.io_org_app:1"])
        #expect(FileBrowser.image(DockerImage(id: "sha256:0123456789abcdef", repoTags: [])) == ["docker", "images", "0123456789ab"])
    }
}
