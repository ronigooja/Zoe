import A2UISwiftCore
import Foundation
import XCTest
import ZoeKit
@testable import ZoeUI

final class UISurfaceHistoryStoreTests: XCTestCase {
    private var root: URL!
    private let scope = UISurfaceHistoryScope(instanceLineageID: "instance-1")
    private let base = Date(timeIntervalSince1970: 1_800_000_000)

    override func setUp() {
        super.setUp()
        root = FileManager.default.temporaryDirectory.appendingPathComponent("zoe-history-tests-\(UUID().uuidString)", isDirectory: true)
    }

    override func tearDown() {
        try? FileManager.default.removeItem(at: root)
        super.tearDown()
    }

    private func messages(_ text: String) throws -> [A2uiMessage] {
        try JSONDecoder().decode([A2uiMessage].self, from: Data("""
        [{"version":"v0.9","createSurface":{"surfaceId":"s","catalogId":"https://a2ui.org/specification/v0_9/basic_catalog.json"}},
         {"version":"v0.9","updateComponents":{"surfaceId":"s","components":[{"id":"root","component":"Text","text":"\(text)"}]}}]
        """.utf8))
    }

    private func append(_ store: UISurfaceHistoryStore, _ index: Int, scope: UISurfaceHistoryScope? = nil) async throws {
        try await store.append(scope: scope ?? self.scope, id: "entry-\(index)", container: .window,
                               messages: try messages("Item \(index)"), time: base.addingTimeInterval(TimeInterval(index)))
    }

    private func allPages(_ store: UISurfaceHistoryStore, scope: UISurfaceHistoryScope? = nil) async throws -> [UISurfaceHistoryEntry] {
        var offset = 0
        var result: [UISurfaceHistoryEntry] = []
        while true {
            let page = try await store.page(scope: scope ?? self.scope, offset: offset)
            result += page.entries
            offset += page.entries.count
            if !page.hasMore { return result }
        }
    }

    func testAppendListsNewestFirstWithTitlesAndSizes() async throws {
        let store = UISurfaceHistoryStore(root: root)
        for index in 0..<3 { try await append(store, index) }
        let page = try await store.page(scope: scope, offset: 0)
        XCTAssertEqual(page.entries.map(\.id), ["entry-2", "entry-1", "entry-0"])
        XCTAssertEqual(page.entries.first?.title, "Item 2")
        XCTAssertGreaterThan(page.entries.first?.size ?? 0, 0)
        XCTAssertFalse(page.hasMore)
    }

    func testAppendingExistingIDIsIdempotent() async throws {
        let store = UISurfaceHistoryStore(root: root)
        try await append(store, 0)
        try await append(store, 0)
        let page = try await store.page(scope: scope, offset: 0)
        XCTAssertEqual(page.entries.count, 1)
    }

    func testPagesFollowFiftyEntryBoundaries() async throws {
        let store = UISurfaceHistoryStore(root: root)
        for index in 0..<120 { try await append(store, index) }
        let first = try await store.page(scope: scope, offset: 0)
        XCTAssertEqual(first.entries.count, 50)
        XCTAssertTrue(first.hasMore)
        XCTAssertEqual(first.entries.first?.id, "entry-119")
        let second = try await store.page(scope: scope, offset: 50)
        XCTAssertEqual(second.entries.count, 50)
        XCTAssertTrue(second.hasMore)
        let last = try await store.page(scope: scope, offset: 100)
        XCTAssertEqual(last.entries.count, 20)
        XCTAssertFalse(last.hasMore)
        XCTAssertEqual(last.entries.last?.id, "entry-0")
        let beyond = try await store.page(scope: scope, offset: 120)
        XCTAssertEqual(beyond.entries, [])
    }

    func testDeleteRemovesOnlyTheChosenEntry() async throws {
        let store = UISurfaceHistoryStore(root: root)
        for index in 0..<3 { try await append(store, index) }
        try await store.delete(scope: scope, id: "entry-1")
        let remaining = try await allPages(store).map(\.id)
        XCTAssertEqual(remaining, ["entry-2", "entry-0"])
        await XCTAssertThrowsErrorAsync(try await store.record(scope: scope, id: "entry-1"))
    }

    func testClearAllRemovesOnlyThisScope() async throws {
        let store = UISurfaceHistoryStore(root: root)
        let other = UISurfaceHistoryScope(instanceLineageID: "instance-2")
        try await append(store, 0)
        try await append(store, 1, scope: other)
        try await store.clearAll(scope: scope)
        let cleared = try await allPages(store)
        let kept = try await allPages(store, scope: other).map(\.id)
        XCTAssertEqual(cleared, [])
        XCTAssertEqual(kept, ["entry-1"])
    }

