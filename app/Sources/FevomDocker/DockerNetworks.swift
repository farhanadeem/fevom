import Foundation

extension DockerClient {
    public func networks() async throws -> [DockerNetwork] {
        try JSONDecoder().decode([DockerNetwork].self, from: try await request("GET", "/networks"))
            .sorted { $0.name.localizedStandardCompare($1.name) == .orderedAscending }
    }

    public func network(_ id: String) async throws -> DockerNetwork {
        try JSONDecoder().decode(DockerNetwork.self, from: try await request("GET", "/networks/\(id)"))
    }

    public func createNetwork(_ name: String, subnet: String = "", gateway: String = "", isInternal: Bool = false) async throws {
        struct Pool: Encodable { let Subnet: String; let Gateway: String? }
        struct IPAM: Encodable { let Config: [Pool] }
        struct Create: Encodable { let Name: String; let Driver = "bridge"; let Internal: Bool; let IPAM: IPAM? }
        let ipam = subnet.isEmpty ? nil : IPAM(Config: [Pool(Subnet: subnet, Gateway: gateway.isEmpty ? nil : gateway)])
        _ = try await request("POST", "/networks/create", body: try JSONEncoder().encode(Create(Name: name, Internal: isInternal, IPAM: ipam)))
    }

    public func removeNetwork(_ id: String) async throws {
        _ = try await request("DELETE", "/networks/\(id)")
    }
}

public struct DockerNetwork: Decodable, Sendable, Hashable, Identifiable {
    public var id: String
    public var name: String
    public var driver: String
    public var scope: String
    public var created: String
    public var isInternal: Bool
    public var attachable: Bool
    public var ipv6: Bool
    public var pools: [Pool]
    public var options: [String: String]
    public var labels: [String: String]
    public var containers: [Member]

    public struct Pool: Decodable, Sendable, Hashable {
        public var subnet: String
        public var gateway: String
        enum CodingKeys: String, CodingKey { case subnet = "Subnet", gateway = "Gateway" }
        public init(from decoder: any Decoder) throws {
            let c = try decoder.container(keyedBy: CodingKeys.self)
            subnet = try c.decodeIfPresent(String.self, forKey: .subnet) ?? ""
            gateway = try c.decodeIfPresent(String.self, forKey: .gateway) ?? ""
        }
        public init(subnet: String, gateway: String) { self.subnet = subnet; self.gateway = gateway }
    }

    public struct Member: Sendable, Hashable {
        public var name: String
        public var ipv4: String
        public var ipv6: String
    }

    public var predefined: Bool { ["bridge", "host", "none"].contains(name) }

    public var subnet: String { (pools.first { !$0.subnet.contains(":") } ?? pools.first)?.subnet ?? "" }
    public var gateway: String { (pools.first { !$0.subnet.contains(":") } ?? pools.first)?.gateway ?? "" }
    public var subnet6: String { pools.first { $0.subnet.contains(":") }?.subnet ?? "" }

    enum CodingKeys: String, CodingKey {
        case id = "Id", name = "Name", driver = "Driver", scope = "Scope", created = "Created", isInternal = "Internal"
        case attachable = "Attachable", ipv6 = "EnableIPv6", ipam = "IPAM", options = "Options", labels = "Labels", containers = "Containers"
    }
    struct IPAM: Decodable { let Config: [Pool]? }
    struct Endpoint: Decodable { let Name: String?; let IPv4Address: String?; let IPv6Address: String? }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        name = try c.decodeIfPresent(String.self, forKey: .name) ?? ""
        driver = try c.decodeIfPresent(String.self, forKey: .driver) ?? ""
        scope = try c.decodeIfPresent(String.self, forKey: .scope) ?? ""
        created = try c.decodeIfPresent(String.self, forKey: .created) ?? ""
        isInternal = try c.decodeIfPresent(Bool.self, forKey: .isInternal) ?? false
        attachable = try c.decodeIfPresent(Bool.self, forKey: .attachable) ?? false
        ipv6 = try c.decodeIfPresent(Bool.self, forKey: .ipv6) ?? false
        pools = (try c.decodeIfPresent(IPAM.self, forKey: .ipam))?.Config ?? []
        options = try c.decodeIfPresent([String: String].self, forKey: .options) ?? [:]
        labels = try c.decodeIfPresent([String: String].self, forKey: .labels) ?? [:]
        let eps = try c.decodeIfPresent([String: Endpoint].self, forKey: .containers) ?? [:]
        containers = eps.values.map { Member(name: $0.Name ?? "", ipv4: $0.IPv4Address ?? "", ipv6: $0.IPv6Address ?? "") }
            .sorted { $0.name < $1.name }
    }

    public init(id: String, name: String, driver: String = "bridge", subnet: String = "", gateway: String = "") {
        self.id = id
        self.name = name
        self.driver = driver
        scope = "local"
        created = ""
        isInternal = false
        attachable = false
        ipv6 = false
        pools = subnet.isEmpty ? [] : [Pool(subnet: subnet, gateway: gateway)]
        options = [:]
        labels = [:]
        containers = []
    }
}
