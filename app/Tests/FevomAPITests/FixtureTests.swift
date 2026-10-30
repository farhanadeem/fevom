import Foundation
import Testing

@testable import FevomAPI

let protoDaemon = URL(fileURLWithPath: #filePath)
    .deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
    .appendingPathComponent("proto/daemon")

struct FixtureFile: Sendable, CustomStringConvertible {
    let name: String
    let pending: Bool
    let method: String
    let notification: Bool
    let params: Data
    let result: Data?
    var description: String { (pending ? "pending/" : "") + name }
}

func loadFixtures() throws -> [FixtureFile] {
    var out: [FixtureFile] = []
    for (dir, pending) in [("fixtures", false), ("fixtures/pending", true)] {
        let url = protoDaemon.appendingPathComponent(dir)
        let names = try FileManager.default.contentsOfDirectory(atPath: url.path).filter { $0.hasSuffix(".json") }.sorted()
        for name in names {
            let data = try Data(contentsOf: url.appendingPathComponent(name))
            guard let obj = try JSONSerialization.jsonObject(with: data) as? [String: Any],
                let method = obj["method"] as? String
            else { throw FixtureError("\(name): not a fixture") }
            let extra = Set(obj.keys).subtracting(["method", "notification", "params", "result"])
            if !extra.isEmpty { throw FixtureError("\(name): unknown fixture fields \(extra.sorted())") }
            func raw(_ key: String) throws -> Data? {
                guard let v = obj[key] else { return nil }
                return try JSONSerialization.data(withJSONObject: v, options: [.fragmentsAllowed])
            }
            out.append(FixtureFile(name: name, pending: pending, method: method,
                notification: (obj["notification"] as? Bool) ?? false,
                params: try raw("params") ?? Data("null".utf8), result: try raw("result")))
        }
    }
    return out
}

struct FixtureError: Error, CustomStringConvertible {
    let description: String
    init(_ d: String) { description = d }
}

func roundTrip<T: DaemonType>(_ type: T.Type, _ data: Data) throws -> Data {
    let value = try JSONDecoder().decode(T.self, from: data)
    let again = try JSONEncoder.daemon.encode(value)
    let back = try JSONDecoder().decode(T.self, from: again)
    if back != value { throw FixtureError("\(T.self) does not survive a second round trip") }
    return again
}

func jsonDiff(_ a: Any, _ b: Any, path: String = "$") -> [String] {
    switch (a, b) {
    case let (x as [String: Any], y as [String: Any]):
        var out: [String] = []
        for k in Set(x.keys).union(y.keys).sorted() {
            switch (x[k], y[k]) {
            case (nil, _): out.append("\(path).\(k): in the fixture but not written (unknown or dropped field)")
            case (_, nil): out.append("\(path).\(k): written but not in the fixture")
            case let (v?, w?): out += jsonDiff(v, w, path: "\(path).\(k)")
            }
        }
        return out
    case let (x as [Any], y as [Any]):
        if x.count != y.count { return ["\(path): \(x.count) elements against \(y.count)"] }
        return zip(x, y).enumerated().flatMap { jsonDiff($1.0, $1.1, path: "\(path)[\($0)]") }
    case let (x as NSNumber, y as NSNumber):
        let xb = CFGetTypeID(x) == CFBooleanGetTypeID(), yb = CFGetTypeID(y) == CFBooleanGetTypeID()
        return xb == yb && x == y ? [] : ["\(path): \(x) against \(y)"]
    case let (x as String, y as String):
        return x == y ? [] : ["\(path): \"\(x)\" against \"\(y)\""]
    case (is NSNull, is NSNull):
        return []
    default:
        return ["\(path): \(a) against \(b)"]
    }
}

func check(_ type: any DaemonType.Type, _ data: Data, _ what: String) throws -> [String] {
    let again: Data
    do {
        again = try roundTrip(type, data)
    } catch {
        return ["\(what): does not decode as \(type): \(error)"]
    }
    let want = try JSONSerialization.jsonObject(with: data, options: [.fragmentsAllowed])
    let have = try JSONSerialization.jsonObject(with: again, options: [.fragmentsAllowed])
    return jsonDiff(have, want).map { "\(what) as \(type): \($0)" }
}

@Suite struct DaemonAPIFixtures {
    @Test func everyFixtureRoundTripsThroughTheSwiftTypes() throws {
        let fixtures = try loadFixtures()
        #expect(fixtures.count > 90, "found \(fixtures.count) fixtures under \(protoDaemon.path)")
        var problems: [String] = []
        for f in fixtures {
            guard let entry = DaemonAPI.schema[f.method] ?? (f.pending ? DaemonAPI.pending[f.method] : nil) else {
                problems.append("\(f): a method the schema does not define")
                continue
            }
            if f.notification != (entry.result == nil) {
                problems.append("\(f): notification flag disagrees with the schema")
            }
            problems += try check(entry.params, f.params, "\(f) params")
            if let result = entry.result {
                guard let data = f.result else {
                    problems.append("\(f): no result")
                    continue
                }
                problems += try check(result, data, "\(f) result")
            }
        }
        #expect(problems.isEmpty, Comment(rawValue: "\(problems.count) problems:\n" + problems.joined(separator: "\n")))
    }

    @Test func everyMethodHasAFixture() throws {
        let fixtures = try loadFixtures()
        let built = Set(fixtures.filter { !$0.pending }.map(\.method))
        let pending = Set(fixtures.filter(\.pending).map(\.method))
        let missing = Set(DaemonAPI.schema.keys).subtracting(built).sorted()
        #expect(missing.isEmpty, "built methods with no fixture: \(missing)")
        let missingPending = Set(DaemonAPI.pending.keys).subtracting(pending).sorted()
        #expect(missingPending.isEmpty, "pending methods with no fixture: \(missingPending)")
        #expect(Set(DaemonAPI.schema.keys).isDisjoint(with: DaemonAPI.pending.keys))
    }

    @Test func anUnknownFieldIsCaught() throws {
        let data = Data(#"{"api_version":1,"build":"b","pid":1,"home":"/h","surprise":true}"#.utf8)
        let problems = try check(DaemonAPI.HelloResult.self, data, "hello")
        #expect(problems.contains { $0.contains("surprise") })
    }

    @Test func zeroValuesAreWrittenAsGoWritesThem() throws {
        let s = DaemonAPI.Status(seq: 3, state: "running", engine: "ready", since: "t")
        let obj = try JSONSerialization.jsonObject(with: JSONEncoder.daemon.encode(s)) as! [String: Any]
        #expect(Set(obj.keys) == ["seq", "state", "engine", "since", "pending_restart"])
        #expect((obj["pending_restart"] as? [Any])?.isEmpty == true)
    }
}
