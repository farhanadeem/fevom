import Foundation
import Testing

@testable import FevomDocker
@testable import FevomStores

@Suite struct ViewStoreTests {
    func c(_ name: String, project: String? = nil, service: String? = nil, state: String = "running", created: Int64 = 0) -> Container {
        var labels: [String: String] = [:]
        if let project { labels["com.docker.compose.project"] = project }
        if let service { labels["com.docker.compose.service"] = service }
        return Container(id: "id-" + name, names: ["/" + name], image: "img", state: state, created: created, labels: labels)
    }

    @Test func containersAreGroupedByProject() {
        let all = [c("solo"), c("shop-web-1", project: "shop", service: "web"), c("shop-db-1", project: "shop", service: "db", state: "exited"),
                   c("blog-app-1", project: "blog", service: "app")]
        let rows = ContainerRow.rows(all)
        #expect(rows.map(\.name) == ["blog", "shop", "solo"])
        #expect(rows[1].children?.map(\.name) == ["db", "web"])
        #expect(rows[1].state == "1/2 running" && rows[0].state == "running")
        #expect(ContainerRow.rows(all, filter: "web").map(\.name) == ["shop"])
        #expect(ContainerRow.rows(all, filter: "web").first?.children?.count == 1)
        #expect(ContainerRow.rows(all, filter: "shop").first?.children?.count == 2)
        #expect(ContainerRow.rows(all, filter: "nothing").isEmpty)
    }

    @Test func theLogStreamIsParsedAcrossReads() {
        func frame(_ s: UInt8, _ text: String) -> [UInt8] {
            let b = Array(text.utf8)
            return [s, 0, 0, 0, UInt8(b.count >> 24), UInt8(b.count >> 16 & 255), UInt8(b.count >> 8 & 255), UInt8(b.count & 255)] + b
        }
        let body = frame(1, "hello\nwor") + frame(1, "ld\n") + frame(2, "oops\n")
        let chunk1 = Array(body[0..<10]), chunk2 = Array(body[10...])
        func chunked(_ b: [UInt8]) -> [UInt8] { Array(String(b.count, radix: 16).utf8) + [13, 10] + b + [13, 10] }
        let wire = Array("HTTP/1.1 200 OK\r\nContent-Type: application/vnd.docker.multiplexed-stream\r\nTransfer-Encoding: chunked\r\n\r\n".utf8)
            + chunked(chunk1) + chunked(chunk2) + Array("0\r\n\r\n".utf8)
        let p = DockerLogStream.parser()
        var lines: [DockerLogStream.Line] = []
        for piece in stride(from: 0, to: wire.count, by: 7) { lines += p.feed(Array(wire[piece..<min(wire.count, piece + 7)])) }
        #expect(lines.map { String(decoding: $0.bytes, as: UTF8.self) } == ["hello", "world", "oops"])
        #expect(lines.map(\.stream) == [1, 1, 2])
        #expect(p.s.sawEnd)

        let raw = DockerLogStream.parser(maxLine: 4)
        let out = raw.feed(Array("HTTP/1.1 200 OK\r\nContent-Type: application/vnd.docker.raw-stream\r\n\r\nabcdefghij\nxy\n".utf8))
        #expect(out.map { String(decoding: $0.bytes, as: UTF8.self) } == ["abcd", "efgh", "ij", "xy"])

        let e = DockerLogStream.parser()
        _ = e.feed(Array("HTTP/1.1 404 Not Found\r\nContent-Length: 30\r\n\r\n{\"message\":\"No such container\"}".utf8))
        #expect(e.status == 404)
    }

