import Foundation
import A2UISwiftCore

public enum DefaultScreenBuilder {
    public static func build(tasks: [TaskRecord], approvals: [Approval], notifications: [NotificationRecord]) -> [A2uiMessage] {
        struct Item {
            let id: String
            let date: String
            let title: String
            let status: String
        }
        var items = tasks.map { Item(id: "task.\($0.id)", date: $0.updatedAt, title: $0.title, status: $0.status.label) }
        items += approvals.map { Item(id: "approval.\($0.id)", date: $0.createdAt, title: $0.prompt, status: $0.status) }
        items += notifications.map { Item(id: "notification.\($0.id)", date: $0.createdAt, title: $0.title, status: $0.kind) }
        items.sort { $0.date == $1.date ? $0.id < $1.id : $0.date > $1.date }
        var components = [RawComponent(id: "root", component: "List", properties: [
            "children": .array(items.map { .string($0.id) })
        ])]
        for item in items {
            components += [
                RawComponent(id: item.id, component: "Card", properties: ["child": .string("\(item.id).content")]),
                RawComponent(id: "\(item.id).content", component: "Column", properties: [
                    "children": .array([.string("\(item.id).title"), .string("\(item.id).status")])
                ]),
                RawComponent(id: "\(item.id).title", component: "Text", properties: ["text": .string(item.title)]),
                RawComponent(id: "\(item.id).status", component: "Text", properties: ["text": .string(item.status)])
            ]
        }
        return [
            .createSurface(CreateSurfacePayload(surfaceId: "main", catalogId: basicCatalogId, sendDataModel: false)),
            .updateComponents(UpdateComponentsPayload(surfaceId: "main", components: components))
        ]
    }
}
