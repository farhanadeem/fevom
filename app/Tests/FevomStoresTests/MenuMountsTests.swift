import Foundation
import Testing

@testable import FevomAPI
@testable import FevomDocker
@testable import FevomStores

@Suite struct MenuMountsTests {
    let machines = [DaemonAPI.Machine(name: "dev", state: "running"), DaemonAPI.Machine(name: "old", state: "stopped")]
    let containers = [Container(id: "1", names: ["/web"], image: "nginx", state: "running"), Container(id: "2", names: ["/gone"], image: "x", state: "exited")]

    @Test func theViewAndWhatRunsInIt() {
        let v = DaemonAPI.FilesView(state: "mounted", path: "/tmp/bv.x/Fevom")
        let items = MenuMounts.items(view: v, machines: machines, containers: containers)
        #expect(items.map(\.path) == ["/tmp/bv.x/Fevom", "/tmp/bv.x/Fevom/dev", "/tmp/bv.x/Fevom/docker/containers/web"])
        #expect(items.map(\.title) == ["/tmp/bv.x/Fevom", "dev", "web"])
        let many = (0..<30).map { Container(id: "\($0)", names: ["/c\($0)"], image: "i", state: "running") }
        #expect(MenuMounts.items(view: v, machines: [], containers: many).count == 21, "the view and 20 containers")
    }

    @Test func aViewNotMountedIsSaid() {
        let v = DaemonAPI.FilesView(state: "failed", path: "/tmp/bv.x/Fevom", error: "mount_nfs: refused")
        let items = MenuMounts.items(view: v, machines: machines, containers: containers)
        #expect(items == [MenuMount(title: "/tmp/bv.x/Fevom (failed: mount_nfs: refused)", path: "")])
        #expect(MenuMounts.items(view: nil, machines: machines, containers: containers).isEmpty)
    }
}