    func testHistoryIsSeparatedPerAuthenticatedInstance() async throws {
        let store = UISurfaceHistoryStore(root: root)
        let otherServer = UISurfaceHistoryScope(instanceLineageID: "instance-2")
        let otherInstance = UISurfaceHistoryScope(instanceLineageID: "instance-3")
        try await append(store, 0)
        try await append(store, 1, scope: otherServer)
        try await append(store, 2, scope: otherInstance)
        let first = try await allPages(store).map(\.id)
        let second = try await allPages(store, scope: otherServer).map(\.id)
        let third = try await allPages(store, scope: otherInstance).map(\.id)
        XCTAssertEqual(first, ["entry-0"])
        XCTAssertEqual(second, ["entry-1"])
        XCTAssertEqual(third, ["entry-2"])
    }

    func testHistoryIdentitySurvivesCredentialAndAddressChanges() async throws {
        let store = UISurfaceHistoryStore(root: root)
        try await append(store, 0)
        // Neither a key nor a server address participates in the persisted namespace.
        let reauthenticated = UISurfaceHistoryScope(instanceLineageID: "instance-1")
        let entries = try await allPages(store, scope: reauthenticated)
        XCTAssertEqual(entries.map(\.id), ["entry-0"])
    }

    func testCorruptIndexIsRebuiltFromRecordFiles() async throws {
        let store = UISurfaceHistoryStore(root: root)
        for index in 0..<2 { try await append(store, index) }
        try corruptIndex(Data("not json".utf8))
        let rebuilt = try await allPages(store)
        XCTAssertEqual(rebuilt.map(\.id), ["entry-1", "entry-0"])
        XCTAssertEqual(rebuilt.map(\.title), ["Item 1", "Item 0"])
    }

    func testMissingIndexIsRebuiltFromRecordFiles() async throws {
        let store = UISurfaceHistoryStore(root: root)
        try await append(store, 0)
        try FileManager.default.removeItem(at: try indexURL())
        let rebuilt = try await allPages(store).map(\.id)
        XCTAssertEqual(rebuilt, ["entry-0"])
    }

    func testListingDoesNotReadEntryContent() async throws {
        let store = UISurfaceHistoryStore(root: root)
        try await append(store, 0)
        let page = try await store.page(scope: scope, offset: 0)
        let file = try recordURL(named: try XCTUnwrap(page.entries.first).fileName)
        try Data("damaged".utf8).write(to: file)
        let listed = try await store.page(scope: scope, offset: 0)
        XCTAssertEqual(listed.entries.map(\.id), ["entry-0"])
        await XCTAssertThrowsErrorAsync(try await store.record(scope: scope, id: "entry-0"))
    }

    func testRecordReturnsContentWhenOpened() async throws {
        let store = UISurfaceHistoryStore(root: root)
        try await append(store, 4)
        let record = try await store.record(scope: scope, id: "entry-4")
        XCTAssertEqual(record.title, "Item 4")
        XCTAssertEqual(record.container, .window)
        XCTAssertEqual(record.messages.count, 2)
    }

    func testTitlePrefersHeadingThenFirstTextAndNormalizesWhitespace() throws {
        let body = try messages("  first   body   line ")
        XCTAssertEqual(UISurfaceHistoryTitle.make(from: body), "first body line")
        let heading = try JSONDecoder().decode([A2uiMessage].self, from: Data("""
        [{"version":"v0.9","createSurface":{"surfaceId":"s","catalogId":"c"}},
         {"version":"v0.9","updateComponents":{"surfaceId":"s","components":[
           {"id":"a","component":"Text","text":"Body"},
           {"id":"b","component":"Text","text":"Heading","variant":"h2"}]}}]
        """.utf8))
        XCTAssertEqual(UISurfaceHistoryTitle.make(from: heading), "Heading")
    }

    func testTitleFallsBackWhenThereIsNoText() throws {
        let empty = try JSONDecoder().decode([A2uiMessage].self, from: Data("""
        [{"version":"v0.9","createSurface":{"surfaceId":"s","catalogId":"c"}}]
        """.utf8))
        XCTAssertEqual(UISurfaceHistoryTitle.make(from: empty), "Untitled")
    }

    private func indexURL() throws -> URL {
        try XCTUnwrap(FileManager.default.enumerator(at: root, includingPropertiesForKeys: nil)?
            .compactMap { $0 as? URL }.first { $0.lastPathComponent == "index.json" })
    }

    private func corruptIndex(_ data: Data) throws {
        try data.write(to: try indexURL(), options: .atomic)
    }

    private func recordURL(named name: String) throws -> URL {
        try XCTUnwrap(FileManager.default.enumerator(at: root, includingPropertiesForKeys: nil)?
            .compactMap { $0 as? URL }.first { $0.lastPathComponent == name })
    }
}

private func XCTAssertThrowsErrorAsync<T>(_ expression: @autoclosure () async throws -> T, file: StaticString = #filePath, line: UInt = #line) async {
    do {
        _ = try await expression()
        XCTFail("Expected an error", file: file, line: line)
    } catch {}
}
