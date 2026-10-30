import FevomAPI
import FevomDocker
import Foundation

struct BuildHistoryRunner: Sendable {
    let buildx: String?
    let home: String

    struct Failure: Error, CustomStringConvertible {
        let message: String
        var description: String { message }
    }

    func list() async throws -> [BuildRecord] {
        BuildRecord.parseList(try await run(["history", "ls", "--format", "json"]))
    }

    func steps(_ ref: String) async throws -> BuildSteps {
        BuildSteps.parse(try await run(["history", "logs", "--progress", "rawjson", ref], from: .error))
    }

    func remove(_ ref: String) async throws {
        _ = try await run(["history", "rm", ref])
    }

    enum Stream { case output, error }

    func run(_ args: [String], from stream: Stream = .output) async throws -> Data {
        guard let buildx else {
            throw Failure(message: "this app has no Buildx (Contents/MacOS/xbin/docker-buildx) to read the build history with")
        }
        let config = home + "/app/docker-config"
        try? FileManager.default.createDirectory(atPath: config, withIntermediateDirectories: true)
        let env = [
            "HOME": NSHomeDirectory(), "PATH": "/usr/bin:/bin",
            "DOCKER_HOST": "unix://" + DaemonAPI.dockerSocketPath(home: home), "DOCKER_CONFIG": config,
        ]
        let held = Held()
        return try await withTaskCancellationHandler {
            try await withCheckedThrowingContinuation { k in
                DispatchQueue.global(qos: .userInitiated).async {
                    let p = Process()
                    p.executableURL = URL(fileURLWithPath: buildx)
                    p.arguments = ["--builder", "default"] + args
                    p.environment = env
                    p.standardInput = FileHandle.nullDevice
                    let out = Pipe(), err = Pipe()
                    p.standardOutput = out
                    p.standardError = err
                    do {
                        try held.start(p) { try SessionAttach.spawning { try p.run() } }
                    } catch {
                        k.resume(throwing: error)
                        return
                    }
                    var errData = Data()
                    let group = DispatchGroup()
                    group.enter()
                    DispatchQueue.global().async {
                        errData = err.fileHandleForReading.readDataToEndOfFile()
                        group.leave()
                    }
                    let data = out.fileHandleForReading.readDataToEndOfFile()
                    group.wait()
                    p.waitUntilExit()
                    if held.cancelled {
                        k.resume(throwing: CancellationError())
                        return
                    }
                    if p.terminationStatus != 0 {
                        let said = String(decoding: errData, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
                        k.resume(throwing: Failure(message: said.isEmpty ? "buildx \(args.prefix(2).joined(separator: " ")) ended with \(p.terminationStatus)" : said))
                        return
                    }
                    k.resume(returning: stream == .output ? data : errData)
                }
            }
        } onCancel: {
            held.cancel()
        }
    }

    final class Held: @unchecked Sendable {
        private let lock = NSLock()
        private var process: Process?
        private(set) var cancelled: Bool {
            get { lock.withLock { _cancelled } }
            set { lock.withLock { _cancelled = newValue } }
        }
        private var _cancelled = false

        func start(_ p: Process, _ run: () throws -> Void) throws {
            try lock.withLock {
                if _cancelled { throw CancellationError() }
                try run()
                process = p
            }
        }

        func cancel() {
            lock.withLock {
                _cancelled = true
                if let p = process, p.isRunning { p.terminate() }
            }
        }
    }
}
