import XCTest
@testable import ZoeKit

private final class TestClock: SilenceClock {
    var date: Date
    init(_ date: Date = Date(timeIntervalSince1970: 0)) { self.date = date }
    func now() -> Date { date }
}

@MainActor private final class TestSurfaceHost: SurfaceHost {
    var opened: [SurfaceSpec] = []
    var updated: [(String, ScreenDocument?, SurfaceWindowSpec?)] = []
    var closed: [String] = []
    var ids: [String] = []

    func open(_ spec: SurfaceSpec) { opened.append(spec); ids.append(spec.id) }
    func update(id: String, document: ScreenDocument?, window: SurfaceWindowSpec?) { updated.append((id, document, window)) }
    func close(id: String) { closed.append(id); ids.removeAll { $0 == id } }
    func openSurfaceIDs() -> [String] { ids }
}

@MainActor
final class ScreenTests: XCTestCase {
    func testCatalogIncludesEveryPrimitiveAndStandardComponent() {
        let types = Set(ComponentCatalog.descriptors.map(\.type))
        let expected = ["stack", "grid", "split", "group", "spacer", "canvas", "layer", "scroll", "frame", "background",
                        "title", "text", "richText", "list", "table", "progress", "statusDot", "image", "icon", "link", "countdown", "divider",
                        "button", "choice", "input", "toggle", "metric", "timeline", "steps", "decision", "compare", "collection", "article", "chart",
                        "place", "media", "notice", "facts", "status", "form", "quote", "code", "gauge", "calendar", "map", "gallery", "ranking", "checklist"]
        XCTAssertEqual(types, Set(expected))
        XCTAssertTrue(ComponentCatalog.descriptors.allSatisfy { !$0.purpose.isEmpty && !($0.fallback.isEmpty) })
    }

    func testUnknownComponentFallsBackToReadableTextAndResolvesChildren() {
        let node = ScreenNode(id: "mystery", type: "businessConcept", props: ["title": .string("Hello")], children: [
            ScreenNode(id: "known", type: "text", props: ["text": .string("Child")]),
            ScreenNode(id: "unknownChild", type: "notKnown", props: ["value": .number(2)])
        ])
        let result = ComponentCatalog.resolve(node)
        XCTAssertEqual(result.type, "stack")
        XCTAssertEqual(result.children.count, 4)
        XCTAssertTrue(result.children.allSatisfy { $0.type == "text" })
        XCTAssertTrue(result.children.contains { $0.props["text"] == .string("title: Hello") })
    }

    func testDocumentPatchAppliesInOrderAndRejectsStaleRevision() {
        let root = ScreenNode(id: "root", type: "stack", children: [
            ScreenNode(id: "one", type: "text", props: ["text": .string("old")]),
            ScreenNode(id: "two", type: "text", props: ["text": .string("keep")])
        ])
        let document = ScreenDocument(version: 1, revision: 4, root: root)
        let patch = ScreenDocumentPatch(revision: 5, operations: [
            .update(ScreenNodePatch(id: "one", props: ["text": .string("updated")])),
            .replace(id: "two", node: ScreenNode(id: "two", type: "statusDot", props: ["state": .string("done")])),
            .remove(id: "one")
        ])
        let result = document.applying(patch)
        XCTAssertEqual(result?.revision, 5)
        XCTAssertEqual(result?.root.children.map(\.id), ["two"])
        XCTAssertEqual(result?.root.children.first?.type, "statusDot")
        XCTAssertNil(document.applying(ScreenDocumentPatch(revision: 4)))
    }

    func testSettingsUserOnlyAuditUndoAndPersistence() throws {
        let store = InMemorySettingsStore()
        let registry = SettingsRegistry(store: store)
        registry.registerDefaults()
        XCTAssertEqual(registry.value(for: "voice.silenceTimeoutSeconds"), .number(60))
        XCTAssertThrowsError(try registry.write(key: "connection.instanceKey", value: .string("secret"), by: .ai)) { error in
            XCTAssertEqual(error as? SettingWriteError, .userOnly)
        }
        var notice: SettingAuditRecord?
        registry.onAIChange = { notice = $0 }
        let record = try XCTUnwrap(try registry.write(key: "voice.silenceTimeoutSeconds", value: .number(120), by: .ai))
        XCTAssertEqual(record.newValue, .number(120))
        XCTAssertEqual(registry.audit, [record])
        XCTAssertEqual(notice, record)
        XCTAssertTrue(registry.undo(recordID: record.id))
        XCTAssertEqual(registry.value(for: "voice.silenceTimeoutSeconds"), .number(60))
        XCTAssertTrue(registry.audit[0].undone)
        XCTAssertEqual(store.values["voice.silenceTimeoutSeconds"], .number(60))
    }

