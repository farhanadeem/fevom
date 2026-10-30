import FevomDocker
import FevomStores
import Foundation
import Testing

@Suite struct NetworkDecodeTests {
    static let inspect = #"""
    {"Name":"shop_default","Id":"7d1c2b0a9e8f7d1c2b0a9e8f7d1c2b0a9e8f7d1c2b0a9e8f7d1c2b0a9e8f7d1c","Created":"2026-10-25T20:56:40.123456789Z","Scope":"local","Driver":"bridge","EnableIPv4":true,"EnableIPv6":true,
     "IPAM":{"Driver":"default","Options":null,"Config":[{"Subnet":"fd00:db8::/64","Gateway":"fd00:db8::1"},{"Subnet":"198.19.64.0/24","Gateway":"198.19.64.1"}]},
     "Internal":false,"Attachable":true,"Ingress":false,"ConfigFrom":{"Network":""},"ConfigOnly":false,
     "Containers":{"abc":{"Name":"shop-web-1","EndpointID":"e1","MacAddress":"02:42:c6:13:40:02","IPv4Address":"198.19.64.2/24","IPv6Address":""}},
     "Options":{"com.docker.network.driver.mtu":"1500"},"Labels":{"com.docker.compose.network":"default","com.docker.compose.project":"shop"}}
    """#

    @Test func aNetworksInspectIsRead() throws {
        let n = try JSONDecoder().decode(DockerNetwork.self, from: Data(Self.inspect.utf8))
        #expect(n.name == "shop_default" && n.driver == "bridge" && n.attachable && !n.isInternal && n.ipv6)
        #expect(n.subnet == "198.19.64.0/24" && n.gateway == "198.19.64.1" && n.subnet6 == "fd00:db8::/64")
        #expect(n.containers.map(\.name) == ["shop-web-1"] && n.containers.first?.ipv4 == "198.19.64.2/24")
        #expect(n.options["com.docker.network.driver.mtu"] == "1500" && n.labels.count == 2 && !n.predefined)
        #expect(BuildTime.parse(n.created) != nil)
        let host = try JSONDecoder().decode(DockerNetwork.self, from: Data(#"{"Name":"host","Id":"h1","Driver":"host","Scope":"local","IPAM":{"Config":[]},"Containers":{}}"#.utf8))
        #expect(host.predefined && host.subnet.isEmpty && host.containers.isEmpty)
    }

    @Test func imageNamesAreShownAsTheDockerCLIShowsThem() {
        #expect(AppModel.familiar("docker.io/library/demo-api:latest") == "demo-api:latest")
        #expect(AppModel.familiar("docker.io/acme/web:1") == "acme/web:1")
        #expect(AppModel.familiar("ghcr.io/acme/web:1") == "ghcr.io/acme/web:1")
    }
}
