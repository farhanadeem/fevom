import Foundation

public struct LogBuffer: Sendable {
    public let capacity: Int
    public let byteCapacity: Int
    public let chunkLines: Int
    public let chunkBytes: Int
    public let maxLineBytes: Int

    public static let defaultCapacity = 200_000
    public static let defaultByteCapacity = 64 << 20

    struct Chunk: Sendable {
        var bytes: [UInt8] = []
        var ends: [UInt32] = []
    }

    var chunks: [Chunk] = []
    var starts: [Int] = []
    public private(set) var firstSeq = 0
    public private(set) var endSeq = 0
    public private(set) var lineBytes = 0
    public private(set) var cutLines = 0

    public init(capacity: Int = LogBuffer.defaultCapacity, byteCapacity: Int = LogBuffer.defaultByteCapacity,
                chunkLines: Int = 16384, chunkBytes: Int = 8 << 20, maxLineBytes: Int = 1 << 20) {
        precondition(chunkBytes + maxLineBytes <= Int(UInt32.max), "a chunk's line ends are UInt32")
        precondition(capacity > 0 && byteCapacity > 0 && chunkLines > 0 && chunkBytes > 0 && maxLineBytes > 0)
        self.capacity = capacity
        self.byteCapacity = byteCapacity
        self.chunkLines = chunkLines
        self.chunkBytes = chunkBytes
        self.maxLineBytes = maxLineBytes
    }

    public var count: Int { endSeq - firstSeq }

    @discardableResult
    public mutating func append(_ line: some Collection<UInt8>) -> Int {
        var n = line.count
        if n > maxLineBytes {
            n = maxLineBytes
            cutLines += 1
        }
        if chunks.isEmpty || chunks[chunks.count - 1].ends.count == chunkLines
            || (!chunks[chunks.count - 1].ends.isEmpty && chunks[chunks.count - 1].bytes.count + n > chunkBytes) {
            if let last = chunks.indices.last, chunks[last].bytes.capacity > chunks[last].bytes.count + chunks[last].bytes.count / 8 {
                var exact = [UInt8]()
                exact.reserveCapacity(chunks[last].bytes.count)
                exact.append(contentsOf: chunks[last].bytes)
                chunks[last].bytes = exact
            }
            var c = Chunk()
            c.bytes.reserveCapacity(min(chunkBytes, chunkLines * 96))
            c.ends.reserveCapacity(chunkLines)
            chunks.append(c)
            starts.append(endSeq)
        }
        let i = chunks.count - 1
        if n == line.count {
            chunks[i].bytes.append(contentsOf: line)
        } else {
            chunks[i].bytes.append(contentsOf: line.prefix(n))
        }
        chunks[i].ends.append(UInt32(chunks[i].bytes.count))
        endSeq += 1
        lineBytes += n
        var dropped = 0
        while chunks.count > 1 && (count - chunks[0].ends.count >= capacity || lineBytes > byteCapacity) {
            dropped += chunks[0].ends.count
            firstSeq += chunks[0].ends.count
            lineBytes -= chunks[0].bytes.count
            chunks.removeFirst()
            starts.removeFirst()
        }
        return dropped
    }

    @inline(__always) func chunkIndex(_ seq: Int) -> Int {
        var lo = 0, hi = starts.count - 1
        while lo < hi {
            let mid = (lo + hi + 1) / 2
            if starts[mid] <= seq { lo = mid } else { hi = mid - 1 }
        }
        return lo
    }

    public func bytes(_ seq: Int) -> ArraySlice<UInt8>? {
        guard seq >= firstSeq, seq < endSeq else { return nil }
        let ci = chunkIndex(seq)
        let li = seq - starts[ci]
        let c = chunks[ci]
        let start = li == 0 ? 0 : Int(c.ends[li - 1])
        return c.bytes[start..<Int(c.ends[li])]
    }

    public func line(_ seq: Int) -> String? {
        bytes(seq).map { String(decoding: $0, as: UTF8.self) }
    }

