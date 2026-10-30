import Foundation

public enum DaemonAPI {
    public static func socketPath(home: String) -> String { home + "/run/fevd.sock" }
    public static func sessionSocketPath(home: String) -> String { home + "/run/session.sock" }
    public static func dockerSocketPath(home: String) -> String { home + "/run/docker.sock" }
    public static func machineConsoleLog(_ name: String) -> String { Const.logMachinePrefix + name }
    public static func machineRuntimeLog(_ name: String) -> String { Const.logMachinePrefix + name + ":runtime" }
    public static func stopMarkPath(home: String) -> String { home + "/stop-requested" }

    public struct SchemaEntry: Sendable {
        public let params: any DaemonType.Type
        public let result: (any DaemonType.Type)?
    }
}

public protocol DaemonType: Codable, Sendable, Equatable {}

public struct Call<Params: DaemonType, Result: DaemonType>: Sendable {
    public let method: String
    public init(_ method: String) { self.method = method }
}

public struct Notice<Params: DaemonType>: Sendable {
    public let method: String
    public init(_ method: String) { self.method = method }
}

public struct DaemonError: Error, Sendable, Equatable, CustomStringConvertible, Decodable {
    public let code: Int
    public let message: String
    public let data: JSONValue?

    public init(code: Int, message: String, data: JSONValue? = nil) {
        self.code = code
        self.message = message
        self.data = data
    }

    public var description: String { "\(message) (code \(code))" }

    public func data<T: DaemonType>(as type: T.Type) -> T? {
        guard let data, let raw = try? JSONEncoder.daemon.encode(data) else { return nil }
        return try? JSONDecoder().decode(T.self, from: raw)
    }
}
