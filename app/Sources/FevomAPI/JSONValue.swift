import Foundation

public enum JSONValue: Sendable, Hashable, Codable {
    case null
    case bool(Bool)
    case int(Int64)
    case uint(UInt64)
    case double(Double)
    case string(String)
    case array([JSONValue])
    case object([String: JSONValue])

    public init(from decoder: any Decoder) throws {
        let c = try decoder.singleValueContainer()
        if c.decodeNil() {
            self = .null
        } else if let b = try? c.decode(Bool.self) {
            self = .bool(b)
        } else if let i = try? c.decode(Int64.self) {
            self = .int(i)
        } else if let u = try? c.decode(UInt64.self) {
            self = .uint(u)
        } else if let d = try? c.decode(Double.self) {
            self = .double(d)
        } else if let s = try? c.decode(String.self) {
            self = .string(s)
        } else if let a = try? c.decode([JSONValue].self) {
            self = .array(a)
        } else {
            self = .object(try c.decode([String: JSONValue].self))
        }
    }

    public func encode(to encoder: any Encoder) throws {
        var c = encoder.singleValueContainer()
        switch self {
        case .null: try c.encodeNil()
        case .bool(let v): try c.encode(v)
        case .int(let v): try c.encode(v)
        case .uint(let v): try c.encode(v)
        case .double(let v): try c.encode(v)
        case .string(let v): try c.encode(v)
        case .array(let v): try c.encode(v)
        case .object(let v): try c.encode(v)
        }
    }

    public var number: Double? {
        switch self {
        case .int(let v): Double(v)
        case .uint(let v): Double(v)
        case .double(let v): v
        default: nil
        }
    }

    public var display: String {
        switch self {
        case .null: "null"
        case .bool(let v): v ? "true" : "false"
        case .int(let v): String(v)
        case .uint(let v): String(v)
        case .double(let v): String(v)
        case .string(let v): v
        default:
            (try? String(decoding: JSONEncoder.daemon.encode(self), as: UTF8.self)) ?? "?"
        }
    }
}

extension JSONEncoder {
    public static var daemon: JSONEncoder {
        let e = JSONEncoder()
        e.outputFormatting = [.withoutEscapingSlashes]
        return e
    }
}
