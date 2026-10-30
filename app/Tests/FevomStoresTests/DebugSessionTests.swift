import Foundation
import Testing

@testable import FevomAPI
@testable import FevomStores

@MainActor
@Suite struct DebugSessionTests {
    @Test func theBannerNamesARunningContainerAndItsImage() {
        let s = DaemonAPI.DebugStarted(id: 1, token: String(repeating: "a", count: 32), kind: "running",
                                       targetID: "abc123", name: "web", image: "nginx:1.29")
        let lines = DebugSession.banner(from: s)
        #expect(lines == ["fev debug: web (nginx:1.29), running; tools in /nix"])
    }

    @Test func theBannerSaysReadOnlyAndCarriesTheGuestsNotes() {
        let s = DaemonAPI.DebugStarted(id: 2, token: String(repeating: "b", count: 32), kind: "stopped",
                                       targetID: "def456", name: "db", image: "postgres:17", readOnly: true,
                                       notes: ["the container is stopped; this session's changes are committed on exit"])
        let lines = DebugSession.banner(from: s)
        #expect(lines.first == "fev debug: db (postgres:17), stopped, read-only; tools in /nix")
        #expect(lines.last == "the container is stopped; this session's changes are committed on exit")
    }

    @Test func theBannerSaysAnImageHasNoContainer() {
        let s = DaemonAPI.DebugStarted(id: 3, token: String(repeating: "c", count: 32), kind: "image",
                                       targetID: "sha256:aa", name: "alpine:latest")
        let lines = DebugSession.banner(from: s)
        #expect(lines == ["fev debug: alpine:latest, image; tools in /nix"])
    }

    @Test func theBannerNamesARemoteContextAndCarriesItsNote() {
        let s = DaemonAPI.DebugStarted(id: 4, token: String(repeating: "d", count: 32), kind: "remote",
                                       targetID: "", name: "api",
                                       notes: ["on work-remote (its own engine); setup messages follow in the shell"])
        let lines = DebugSession.banner(from: s)
        #expect(lines.first == "fev debug: api — remote context")
        #expect(lines.last == "on work-remote (its own engine); setup messages follow in the shell")
    }

    @Test func theEndSaysWhereAStoppedContainersChangesAreKept() {
        let e = DaemonAPI.SessionExit(id: 1, exitCode: 0, streamEnded: true, note: "db was started during the session; the session's changes were not committed and are kept in the VM at /data/@debug/kept/db-x")
        #expect(DebugSession.why(e).hasPrefix("db was started during the session"))
        #expect(DebugSession.why(e).hasSuffix("exited with status 0"))
    }

    @Test func theEndWordsAnOrdinaryExit() {
        #expect(DebugSession.why(.init(id: 1, exitCode: 0, streamEnded: true)) == "exited with status 0")
        #expect(DebugSession.why(.init(id: 1, exitCode: 2, streamEnded: true)) == "exited with status 2")
        #expect(DebugSession.why(.init(id: 1, exitCode: -1, signal: 9, streamEnded: true)) == "ended by signal 9")
        #expect(DebugSession.why(.init(id: 1, exitCode: -1, error: "no such container", streamEnded: false)) == "no such container")
    }

    @Test func theEndWordsADebugSessionsOwnReasons() {
        let unavailable = DebugSession.why(.init(id: 1, exitCode: -1, reason: DaemonAPI.Const.debugReasonToolsUnavailable))
        #expect(unavailable.contains("tools could not be fetched"))
        #expect(unavailable.contains("fev debug -f"))
        let gone = DebugSession.why(.init(id: 1, exitCode: -1, reason: DaemonAPI.Const.debugReasonTargetGone))
        #expect(gone == "the target stopped or was removed")
    }
}
