@preconcurrency import CoreServices
import Foundation

public enum FinderSidebar {
    public static func contains(_ path: String) -> Bool? {
        guard let list = favorites() else { return nil }
        return items(list).contains { same($0.1, path) }
    }

    public enum Failure: Error, CustomStringConvertible {
        case unavailable
        case refused
        public var description: String {
            switch self {
            case .unavailable: "Finder's Favorites cannot be read on this Mac"
            case .refused: "Finder's Favorites refused the entry"
            }
        }
    }

    public static func add(_ path: String) throws {
        guard let list = favorites() else { throw Failure.unavailable }
        if items(list).contains(where: { same($0.1, path) }) { return }
        let url = URL(fileURLWithPath: path, isDirectory: true) as CFURL
        guard let insert = insertItemURL,
              let item = insert(list, kLSSharedFileListItemLast.toOpaque(), nil, nil, url, nil, nil) else {
            throw Failure.refused
        }
        item.release()
    }

    private typealias InsertItemURL = @convention(c) (
        LSSharedFileList, UnsafeMutableRawPointer, CFString?, OpaquePointer?, CFURL, CFDictionary?, CFArray?
    ) -> Unmanaged<LSSharedFileListItem>?
    private static let insertItemURL: InsertItemURL? = {
        guard let sym = dlsym(UnsafeMutableRawPointer(bitPattern: -2), "LSSharedFileListInsertItemURL") else { return nil }
        return unsafeBitCast(sym, to: InsertItemURL.self)
    }()

    @discardableResult
    public static func remove(_ path: String) throws -> Int {
        guard let list = favorites() else { throw Failure.unavailable }
        var n = 0
        for (item, url) in items(list) where same(url, path) {
            if LSSharedFileListItemRemove(list, item) == noErr { n += 1 }
        }
        return n
    }

    static func same(_ url: URL?, _ path: String) -> Bool {
        guard let url else { return false }
        return url.standardizedFileURL.path == URL(fileURLWithPath: path, isDirectory: true).standardizedFileURL.path
    }

    private static func favorites() -> LSSharedFileList? {
        LSSharedFileListCreate(nil, kLSSharedFileListFavoriteItems.takeUnretainedValue(), nil)?.takeRetainedValue()
    }

    private static func items(_ list: LSSharedFileList) -> [(LSSharedFileListItem, URL?)] {
        var seed: UInt32 = 0
        let snapshot = LSSharedFileListCopySnapshot(list, &seed)?.takeRetainedValue() as? [LSSharedFileListItem] ?? []
        return snapshot.map { item in
            let flags = UInt32(kLSSharedFileListNoUserInteraction | kLSSharedFileListDoNotMountVolumes)
            return (item, LSSharedFileListItemCopyResolvedURL(item, flags, nil)?.takeRetainedValue() as URL?)
        }
    }
}
