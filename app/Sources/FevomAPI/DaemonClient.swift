import Foundation

public final class DaemonClient: Sendable {
    public let connection: RPCConnection
    public let hello: DaemonAPI.HelloResult
    public let home: String

    public static func connect(home: String, build: String, client: String = "fev-app") async throws -> DaemonClient {
        let conn = try RPCConnection.connect(path: DaemonAPI.socketPath(home: home))
        do {
            let env = ProcessInfo.processInfo.environment
            let params = DaemonAPI.HelloParams(
                apiVersion: DaemonAPI.apiVersion, build: build, client: client,
                sshAuthSock: env["SSH_AUTH_SOCK"] ?? "")
            let hello = try await conn.call(DaemonAPI.Methods.daemonHello, params)
            guard hello.build == build else {
                throw DaemonError(code: DaemonAPI.Const.codeBuildMismatch, message: "daemon is build \(hello.build), this app is \(build)")
            }
            return DaemonClient(connection: conn, hello: hello, home: home)
        } catch {
            conn.close()
            throw error
        }
    }

    init(connection: RPCConnection, hello: DaemonAPI.HelloResult, home: String) {
        self.connection = connection
        self.hello = hello
        self.home = home
    }

    public func call<P, R>(_ call: Call<P, R>, _ params: P) async throws -> R {
        try await connection.call(call, params)
    }

    public func call<R>(_ call: Call<DaemonAPI.Empty, R>) async throws -> R {
        try await connection.call(call, DaemonAPI.Empty())
    }

    public func close() { connection.close() }
}

extension RPCConnection {
    public func call<P, R>(_ call: Call<P, R>, _ params: P) async throws -> R {
        let line = try await self.call(method: call.method, params: params)
        return try JSONDecoder().decode(Reply<R>.self, from: line).result
    }
}
