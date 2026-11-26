#if os(macOS)
import Combine
import SwiftUI
import ZoeKit

/// Owns one reusable monitor subscription. It subscribes only while the window is presented and follows the instance's channel.
@MainActor public final class ModelMonitorModel: ObservableObject {
    @Published public private(set) var status: ModelMonitorStatus = .idle
    @Published public private(set) var visibleEntries: [ModelMonitorEntry] = []
    @Published public private(set) var droppedCount = 0
    @Published public private(set) var serverErrorSeen = false
    @Published public private(set) var enabledComponents = Set(ModelMonitorChannel.components)

    private var buffer = ModelMonitorBuffer()
    private var presented = false
    private var channel: ModelMonitorChannel?
    private var consumer: Task<Void, Never>?
    private var generation = 0

    public init() {}

    public func present(_ channel: ModelMonitorChannel?) async {
        presented = true
        await follow(channel)
    }

    public func dismiss() async {
        presented = false
        await follow(nil)
    }

    /// Called when the instance's channel changes. A nil or different channel clears all private state first.
    public func follow(_ next: ModelMonitorChannel?) async {
        guard presented || next == nil else { return }
        if next != nil, next === channel, consumer != nil { return }
        generation += 1
        let current = generation
        await tearDown()
        guard presented, current == generation, let next else { return }
        channel = next
        let updates = await next.makeUpdates()
        consumer = Task { [weak self] in
            for await update in updates {
                guard let self else { return }
                self.apply(update)
            }
        }
        await next.start()
    }

    public func retry() async {
        await channel?.retry()
    }

    public func setComponent(_ component: String, enabled: Bool) {
        if enabled { enabledComponents.insert(component) } else { enabledComponents.remove(component) }
        publish()
    }

    private func tearDown() async {
        consumer?.cancel()
        consumer = nil
        let old = channel
        channel = nil
        buffer.clear()
        droppedCount = 0
        serverErrorSeen = false
        status = .idle
        publish()
        await old?.stop()
    }

    private func apply(_ update: ModelMonitorUpdate) {
        switch update {
        case let .status(next):
            status = next
        case let .entry(entry):
            buffer.append(entry)
        case .serverError:
            serverErrorSeen = true
        case .reset:
            buffer.clear()
        }
        droppedCount = buffer.droppedCount
        publish()
    }

    private func publish() {
        visibleEntries = buffer.visible(components: enabledComponents)
    }
}

public struct ModelMonitorView: View {
    @ObservedObject var model: ModelMonitorModel

    public init(model: ModelMonitorModel) { self.model = model }

    public var body: some View {
        VStack(spacing: 0) {
            HStack(spacing: 8) {
                Text(statusText).font(.callout)
                Spacer()
                if model.status == .ended {
                    Button(uiText("Retry")) { Task { await model.retry() } }
                }
                Menu(uiText("Components")) {
                    ForEach(ModelMonitorChannel.components, id: \.self) { component in
                        Toggle(componentLabel(component), isOn: Binding(
                            get: { model.enabledComponents.contains(component) },
                            set: { model.setComponent(component, enabled: $0) }
                        ))
                    }
                }
                .fixedSize()
            }
            .padding(8)
            Text(uiText("Worker and OpenCode entries show what Zoe observed, not raw provider payloads. Memory entries appear only when Zoe publishes them."))
                .font(.caption).foregroundStyle(.secondary)
                .padding(.horizontal, 8).padding(.bottom, 6)
            Divider()
            if model.visibleEntries.isEmpty {
                Text(uiText("No events yet.")).foregroundStyle(.secondary)
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
            } else {
                ScrollView {
                    LazyVStack(alignment: .leading, spacing: 0) {
                        ForEach(model.visibleEntries) { entry in
                            row(entry)
                            Divider()
                        }
                    }
                    .padding(8)
                }
            }
            if model.serverErrorSeen {
                Text(uiText("Zoe reported a monitor error. The subscription continues."))
                    .font(.caption).foregroundStyle(.red).padding(4)
            }
            if model.droppedCount > 0 {
                Text(uiFormat("%d older entries were removed from this view.", model.droppedCount))
                    .font(.caption).foregroundStyle(.secondary).padding(4)
            }
        }
        .frame(minWidth: 480, minHeight: 360)
    }

    @ViewBuilder private func row(_ entry: ModelMonitorEntry) -> some View {
        switch entry.kind {
        case .gap:
            Label(uiText("Events may be missing here."), systemImage: "exclamationmark.triangle")
                .font(.caption).foregroundStyle(.orange)
                .padding(.vertical, 4)
        case let .event(event):
            VStack(alignment: .leading, spacing: 3) {
                HStack(spacing: 6) {
                    Text(kindLabel(event.kind)).font(.headline)
                    Text(componentLabel(event.component)).font(.caption).foregroundStyle(.secondary)
                    if let time = event.time {
                        Text(time.formatted(date: .omitted, time: .standard)).font(.caption).foregroundStyle(.secondary)
                    }
                    Spacer()
                    if event.truncated {
                        Text(uiText("Truncated")).font(.caption).foregroundStyle(.orange)
                    }
                }
                Text(event.callID).font(.caption2.monospaced()).foregroundStyle(.secondary).textSelection(.enabled)
                if let text = event.payloadText(limit: 4000) {
                    Text(text).font(.caption.monospaced()).textSelection(.enabled)
                        .frame(maxWidth: .infinity, alignment: .leading)
                }
            }
            .padding(.vertical, 6)
        }
    }

    private var statusText: String {
        switch model.status {
        case .idle: uiText("Not monitoring")
        case .connecting: uiText("Connecting…")
        case .subscribing: uiText("Subscribing…")
        case .live: uiText("Live")
        case .reconnecting: uiText("Reconnecting. Events during the gap were not received.")
        case .ended: uiText("Monitoring ended. Retry to resume.")
        case .unauthorized: uiText("The instance key was rejected. Replace it in Settings to monitor.")
        }
    }

    private func kindLabel(_ kind: ModelMonitorEventKind) -> String {
        switch kind {
        case .request: uiText("Request")
        case .response: uiText("Reply")
        case .error: uiText("Error")
        case .toolCall: uiText("Tool call")
        case .toolResult: uiText("Tool result")
        }
    }

    private func componentLabel(_ component: String) -> String {
        switch component {
        case "register": uiText("Register")
        case "compact": uiText("Compact")
        case "steward": uiText("Steward")
        case "ui_controller": uiText("UI controller")
        case "task_summary": uiText("Task summary")
        case "worker": uiText("Worker")
        case "worker_rotation": uiText("Worker rotation")
        case "memory_write": uiText("Memory write")
        case "memory_ingestion": uiText("Memory ingestion")
        default: component
        }
    }
}
#endif
