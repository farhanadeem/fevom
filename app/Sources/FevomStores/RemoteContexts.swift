import FevomAPI
import Foundation

extension AppModel {
    public func loadContexts() async {
        do {
            let r = try await withClient(starting: false) { try await $0.call(DaemonAPI.Methods.contextsList) }
            contexts = r.contexts
        } catch {
            contexts = []
        }
    }

    public func loadContextContainers(_ context: String) async {
        do {
            let r = try await withClient(starting: false) {
                try await $0.call(DaemonAPI.Methods.contextContainers, .init(context: context))
            }
            contextContainers[context] = r.containers
            contextErrors[context] = nil
        } catch {
            contextContainers[context] = []
            contextErrors[context] = oneLineError(error)
        }
    }

    public func loadContextImages(_ context: String) async {
        do {
            let r = try await withClient(starting: false) {
                try await $0.call(DaemonAPI.Methods.contextImages, .init(context: context))
            }
            contextImages[context] = r.images
            contextErrors[context] = nil
        } catch {
            contextImages[context] = []
            contextErrors[context] = oneLineError(error)
        }
    }

    public func dismissContextError(_ context: String) {
        contextErrors[context] = nil
    }

    public func loadContext(_ context: String) async {
        async let a: Void = loadContextContainers(context)
        async let b: Void = loadContextImages(context)
        _ = await (a, b)
    }

    private func oneLineError(_ error: Error) -> String {
        let s = String(describing: error)
        return s.split(whereSeparator: \.isNewline).first.map(String.init) ?? s
    }
}
