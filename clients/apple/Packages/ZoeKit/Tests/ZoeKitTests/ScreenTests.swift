import XCTest
import A2UISwiftCore
@testable import ZoeKit

private final class TestClock: SilenceClock {
    var date: Date
    init(_ date: Date = Date(timeIntervalSince1970: 0)) { self.date = date }
    func now() -> Date { date }
}

@MainActor private final class TestSurfaceHost: SurfaceHost {
    var opened: [SurfaceSpec] = []
    var updated: [(String, [A2uiMessage]?, SurfaceWindowSpec?)] = []
    var closed: [String] = []
    var ids: [String] = []

    func open(_ spec: SurfaceSpec) { opened.append(spec); ids.append(spec.id) }
    func update(id: String, messages: [A2uiMessage]?, window: SurfaceWindowSpec?) { updated.append((id, messages, window)) }
    func close(id: String) { closed.append(id); ids.removeAll { $0 == id } }
    func openSurfaceIDs() -> [String] { ids }
}

@MainActor
final class ScreenTests: XCTestCase {
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

    func testDefaultScreenIsChronologicalWithoutRankingOrInventedContent() throws {
        let task = TaskRecord(id: "t", projectId: nil, title: "Real task", goal: "real goal", source: "user", profileId: nil,
                              status: .active, version: 1, createdAt: "2026-01-01T00:00:00Z", updatedAt: "2026-01-02T00:00:00Z",
                              acceptedAt: nil, startedAt: nil, finishedAt: nil, result: nil, error: nil, evidence: [], canAcceptResult: false)
        let approval = Approval(id: "a", taskId: "t", status: "pending", prompt: "Real approval", version: 1,
                                createdAt: "2026-01-03T00:00:00Z", expiresAt: nil, requestedAction: nil, requestedActionDigest: nil)
        let notification = NotificationRecord(id: "n", kind: "info", title: "Real notice", body: "Real body", aggregateType: "task",
                                              aggregateId: "t", sourceEventId: "event", createdAt: "2026-01-04T00:00:00Z", readAt: nil)
        let messages = DefaultScreenBuilder.build(tasks: [task], approvals: [approval], notifications: [notification])
        guard case let .createSurface(create) = messages[0], case let .updateComponents(update) = messages[1] else { return XCTFail("missing messages") }
        XCTAssertEqual(create.surfaceId, "main")
        XCTAssertEqual(create.catalogId, basicCatalogId)
        XCTAssertFalse(create.sendDataModel)
        XCTAssertEqual(update.components[0].properties["children"], .array([.string("notification.n"), .string("approval.a"), .string("task.t")]))
        XCTAssertEqual(Set(update.components.map(\.id)).count, 13)
        XCTAssertEqual(update.components.first { $0.id == "task.t.title" }?.properties["text"], .string("Real task"))
        let one = DefaultScreenBuilder.build(tasks: [task], approvals: [], notifications: [])
        let json = try JSONDecoder().decode([JSONValue].self, from: JSONEncoder().encode(one))
        guard case let .object(envelope) = json[1], case let .object(payload)? = envelope["updateComponents"],
              case let .array(components)? = payload["components"] else { return XCTFail("invalid encoded components") }
        XCTAssertEqual(components, [
            .object(["id": .string("root"), "component": .string("List"), "children": .array([.string("task.t")])]),
            .object(["id": .string("task.t"), "component": .string("Card"), "child": .string("task.t.content")]),
            .object(["id": .string("task.t.content"), "component": .string("Column"), "children": .array([.string("task.t.title"), .string("task.t.status")])]),
            .object(["id": .string("task.t.title"), "component": .string("Text"), "text": .string("Real task")]),
            .object(["id": .string("task.t.status"), "component": .string("Text"), "text": .string(task.status.label)])
        ])
        var tied = notification
        tied.createdAt = approval.createdAt
        guard case let .updateComponents(tieUpdate) = DefaultScreenBuilder.build(tasks: [], approvals: [approval], notifications: [tied])[1] else { return XCTFail("missing tie update") }
        XCTAssertEqual(tieUpdate.components[0].properties["children"], .array([.string("approval.a"), .string("notification.n")]))
        guard case let .updateComponents(empty) = DefaultScreenBuilder.build(tasks: [], approvals: [], notifications: [])[1] else { return XCTFail("missing empty update") }
        XCTAssertEqual(empty.components, [RawComponent(id: "root", component: "List", properties: ["children": .array([])])])
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

    func testRouterDecodesFlatWindowContractIncludingMenuBar() async throws {
        let host = TestSurfaceHost()
        let router = ScreenToolRouter(host: host, settings: SettingsRegistry(), startVoice: {}, endVoice: {}, setSilenceTimeout: { _ in }, showTextInput: {}, hideTextInput: {})
        let messages = try JSONDecoder().decode(JSONValue.self, from: JSONEncoder().encode(DefaultScreenBuilder.build(tasks: [], approvals: [], notifications: [])))
        let result = await router.handle(tool: "surface.open", args: ["id": .string("surface"), "kind": .string("window"), "messages": messages,
                                                                        "window": .object(["width": .number(400), "height": .number(200), "x": .number(10), "y": .number(20), "floating": .bool(true), "showInMenuBar": .bool(true)])])
        XCTAssertEqual(result, .object(["ok": .bool(true)]))
        XCTAssertEqual(host.opened.first?.window.width, 400)
        XCTAssertEqual(host.opened.first?.window.x, 10)
        XCTAssertEqual(host.opened.first?.window.showInMenuBar, true)

        XCTAssertEqual(host.opened.first?.messages.count, 2)
        let invalid = await router.handle(tool: "surface.open", args: ["id": .string("bad"), "kind": .string("window"), "messages": .array([.object([:])])])
        XCTAssertEqual(invalid, .object(["error": .string("invalid surface messages")]))
        XCTAssertEqual(host.opened.count, 1)
        let updated = await router.handle(tool: "surface.update", args: ["id": .string("surface"), "patch": .object(["messages": messages])])
        XCTAssertEqual(updated, .object(["ok": .bool(true)]))
        XCTAssertEqual(host.updated.first?.1?.count, 2)
        let invalidUpdate = await router.handle(tool: "surface.update", args: ["id": .string("surface"), "messages": .string("invalid")])
        XCTAssertEqual(invalidUpdate, .object(["error": .string("invalid surface messages")]))
    }

    func testResolvedActionPreservesJSONValues() {
        let action = ScreenAction(ResolvedAction(name: "surface.open", sourceComponentId: "button", context: [
            "null": .null, "bool": .bool(true), "integer": .number(42), "fraction": .number(1.5),
            "large": .number(1e30), "string": .string("hello"),
            "array": .array([.null, .dictionary(["nested": .bool(false)])])
        ]))
        XCTAssertEqual(action, ScreenAction(intent: "surface.open", args: [
            "null": .null, "bool": .bool(true), "integer": .integer(42), "fraction": .number(1.5),
            "large": .number(1e30), "string": .string("hello"),
            "array": .array([.null, .object(["nested": .bool(false)])])
        ]))
    }

    func testFirstTextLabelUsesMessageOrderAndUpdates() {
        func update(_ components: [RawComponent]) -> A2uiMessage {
            .updateComponents(UpdateComponentsPayload(surfaceId: "main", components: components))
        }
        let initial = update([
            RawComponent(id: "root", component: "List"),
            RawComponent(id: "first", component: "Text", properties: ["text": .string("First")]),
            RawComponent(id: "second", component: "Text", properties: ["text": .string("Second")])
        ])
        XCTAssertNil(firstTextLabel(in: []))
        XCTAssertEqual(firstTextLabel(in: [initial]), "First")
        XCTAssertEqual(firstTextLabel(in: [initial, update([RawComponent(id: "first", component: "Text", properties: ["text": .string("Changed")])])]), "Changed")
        XCTAssertEqual(firstTextLabel(in: [initial, update([RawComponent(id: "first", component: "", properties: ["text": .string("Partial")])])]), "Partial")
        XCTAssertNil(firstTextLabel(in: [initial, .deleteSurface(DeleteSurfacePayload(surfaceId: "main"))]))
        XCTAssertNil(firstTextLabel(in: [update([RawComponent(id: "dynamic", component: "Text", properties: ["text": .dictionary(["path": .string("/label")])])])]))
    }
}
