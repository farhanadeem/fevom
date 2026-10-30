import FevomDocker
import Foundation

extension AppModel {
    var buildHistory: BuildHistoryRunner { BuildHistoryRunner(buildx: place.buildx, home: place.home) }

    public func buildTitle(_ b: BuildRecord) -> String {
        if let t = buildTags[b.ref]?.first { return Self.familiar(t) }
        return b.name.isEmpty ? "Untitled build" : b.name
    }

    nonisolated public static func familiar(_ ref: String) -> String {
        for p in ["docker.io/library/", "docker.io/"] where ref.hasPrefix(p) { return String(ref.dropFirst(p.count)) }
        return ref
    }

    public func buildSteps(_ ref: String) async -> BuildSteps? {
        let running = builds.first(where: { $0.ref == ref })?.state == "running"
        if !running, let s = cachedBuildSteps(ref) { return s }
        do {
            let s = try await buildHistory.steps(ref)
            if running {
                if !Task.isCancelled { refresh(.builds) }
            } else {
                if buildTags[ref] == nil { buildTags[ref] = s.tags }
                if isWatching(.builds) { buildSteps.put(ref, s) }
            }
            return s
        } catch {
            if !(error is CancellationError) { lastError = String(describing: error) }
            return nil
        }
    }

    public func cachedBuildSteps(_ ref: String) -> BuildSteps? {
        guard builds.first(where: { $0.ref == ref })?.state != "running" else { return nil }
        return buildSteps.get(ref)
    }

    func followRunningBuild() {
        guard buildFollower == nil, isWatching(.builds),
              let b = builds.first(where: { $0.state == "running" && !followedBuilds.contains($0.ref) }) else { return }
        followedBuilds.insert(b.ref)
        let token = UUID()
        let task = Task { [weak self] in
            guard let self else { return }
            _ = try? await buildHistory.run(["history", "logs", "--progress", "rawjson", b.ref], from: .error)
            guard buildFollower?.token == token else { return }
            buildFollower = nil
            refresh(.builds)
        }
        buildFollower = (b.ref, token, task)
    }

    func stopFollowingBuild() {
        guard let f = buildFollower else { return }
        f.task.cancel()
        followedBuilds.remove(f.ref)
        buildFollower = nil
    }

    public func removeBuild(_ ref: String) async {
        lastError = nil
        do {
            try await buildHistory.remove(ref)
            builds.removeAll { $0.ref == ref }
        } catch { lastError = String(describing: error) }
    }

    func tagBuilds() {
        guard !taggingBuilds else { return }
        taggingBuilds = true
        Task { [weak self] in
            guard let self else { return }
            defer { taggingBuilds = false }
            for b in builds.prefix(100) where buildTags[b.ref] == nil && b.state != "running" {
                guard isWatching(.builds) else { return }
                guard let s = try? await buildHistory.steps(b.ref) else { continue }
                buildTags[b.ref] = s.tags
                if isWatching(.builds) { buildSteps.put(b.ref, s) }
            }
        }
    }

    public func networkDetail(_ id: String) async -> DockerNetwork? {
        do { return try await docker.network(id) } catch {
            lastError = String(describing: error)
            return nil
        }
    }

    public func createNetwork(_ name: String, subnet: String = "") async {
        lastError = nil
        do { try await docker.createNetwork(name, subnet: subnet) } catch { lastError = String(describing: error) }
    }

    public func removeNetwork(_ id: String) async {
        lastError = nil
        do { try await docker.removeNetwork(id) } catch { lastError = String(describing: error) }
    }
}
