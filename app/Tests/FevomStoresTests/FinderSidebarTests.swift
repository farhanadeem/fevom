import Foundation
import Testing

@testable import FevomStores

@Suite struct FinderSidebarTests {
    @Test func onePlaceHoweverWritten() {
        #expect(FinderSidebar.same(URL(fileURLWithPath: "/Users/u/Fevom/"), "/Users/u/Fevom"))
        #expect(FinderSidebar.same(URL(string: "file:///Users/u/Fevom"), "/Users/u/./Fevom/"))
        #expect(!FinderSidebar.same(URL(fileURLWithPath: "/Users/u/Fevom2"), "/Users/u/Fevom"))
        #expect(!FinderSidebar.same(nil, "/Users/u/Fevom"))
    }
}