    @Test func longLinesInPartsAreJoined() {
        func frame(_ s: UInt8, _ b: [UInt8]) -> [UInt8] {
            [s, 0, 0, 0, UInt8(b.count >> 24), UInt8(b.count >> 16 & 255), UInt8(b.count >> 8 & 255), UInt8(b.count & 255)] + b
        }
        let part = DockerLogStream.logPart
        let ts = Array("2026-10-16T12:00:00.123456789Z ".utf8)
        let ts2 = Array("2026-10-16T12:00:01.000000000Z ".utf8)
        let long = [UInt8](repeating: 0x79, count: 2 * part + 100)
        let exact = [UInt8](repeating: 0x7A, count: part)
        func parts(_ stream: UInt8, _ t: [UInt8], _ text: [UInt8], framed: Bool) -> [UInt8] {
            var out: [UInt8] = []
            var rest = text[...]
            while true {
                let n = min(rest.count, part)
                var msg = t + rest.prefix(n)
                rest = rest.dropFirst(n)
                if n < part { msg.append(10) }
                out += framed ? frame(stream, msg) : msg
                if n < part { return out }
            }
        }
        func feed(_ p: DockerLogStream.Parser, _ head: String, _ body: [UInt8]) -> [DockerLogStream.Line] {
            let wire = Array("HTTP/1.1 200 OK\r\nContent-Type: \(head)\r\n\r\n".utf8) + body
            var out: [DockerLogStream.Line] = []
            for i in stride(from: 0, to: wire.count, by: 4099) { out += p.feed(Array(wire[i..<min(wire.count, i + 4099)])) }
            return out
        }
        let first = parts(1, ts, long, framed: true)
        let firstFrame = 8 + ts.count + part
        let body = Array(first[0..<firstFrame]) + frame(2, ts2 + Array("err\n".utf8)) + Array(first[firstFrame...])
            + parts(1, ts2, exact, framed: true) + frame(1, ts2 + Array("2026-10-16T12:00:09.000000000Z looks like one\n".utf8))
        let lines = feed(DockerLogStream.parser(timestamps: true), "application/vnd.docker.multiplexed-stream", body)
        #expect(lines.map(\.stream) == [2, 1, 1, 1])
        #expect(lines.map(\.bytes) == [ts2 + Array("err".utf8), ts + long, ts2 + exact, ts2 + Array("2026-10-16T12:00:09.000000000Z looks like one".utf8)])
        let plain = feed(DockerLogStream.parser(), "application/vnd.docker.multiplexed-stream", parts(1, [], long, framed: true))
        #expect(plain.map(\.bytes) == [long])
        let raw = feed(DockerLogStream.parser(timestamps: true), "application/vnd.docker.raw-stream",
                       parts(1, ts, long, framed: false) + parts(1, ts2, exact, framed: false) + ts + Array("short\r\n".utf8))
        #expect(raw.map(\.bytes) == [ts + long, ts2 + exact, ts + Array("short\r".utf8)])
        let cut = feed(DockerLogStream.parser(maxLine: part, timestamps: true), "application/vnd.docker.multiplexed-stream", parts(1, ts, long, framed: true))
        #expect(cut.count == 3 && cut.allSatisfy { Array($0.bytes.prefix(ts.count)) == ts })
        #expect(cut.reduce(0) { $0 + $1.bytes.count - ts.count } == long.count)
    }

    @Test func aLineBeingWrittenAtTheBacklogsEndIsGivenWhole() {
        func frame(_ s: UInt8, _ b: [UInt8]) -> [UInt8] {
            [s, 0, 0, 0, UInt8(b.count >> 24), UInt8(b.count >> 16 & 255), UInt8(b.count >> 8 & 255), UInt8(b.count & 255)] + b
        }
        let part = DockerLogStream.logPart
        let t1 = Array("2026-10-16T12:00:00.100000000Z ".utf8)
        let t2 = Array("2026-10-16T12:00:00.200000000Z ".utf8)
        let t3 = Array("2026-10-16T12:00:00.300000000Z ".utf8)
        let p1 = [UInt8](repeating: 0x70, count: part), p2 = [UInt8](repeating: 0x71, count: part), end = Array("the end".utf8)
        func lines(_ body: [UInt8], maxLine: Int = 1 << 20) -> [LogFeed.Item] {
            let p = DockerLogStream.parser(maxLine: maxLine, timestamps: true)
            let out = p.feed(Array("HTTP/1.1 200 OK\r\nContent-Type: application/vnd.docker.multiplexed-stream\r\n\r\n".utf8) + body) + p.end()
            return out.map { LogFeed.item($0, label: "") }
        }
        let backlog = frame(1, t1 + Array("first\n".utf8)) + frame(1, t2 + p1) + frame(2, t3 + Array("err\n".utf8)) + frame(1, t2 + p2)
        var cut = FollowCut(lines(backlog))
        #expect(cut.shown.map(\.bytes) == [t1 + Array("first".utf8), t3 + Array("err".utf8)])
        #expect(cut.since == "\(TimeKey(t1[0..<30])!.seconds)")
        let t4 = Array("2026-10-16T12:00:00.400000000Z ".utf8)
        let follow = backlog + frame(1, t2 + end + [10]) + frame(1, t4 + Array("after\n".utf8))
        let kept = lines(follow).filter { cut.keep($0) }
        #expect(kept.map(\.bytes) == [t2 + p1 + p2 + end, t4 + Array("after".utf8)], "\(kept.map { $0.bytes.count })")

        let small = part
        var cut2 = FollowCut(lines(backlog, maxLine: small))
        let pieces = cut2.shown.filter { $0.key == TimeKey(t2[0..<30])! }
        #expect(pieces.count == 2 && cut2.open[1]?.skip == 2, "\(pieces.count)")
        let kept2 = lines(follow, maxLine: small).filter { cut2.keep($0) }
        let given = (pieces + kept2.filter { $0.key == TimeKey(t2[0..<30])! }).map { Array($0.bytes.dropFirst(t2.count)) }
        #expect(given.reduce([], +) == p1 + p2 + end, "the line's text once, in order")
        #expect(kept2.last?.bytes == t4 + Array("after".utf8))
    }

