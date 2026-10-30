import Darwin
import Foundation

@MainActor
public final class RunDirWatch {
    private let home: String
    private let onEvent: @MainActor () -> Void
    private var source: (any DispatchSourceFileSystemObject)?
    public private(set) var place = ""

    public init(home: String, onEvent: @escaping @MainActor () -> Void) {
        self.home = home
        self.onEvent = onEvent
        arm()
    }

    public func cancel() {
        source?.cancel()
        source = nil
        place = ""
    }

    private func target() -> String {
        let run = home + "/run"
        if FileManager.default.fileExists(atPath: run) { return run }
        if FileManager.default.fileExists(atPath: home) { return home }
        return (home as NSString).deletingLastPathComponent
    }

    private func arm() {
        let path = target()
        if path == place, source != nil { return }
        source?.cancel()
        source = nil
        let fd = open(path, O_EVTONLY)
        guard fd >= 0 else { place = ""; return }
        let s = DispatchSource.makeFileSystemObjectSource(fileDescriptor: fd, eventMask: [.write, .delete, .rename], queue: .main)
        s.setEventHandler { [weak self] in
            MainActor.assumeIsolated {
                guard let self, self.source != nil else { return }
                self.arm()
                self.onEvent()
            }
        }
        s.setCancelHandler { close(fd) }
        source = s
        place = path
        s.resume()
    }
}
