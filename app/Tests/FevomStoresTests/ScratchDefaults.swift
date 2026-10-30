import Foundation

enum ScratchDefaults {
    static let dir: URL = {
        let d = FileManager.default.temporaryDirectory.appendingPathComponent("fev-test-defaults-\(getpid())")
        try? FileManager.default.createDirectory(at: d, withIntermediateDirectories: true)
        atexit { try? FileManager.default.removeItem(at: ScratchDefaults.dir) }
        return d
    }()

    static func make() -> UserDefaults {
        UserDefaults(suiteName: dir.appendingPathComponent("dev.fevom.test.\(UUID().uuidString)").path)!
    }
}