    public func display(_ seq: Int, limit: Int) -> (text: String, more: Int)? {
        guard let b = bytes(seq) else { return nil }
        if b.count <= limit { return (String(decoding: b, as: UTF8.self), 0) }
        var end = b.startIndex + limit
        while end > b.startIndex && (b[end] & 0xC0) == 0x80 { end -= 1 }
        return (String(decoding: b[b.startIndex..<end], as: UTF8.self), b.endIndex - end)
    }

    public func cells(_ seq: Int, limit: Int) -> Int {
        guard let b = bytes(seq) else { return 0 }
        var n = 0
        for x in b.prefix(limit) {
            if x == 0x09 { n = (n / 8 + 1) * 8 } else if (x & 0xC0) != 0x80 { n += 1 }
        }
        return n
    }

    public var heldBytes: Int { chunks.reduce(0) { $0 + $1.bytes.capacity + $1.ends.capacity * 4 } }

    public func scan(_ filter: LogFilter, from: Int, to: Int) -> [Int] {
        var out: [Int] = []
        var seq = max(from, firstSeq)
        let end = min(to, endSeq)
        guard seq < end else { return out }
        var ci = chunkIndex(seq)
        while seq < end {
            let c = chunks[ci]
            var li = seq - starts[ci]
            let stop = min(c.ends.count, li + (end - seq))
            c.bytes.withUnsafeBufferPointer { buf in
                while li < stop {
                    let s = li == 0 ? 0 : Int(c.ends[li - 1])
                    if filter.matches(UnsafeBufferPointer(rebasing: buf[s..<Int(c.ends[li])])) { out.append(seq) }
                    li += 1
                    seq += 1
                }
            }
            ci += 1
        }
        return out
    }

    public mutating func clear() {
        chunks.removeAll()
        starts.removeAll()
        firstSeq = endSeq
        lineBytes = 0
    }
}

public struct LogFilter: Sendable, Equatable {
    public let text: String
    let needle: [UInt8]

    public init(_ text: String) {
        self.text = text
        needle = Array(text.utf8).map(LogFilter.fold)
    }

    public var isEmpty: Bool { needle.isEmpty }

    @inline(__always) static func fold(_ b: UInt8) -> UInt8 { b >= 65 && b <= 90 ? b | 0x20 : b }

    public func matches(_ hay: UnsafeBufferPointer<UInt8>) -> Bool {
        let n = needle.count
        if n == 0 { return true }
        if hay.count < n { return false }
        let first = needle[0]
        var i = 0
        let last = hay.count - n
        while i <= last {
            if LogFilter.fold(hay[i]) == first {
                var j = 1
                while j < n && LogFilter.fold(hay[i + j]) == needle[j] { j += 1 }
                if j == n { return true }
            }
            i += 1
        }
        return false
    }

    public func matches(_ s: String) -> Bool {
        var s = s
        return s.withUTF8 { matches($0) }
    }
}

public struct LogRows: Sendable {
    public private(set) var filter = LogFilter("")
    var matches: [Int] = []
    var head = 0

    public init() {}

    public func count(_ buf: LogBuffer) -> Int { filter.isEmpty ? buf.count : matches.count - head }

    public func seq(row: Int, _ buf: LogBuffer) -> Int { filter.isEmpty ? buf.firstSeq + row : matches[head + row] }

    public mutating func appended(from: Int, _ buf: LogBuffer) {
        guard !filter.isEmpty else { return }
        matches += buf.scan(filter, from: from, to: buf.endSeq)
        dropped(buf)
    }

    public mutating func dropped(_ buf: LogBuffer) {
        while head < matches.count && matches[head] < buf.firstSeq { head += 1 }
        if head > 65536 && head * 2 > matches.count {
            matches.removeFirst(head)
            head = 0
        }
    }

    public static func scanned(_ filter: LogFilter, _ buf: LogBuffer) -> (LogRows, upTo: Int) {
        var r = LogRows()
        r.filter = filter
        if !filter.isEmpty { r.matches = buf.scan(filter, from: buf.firstSeq, to: buf.endSeq) }
        return (r, buf.endSeq)
    }
}
