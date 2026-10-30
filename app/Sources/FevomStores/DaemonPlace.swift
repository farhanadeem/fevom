import Darwin
import Foundation

public struct DaemonPlace: Sendable, Equatable {
    public var home: String
    public var build: String
    public var fevd: String?
    public var daemonArgs: [String]
    public var daemonEnvironment: [String: String]?
    public var buildx: String?

    public init(home: String, build: String, fevd: String?, daemonArgs: [String] = [], daemonEnvironment: [String: String]? = nil, buildx: String? = nil) {
        self.home = home
        self.build = build
        self.fevd = fevd
        self.daemonArgs = daemonArgs
        self.daemonEnvironment = daemonEnvironment
        self.buildx = buildx
    }

    public static var honoursEnvironment: Bool {
        #if DEBUG
        return true
        #else
        return false
        #endif
    }

    public static func resolve(build: String, environment env: [String: String] = ProcessInfo.processInfo.environment,
                               bundle: Bundle = .main, honourEnvironment: Bool = honoursEnvironment) -> DaemonPlace {
        let asked = honourEnvironment ? env["FEV_HOME"].flatMap { $0.isEmpty ? nil : $0 } : nil
        let home = asked ?? (userHome() + "/.fev")
        let fm = FileManager.default
        var candidates: [String] = []
        if honourEnvironment, let d = env["FEV_DAEMON"], !d.isEmpty { candidates.append(d) }
        candidates.append(bundle.bundleURL.appendingPathComponent("Contents/Helpers/fevd").path)
        if let exe = bundle.executableURL { candidates.append(exe.deletingLastPathComponent().appendingPathComponent("fevd").path) }
        let fevd = candidates.first { fm.isExecutableFile(atPath: $0) }
        let args = honourEnvironment ? (env["FEV_DAEMON_ARGS"] ?? "").split(separator: " ").map(String.init) : []
        var buildxs: [String] = []
        if honourEnvironment, let b = env["FEV_BUILDX"], !b.isEmpty { buildxs.append(b) }
        buildxs.append(bundle.bundleURL.appendingPathComponent("Contents/MacOS/xbin/docker-buildx").path)
        return DaemonPlace(home: home, build: build, fevd: fevd, daemonArgs: args,
                           daemonEnvironment: honourEnvironment ? nil : minimalEnvironment(lang: env["LANG"]),
                           buildx: buildxs.first { fm.isExecutableFile(atPath: $0) })
    }

    public static func minimalEnvironment(lang: String?) -> [String: String] {
        var env = [
            "PATH": "/usr/bin:/bin:/usr/sbin:/sbin",
            "HOME": userHome(),
            "USER": userName(),
            "TMPDIR": userTempDir(),
        ]
        if let lang, !lang.isEmpty, lang.utf8.count <= 64,
           lang.unicodeScalars.allSatisfy({ CharacterSet.alphanumerics.contains($0) || "_.-@".unicodeScalars.contains($0) }) {
            env["LANG"] = lang
        } else {
            env["LANG"] = "en_US.UTF-8"
        }
        return env
    }

    public static var defaultHome: String { userHome() + "/.fev" }
    public static var defaultView: String { userHome() + "/Fevom" }

    static func userHome() -> String {
        if let pw = getpwuid(getuid()), let dir = pw.pointee.pw_dir { return String(cString: dir) }
        return NSHomeDirectory()
    }

    static func userName() -> String {
        if let pw = getpwuid(getuid()), let name = pw.pointee.pw_name { return String(cString: name) }
        return NSUserName()
    }

    static func userTempDir() -> String {
        var buf = [CChar](repeating: 0, count: Int(PATH_MAX))
        let n = confstr(_CS_DARWIN_USER_TEMP_DIR, &buf, buf.count)
        if n > 0 && n <= buf.count { return String(cString: buf) }
        return "/tmp/"
    }
}