    @Test func deletionsSayWhatGoesAndWhatStays() {
        let web = c("shop-web-1", project: "shop", service: "web"), db = c("shop-db-1", project: "shop", service: "db", state: "exited")
        let p = DeleteRequest.project("shop", [web, db])
        #expect(p.title == "Delete the Compose project shop and its 2 containers?")
        #expect(p.message == "shop-db-1, shop-web-1. Running containers are stopped first. The project's networks, volumes and Compose file are kept.")
        #expect(p.button == "Delete Project" && p.id == "project:shop")
        let one = DeleteRequest.container(db)
        #expect(one.title == "Delete the container shop-db-1?" && one.message == "Its files are deleted; its volumes are kept.")
        let all = [web, db, c("lone"), c("other-web-1", project: "other", service: "web")]
        let row = ContainerRow.rows(all, filter: "web").first { $0.project == "shop" }
        #expect(row?.children?.count == 1, "the filter shows shop-web-1 alone")
        let filtered = DeleteRequest.project(named: "shop", in: all)
        #expect(filtered == p, "every container of shop, not the row's")
        #expect(DeleteRequest.container(web).message.hasPrefix("It's stopped first."))
        #expect(DeleteRequest.machine("dev").title == "Delete dev and all of its files?", "as fev delete asks")
    }

    @Test func timestampsOrderAsMoments() {
        let a = TimeKey(Array("2026-10-16T12:00:00.5Z".utf8)[...])!
        let b = TimeKey(Array("2026-10-16T12:00:00.123456789Z".utf8)[...])!
        let c = TimeKey(Array("2026-10-16T12:00:01Z".utf8)[...])!
        #expect(b < a && a < c, "a fraction is a fraction, not a string")
        #expect(a.seconds == 1792152000 && a.nanos == 500_000_000)
        #expect(TimeKey(Array("not a time".utf8)[...]) == nil)
        let item = LogFeed.item(.init(stream: 1, bytes: Array("2026-10-16T12:00:00Z hi".utf8)), label: "web | ")
        #expect(String(decoding: item.bytes, as: UTF8.self) == "2026-10-16T12:00:00Z web | hi")
        #expect(LogFeed.withoutTimestamp("2026-10-16T12:00:00.1Z web | hi") == "web | hi")
        #expect(LogFeed.withoutTimestamp("plain line here and more") == "plain line here and more")
    }

    @Test func theDistributionsAreHostImagesDefaults() throws {
        let file = URL(fileURLWithPath: #filePath).deletingLastPathComponent().appendingPathComponent("../../../host/images/defaults.go")
        let text = try String(contentsOf: file, encoding: .utf8)
        let block = text.components(separatedBy: "var Defaults = map[string]string{")[1].components(separatedBy: "}")[0]
        var go: [String: String] = [:]
        for line in block.split(separator: "\n") {
            let parts = line.split(separator: "\"").map(String.init)
            if parts.count >= 4 { go[parts[1]] = parts[3] }
        }
        #expect(go.count >= 14)
        #expect(Dictionary(uniqueKeysWithValues: MachineCatalog.defaults.map { ($0.distro, $0.release) }) == go)
    }
}