    func testSilenceDoesNotCountInteractionAndTimesOutDeterministically() {
        let clock = TestClock()
        let controller = SilenceController(timeoutSeconds: 10, clock: clock)
        controller.noteVoiceActivity()
        controller.setInteractionInProgress(true)
        clock.date = Date(timeIntervalSince1970: 100)
        XCTAssertEqual(controller.tick(), .none)
        clock.date = Date(timeIntervalSince1970: 1000)
        XCTAssertEqual(controller.tick(), .none)
        controller.setInteractionInProgress(false)
        XCTAssertEqual(controller.tick(), .none)
        clock.date = Date(timeIntervalSince1970: 1010)
        XCTAssertEqual(controller.tick(), .timedOut)
        XCTAssertEqual(controller.tick(), .none)
        controller.reset()
        XCTAssertEqual(controller.tick(), .none)
    }

    func testDefaultScreenIsChronologicalWithoutRankingOrInventedContent() {
        let task = TaskRecord(id: "t", projectId: nil, title: "Real task", goal: "real goal", source: "user", profileId: nil,
                              status: .active, version: 1, createdAt: "2026-01-01T00:00:00Z", updatedAt: "2026-01-02T00:00:00Z",
                              acceptedAt: nil, startedAt: nil, finishedAt: nil, result: nil, error: nil, evidence: [], canAcceptResult: false)
        let approval = Approval(id: "a", taskId: "t", status: "pending", prompt: "Real approval", version: 1,
                                createdAt: "2026-01-03T00:00:00Z", expiresAt: nil, requestedAction: nil, requestedActionDigest: nil)
        let notification = NotificationRecord(id: "n", kind: "info", title: "Real notice", body: "Real body", aggregateType: "task",
                                              aggregateId: "t", sourceEventId: "event", createdAt: "2026-01-04T00:00:00Z", readAt: nil)
        let document = DefaultScreenBuilder.build(tasks: [task], approvals: [approval], notifications: [notification], revision: 9)
        XCTAssertEqual(document.root.children.map(\.id), ["notification.n", "approval.a", "task.t"])
        XCTAssertFalse(document.root.children.contains { $0.type == "ranking" })
        XCTAssertTrue(document.root.children.flatMap(\.children).contains { $0.props["text"] == .string("Real task") })
        XCTAssertEqual(DefaultScreenBuilder.build(tasks: [], approvals: [], notifications: [], revision: 1).root.children, [])
    }

    func testRouterHandlesToolsAndShortErrors() async {
        let host = TestSurfaceHost()
        let settings = SettingsRegistry()
        settings.registerDefaults()
        var starts = 0
        let router = ScreenToolRouter(host: host, settings: settings, startVoice: { starts += 1 }, endVoice: {},
                                      setSilenceTimeout: { _ in }, showTextInput: {}, hideTextInput: {})
        let unknown = await router.handle(tool: "no.such.tool", args: [:])
        XCTAssertEqual(unknown, .object(["error": .string("unknown tool")]))
        let missing = await router.handle(tool: "surface.close", args: [:])
        XCTAssertEqual(missing, .object(["error": .string("missing surface id")]))
        let denied = await router.handle(tool: "settings.write", args: ["key": .string("ai.settingsAccess"), "value": .bool(false)])
        XCTAssertEqual(denied, .object(["error": .string("setting is user-only")]))
        _ = await router.handle(tool: "voice.start", args: [:])
        XCTAssertEqual(starts, 1)
    }

    func testRouterDecodesFlatWindowContractIncludingMenuBarAndDescribesCatalogShape() async throws {
        let host = TestSurfaceHost()
        let router = ScreenToolRouter(host: host, settings: SettingsRegistry(), startVoice: {}, endVoice: {}, setSilenceTimeout: { _ in }, showTextInput: {}, hideTextInput: {})
        let document: JSONValue = .object(["version": .integer(1), "revision": .integer(9), "root": .object(["id": .string("root"), "type": .string("text"), "props": .object(["text": .string("hello")])])])
        let result = await router.handle(tool: "surface.open", args: ["id": .string("surface"), "kind": .string("window"), "document": document,
                                                                        "window": .object(["width": .number(400), "height": .number(200), "x": .number(10), "y": .number(20), "floating": .bool(true), "showInMenuBar": .bool(true)])])
        XCTAssertEqual(result, .object(["ok": .bool(true)]))
        XCTAssertEqual(host.opened.first?.window.width, 400)
        XCTAssertEqual(host.opened.first?.window.x, 10)
        XCTAssertEqual(host.opened.first?.window.showInMenuBar, true)

        let described = await router.handle(tool: "components.describe", args: [:])
        guard case let .object(fields) = described, case let .array(components)? = fields["components"],
              case let .object(stack)? = components.first(where: { if case let .object(value) = $0 { return value["type"] == .string("stack") }; return false }) else { return XCTFail("missing stack descriptor") }
        XCTAssertNotNil(stack["propsSchema"])
        XCTAssertNotNil(stack["example"])
        XCTAssertEqual(stack["libraryVersion"], .number(1))
    }

    func testComponentDescriptorsCarryContractMetadata() {
        let descriptor = try! XCTUnwrap(ComponentCatalog.descriptors.first(where: { $0.type == "decision" }))
        XCTAssertEqual(descriptor.libraryVersion, ComponentCatalog.version)
        XCTAssertEqual(descriptor.example.type, "decision")
        XCTAssertNotNil(descriptor.propsSchema["question"])
    }
}
