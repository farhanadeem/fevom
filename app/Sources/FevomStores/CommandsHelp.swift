import Foundation

public enum CommandsHelp {
    public struct Entry: Sendable, Hashable, Identifiable {
        public var action: String
        public var command: String
        public var id: String { action }
    }

    public struct Group: Sendable, Hashable, Identifiable {
        public var title: String
        public var entries: [Entry]
        public var id: String { title }
    }

    public static let groups: [Group] = [
        Group(title: "Fevom", entries: [
            Entry(action: "Start fev", command: "fev start"),
            Entry(action: "Stop fev", command: "fev stop"),
            Entry(action: "Restart fev", command: "fev restart"),
            Entry(action: "Show the state", command: "fev status"),
            Entry(action: "Set up the command line", command: Onboarding.setupCommand),
            Entry(action: "Install the admin helper", command: Onboarding.adminCommand),
            Entry(action: "Trust Fevom's root certificate for HTTPS", command: Onboarding.httpsCommand),
        ]),
        Group(title: "Containers", entries: [
            Entry(action: "List containers", command: "docker ps -a"),
            Entry(action: "Follow a container's log", command: "docker logs -f NAME"),
            Entry(action: "Open a shell in a container", command: "docker exec -it NAME sh"),
            Entry(action: "Follow a Compose project's logs", command: "docker compose -p PROJECT logs -f"),
            Entry(action: "Stop a Compose project", command: "docker compose -p PROJECT stop"),
            Entry(action: "Resource usage", command: "docker stats"),
            Entry(action: "List published ports", command: "fev ports"),
        ]),
        Group(title: "Images and volumes", entries: [
            Entry(action: "Remove dangling images", command: "docker image prune"),
            Entry(action: "Create a volume", command: "docker volume create NAME"),
            Entry(action: "Remove a volume", command: "docker volume rm NAME"),
        ]),
        Group(title: "Machines", entries: [
            Entry(action: "List machines", command: "fev list"),
            Entry(action: "Create a machine", command: "fev create ubuntu NAME"),
            Entry(action: "Open a shell in a machine", command: "ssh NAME@fev"),
            Entry(action: "Run a command in the default machine", command: "fev run uname -a"),
            Entry(action: "Stop a machine", command: "fev stop NAME"),
            Entry(action: "Clone a machine", command: "fev clone NAME NEW"),
            Entry(action: "Rename a machine", command: "fev rename NAME NEW"),
            Entry(action: "Make a machine the default", command: "fev default NAME"),
            Entry(action: "Export a machine", command: "fev export NAME FILE.tar.zst"),
            Entry(action: "Follow fev's own log", command: "fev logs"),
            Entry(action: "Follow a machine's console and runtime log", command: "fev logs -a NAME"),
            Entry(action: "Delete a machine", command: "fev delete NAME"),
        ]),
        Group(title: "Settings", entries: [
            Entry(action: "Show every setting", command: "fev config show"),
            Entry(action: "Change a setting", command: "fev config set KEY VALUE"),
            Entry(action: "Put a setting back to its default", command: "fev config reset KEY"),
            Entry(action: "Edit the Docker engine's configuration", command: "fev config docker"),
        ]),
    ]

    public static var fevSubcommands: [[String]] {
        Array(Set(fevInvocations.map(\.path))).sorted { $0.joined(separator: " ") < $1.joined(separator: " ") }
    }

    public struct Invocation: Sendable, Hashable {
        public var path: [String]
        public var flags: [String]
    }

    public static var fevInvocations: [Invocation] {
        var out: Set<Invocation> = []
        for g in groups {
            for e in g.entries {
                var words = e.command.split(separator: " ").map(String.init)
                if words.first == "sudo" { words.removeFirst() }
                guard words.first == "fev" else { continue }
                words.removeFirst()
                guard let first = words.first else { continue }
                let path = first == "config" && words.count > 1 ? [first, words[1]] : [first]
                var flags: [String] = []
                for w in words.dropFirst(path.count) {
                    if w.hasPrefix("-") { flags.append(w); continue }
                    if let c = w.first, c.isUppercase { continue }
                    break
                }
                out.insert(Invocation(path: path, flags: flags))
            }
        }
        return out.sorted { ($0.path + $0.flags).joined(separator: " ") < ($1.path + $1.flags).joined(separator: " ") }
    }
}

public struct AppPreferences {
    public let defaults: UserDefaults
    public init(defaults: UserDefaults = .standard) { self.defaults = defaults }

    static let statusItemKey = "dev.fevom.showStatusItem"
    static let logLinesKey = "dev.fevom.logs.lines"
    static let logMiBKey = "dev.fevom.logs.mib"

    public var showStatusItem: Bool {
        get { defaults.object(forKey: Self.statusItemKey) as? Bool ?? true }
        nonmutating set { defaults.set(newValue, forKey: Self.statusItemKey) }
    }

    public var logLines: Int {
        get { Self.clamp(defaults.object(forKey: Self.logLinesKey) as? Int ?? LogBuffer.defaultCapacity, 1_000, 2_000_000) }
        nonmutating set { defaults.set(Self.clamp(newValue, 1_000, 2_000_000), forKey: Self.logLinesKey) }
    }

    public var logMiB: Int {
        get { Self.clamp(defaults.object(forKey: Self.logMiBKey) as? Int ?? (LogBuffer.defaultByteCapacity >> 20), 8, 1024) }
        nonmutating set { defaults.set(Self.clamp(newValue, 8, 1024), forKey: Self.logMiBKey) }
    }

    static func clamp(_ v: Int, _ lo: Int, _ hi: Int) -> Int { min(max(v, lo), hi) }
}
