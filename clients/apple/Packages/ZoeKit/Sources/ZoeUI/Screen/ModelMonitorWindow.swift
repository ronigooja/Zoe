#if os(macOS)
import AppKit
import Combine
import SwiftUI
import ZoeKit

/// A monitor tab. Overview shows every event; a component tab shows only that Zoe component.
public enum ModelMonitorTab: Hashable, Sendable {
    case overview
    case component(String)
}

/// Owns one reusable monitor subscription. It subscribes only while the window is presented and follows the instance's channel.
@MainActor public final class ModelMonitorModel: ObservableObject {
    @Published public private(set) var status: ModelMonitorStatus = .idle
    @Published public private(set) var visibleEntries: [ModelMonitorEntry] = []
    @Published public private(set) var droppedCount = 0
    @Published public private(set) var serverErrorSeen = false
    @Published public private(set) var selectedTab: ModelMonitorTab = .overview
    /// False while the user has scrolled away from the end of the selected tab. New entries do not move the list then.
    @Published public private(set) var isAtBottom = true
    /// Entries that arrived in each known tab while it was not followed. Cleared when the user returns to the end of that tab.
    @Published public private(set) var unseenCounts: [ModelMonitorTab: Int] = [:]
    /// Short label per call ID, unique across the whole buffer so one call has the same label in every tab.
    @Published public private(set) var callLabels: [String: String] = [:]

    private var buffer = ModelMonitorBuffer()
    private var presented = false
    private var channel: ModelMonitorChannel?
    private var consumer: Task<Void, Never>?
    private var generation = 0
    private var scrollFollow = ModelMonitorFollow()
    private static let knownTabs: [ModelMonitorTab] = [.overview] + ModelMonitorChannel.components.map { .component($0) }

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
        let updates = await next.begin()
        consumer = Task { [weak self] in
            for await update in updates {
                guard let self else { return }
                self.apply(update)
            }
        }
    }

    public func retry() async {
        await channel?.retry()
    }

    /// Switching tabs returns the new tab to the end, so its latest entries are visible.
    public func select(_ tab: ModelMonitorTab) {
        selectedTab = tab
        scrollFollow.reset()
        isAtBottom = true
        unseenCounts[tab] = nil
        publish()
    }

    /// Jump control: returns the selected tab to the end and clears its unseen count.
    public func jumpToLatest() {
        scrollFollow.jump()
        isAtBottom = true
        unseenCounts[selectedTab] = nil
    }

    /// Geometry from the timeline. Ignored when it belongs to a tab other than the selected one.
    public func measure(tab: ModelMonitorTab, contentHeight: Double, distanceFromBottom: Double) {
        guard tab == selectedTab else { return }
        scrollFollow.measure(contentHeight: contentHeight, distanceFromBottom: distanceFromBottom)
        isAtBottom = scrollFollow.isAtBottom
        if isAtBottom { unseenCounts[selectedTab] = nil }
    }

    /// Count for the Jump control. Clamped because buffer trimming can remove entries that were already counted.
    public var unseenInSelectedTab: Int {
        min(unseenCounts[selectedTab] ?? 0, visibleEntries.count)
    }

    private func tearDown() async {
        consumer?.cancel()
        consumer = nil
        let old = channel
        channel = nil
        buffer.clear()
        unseenCounts = [:]
        scrollFollow.reset()
        isAtBottom = true
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
            countUnseen(entry)
            if isVisible(entry) {
                // Empty tab and first entry: a visible append starts following from the end.
                if visibleEntries.isEmpty { scrollFollow.reset() }
                scrollFollow.noteAppend()
            }
        case .serverError:
            serverErrorSeen = true
        case .reset:
            buffer.clear()
            unseenCounts = [:]
        }
        droppedCount = buffer.droppedCount
        publish()
    }

    private func isVisible(_ entry: ModelMonitorEntry) -> Bool {
        switch entry.kind {
        case .gap: true
        case let .event(event): selectedComponent == nil || event.component == selectedComponent
        }
    }

    /// Bounded: only known tabs are counted. Unknown components stay visible in Overview but get no tab count.
    private func countUnseen(_ entry: ModelMonitorEntry) {
        let tabs: [ModelMonitorTab]
        switch entry.kind {
        case .gap:
            tabs = Self.knownTabs
        case let .event(event):
            var known: [ModelMonitorTab] = [ModelMonitorTab.overview]
            if ModelMonitorChannel.components.contains(event.component) {
                known.append(ModelMonitorTab.component(event.component))
            }
            tabs = known
        }
        for tab in tabs where !(tab == selectedTab && isAtBottom) {
            unseenCounts[tab, default: 0] += 1
        }
    }

    private var selectedComponent: String? {
        if case let .component(component) = selectedTab { component } else { nil }
    }

    private func publish() {
        visibleEntries = buffer.visibleEntries(component: selectedComponent)
        callLabels = ModelMonitorCallID.shortLabels(for: buffer.entries.compactMap { entry in
            if case let .event(event) = entry.kind { event.callID } else { nil }
        })
    }
}

public struct ModelMonitorView: View {
    @ObservedObject var model: ModelMonitorModel
    private static let bottomID = "model-monitor-bottom"
    private static let scrollSpace = "model-monitor-scroll"

    public init(model: ModelMonitorModel) { self.model = model }

    public var body: some View {
        VStack(spacing: 0) {
            // Only actionable states stay in the body; the live state is shown in the window title.
            if let actionText = actionableStatusText {
                HStack(spacing: 8) {
                    Text(actionText).font(.callout)
                    Spacer()
                    if model.status == .ended {
                        Button(uiText("Retry")) { Task { await model.retry() } }
                    }
                }
                .padding(8)
            }
            tabBar
            Divider()
            timeline
            if model.serverErrorSeen {
                Text(uiText("Zoe reported a monitor error. The subscription continues."))
                    .font(.caption).foregroundStyle(.red).padding(4)
            }
            if model.droppedCount > 0 {
                Text(uiFormat("%d older entries were removed from this view.", model.droppedCount))
                    .font(.caption).foregroundStyle(.secondary).padding(4)
            }
        }
        .frame(minWidth: 320, minHeight: 360)
    }

    /// Window title for the host window. The live state is shown here, not in the body.
    public static func windowTitle(for status: ModelMonitorStatus) -> String {
        uiFormat("Model Monitor · %@", statusLabel(status))
    }

    /// Overview is pinned; component tabs fill two rows in stable order and scroll horizontally on narrow windows.
    private var tabBar: some View {
        let components = ModelMonitorChannel.components
        let split = (components.count + 1) / 2
        return HStack(spacing: 4) {
            tabButton(.overview, label: uiText("Overview"))
            Divider()
            ScrollView(.horizontal) {
                VStack(alignment: .leading, spacing: 4) {
                    HStack(spacing: 4) {
                        ForEach(Array(components.prefix(split)), id: \.self) { component in
                            tabButton(.component(component), label: componentLabel(component))
                        }
                    }
                    HStack(spacing: 4) {
                        ForEach(Array(components.dropFirst(split)), id: \.self) { component in
                            tabButton(.component(component), label: componentLabel(component))
                        }
                    }
                }
            }
        }
        .padding(.horizontal, 8).padding(.bottom, 4)
    }

    private func tabButton(_ tab: ModelMonitorTab, label: String) -> some View {
        let selected = model.selectedTab == tab
        return Button { model.select(tab) } label: {
            Text(label).font(.callout)
                .padding(.horizontal, 8).padding(.vertical, 4)
                .background(selected ? Color.accentColor.opacity(0.2) : Color.clear, in: RoundedRectangle(cornerRadius: 6))
        }
        .buttonStyle(.plain)
        .fixedSize()
        .accessibilityAddTraits(selected ? .isSelected : [])
    }

    @ViewBuilder private var timeline: some View {
        if model.visibleEntries.isEmpty {
            Text(uiText("No events yet.")).foregroundStyle(.secondary)
                .frame(maxWidth: .infinity, maxHeight: .infinity)
        } else {
            monitorList
                // A new view per tab: geometry from the outgoing tab's list cannot reach the new tab.
                .id(model.selectedTab)
        }
    }

    private var monitorList: some View {
        let tab = model.selectedTab
        return ScrollViewReader { proxy in
            ScrollView {
                VStack(alignment: .leading, spacing: 8) {
                    ForEach(model.visibleEntries) { entry in
                        row(entry)
                    }
                    Color.clear.frame(height: 1).id(Self.bottomID)
                }
                .padding(8)
                .background(GeometryReader { geometry in
                    Color.clear.preference(key: ScrollMetricsKey.self, value: ScrollMetrics(
                        contentHeight: Double(geometry.size.height),
                        contentBottom: Double(geometry.frame(in: .named(Self.scrollSpace)).maxY)))
                })
            }
            .coordinateSpace(name: Self.scrollSpace)
            .background(GeometryReader { geometry in
                Color.clear.preference(key: ScrollMetricsKey.self, value: ScrollMetrics(viewportHeight: Double(geometry.size.height)))
            })
            .onPreferenceChange(ScrollMetricsKey.self) { metrics in
                // Both values come from the same layout pass, so the distance is never computed from a stale viewport.
                guard let viewportHeight = metrics.viewportHeight, let contentHeight = metrics.contentHeight,
                      let contentBottom = metrics.contentBottom else { return }
                model.measure(tab: tab, contentHeight: contentHeight, distanceFromBottom: contentBottom - viewportHeight)
            }
            .defaultScrollAnchor(.bottom)
            .onAppear { proxy.scrollTo(Self.bottomID, anchor: .bottom) }
            .onChange(of: model.visibleEntries.last?.id) { _, _ in
                if model.isAtBottom { proxy.scrollTo(Self.bottomID, anchor: .bottom) }
            }
            .overlay(alignment: .bottom) {
                if !model.isAtBottom {
                    Button {
                        model.jumpToLatest()
                        proxy.scrollTo(Self.bottomID, anchor: .bottom)
                    } label: {
                        Label(jumpTitle, systemImage: "arrow.down.circle.fill").font(.callout)
                    }
                    .buttonStyle(.borderedProminent)
                    .controlSize(.small)
                    .padding(8)
                }
            }
        }
    }

    private var jumpTitle: String {
        let count = model.unseenInSelectedTab
        return count > 0 ? uiFormat("%d new · Jump to latest", count) : uiText("Jump to latest")
    }

    @ViewBuilder private func row(_ entry: ModelMonitorEntry) -> some View {
        switch entry.kind {
        case .gap:
            Label(uiText("Events may be missing here."), systemImage: "exclamationmark.triangle")
                .font(.caption).foregroundStyle(.orange)
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(.vertical, 4)
        case let .event(event):
            ModelMonitorEventRow(
                event: event,
                placement: placement(event.kind),
                componentLabel: componentLabel(event.component),
                kindLabel: kindLabel(event.kind)
            )
        }
    }

    /// Model replies sit left, Zoe requests right, and errors and tool activity span the full width.
    private func placement(_ kind: ModelMonitorEventKind) -> ModelMonitorEventRow.Placement {
        switch kind {
        case .response: .leading
        case .request: .trailing
        case .error, .toolCall, .toolResult: .fullWidth
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

    /// Only states the user must act on or that explain missing data stay in the body.
    private var actionableStatusText: String? {
        switch model.status {
        case .reconnecting, .ended, .unauthorized: Self.statusLabel(model.status)
        case .idle, .connecting, .subscribing, .live: nil
        }
    }

    static func statusLabel(_ status: ModelMonitorStatus) -> String {
        switch status {
        case .idle: uiText("Not monitoring")
        case .connecting: uiText("Connecting…")
        case .subscribing: uiText("Subscribing…")
        case .live: uiText("Live")
        case .reconnecting: uiText("Reconnecting. Events during the gap were not received.")
        case .ended: uiText("Monitoring ended. Retry to resume.")
        case .unauthorized: uiText("The instance key was rejected. Replace it in Settings to monitor.")
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

/// One event card. The payload is collapsed to a preview; the preview states how much is hidden, and expansion shows the full text.
private struct ModelMonitorEventRow: View {
    enum Placement { case leading, trailing, fullWidth }

    let event: ModelMonitorEvent
    let placement: Placement
    let componentLabel: String
    let kindLabel: String
    @State private var expanded = false

    var body: some View {
        HStack(spacing: 0) {
            if placement == .trailing { Spacer(minLength: 40) }
            card
            if placement == .leading { Spacer(minLength: 40) }
        }
    }

    private var card: some View {
        VStack(alignment: .leading, spacing: 4) {
            // Metadata is selectable text, so standard Cmd+C copies it; it has no copy button.
            HStack(spacing: 6) {
                Text(kindLabel).font(.caption.weight(.semibold))
                    .foregroundStyle(event.kind == .error ? Color.red : Color.secondary)
                    .textSelection(.enabled)
                Text(componentLabel).font(.caption2)
                    .padding(.horizontal, 6).padding(.vertical, 1)
                    .background(Color.primary.opacity(0.08), in: Capsule())
                    .textSelection(.enabled)
                if let time = event.time {
                    Text(time.formatted(date: .omitted, time: .standard)).font(.caption2).foregroundStyle(.secondary)
                        .textSelection(.enabled)
                }
            }
            HStack(spacing: 6) {
                Text(event.callID).font(.caption2.monospaced()).foregroundStyle(.secondary)
                    .lineLimit(1).truncationMode(.middle)
                    .textSelection(.enabled)
                    .help(event.callID)
                if event.truncated {
                    // Server truncation: Zoe shortened the payload before it reached the client.
                    Label(uiText("Truncated by Zoe"), systemImage: "scissors")
                        .font(.caption).foregroundStyle(.orange)
                }
            }
            payload
        }
        .padding(8)
        .frame(maxWidth: placement == .fullWidth ? .infinity : nil, alignment: .leading)
        .background(background, in: RoundedRectangle(cornerRadius: 8))
    }

    @ViewBuilder private var payload: some View {
        // Display shows readable text; Copy still copies the complete formatted JSON.
        if let payload = event.payload, let full = event.formattedPayload {
            let shown = ModelMonitorPayloadText.render(payload)
            let preview = ModelMonitorPreview(shown)
            Text(expanded || !preview.isCut ? shown : preview.text)
                .font(.caption.monospaced()).textSelection(.enabled)
                .frame(maxWidth: .infinity, alignment: .leading)
            if preview.isCut && !expanded {
                // UI preview: this is not a server truncation.
                Text(uiFormat("Preview shows the first %d of %d characters.", preview.shownCount, preview.totalCount))
                    .font(.caption2).foregroundStyle(.secondary)
            }
            HStack(spacing: 12) {
                if preview.isCut {
                    Button(expanded ? uiText("Show less") : uiFormat("Show full payload (%d characters)", preview.totalCount)) {
                        expanded.toggle()
                    }
                }
                Button(uiText("Copy")) {
                    NSPasteboard.general.clearContents()
                    NSPasteboard.general.setString(full, forType: .string)
                }
            }
            .buttonStyle(.link)
            .font(.caption)
        }
    }

    private var background: Color {
        switch event.kind {
        case .request: Color.accentColor.opacity(0.12)
        case .response: Color.secondary.opacity(0.12)
        case .error: Color.red.opacity(0.12)
        case .toolCall, .toolResult: Color.primary.opacity(0.05)
        }
    }
}

/// Timeline geometry from one layout pass. Each field is set only by its own source view; `reduce` merges them.
private struct ScrollMetrics: Equatable {
    var viewportHeight: Double?
    var contentHeight: Double?
    /// Content bottom edge in the scroll view's coordinates; the viewport bottom is `viewportHeight`.
    var contentBottom: Double?

    init(viewportHeight: Double? = nil) { self.viewportHeight = viewportHeight }

    init(contentHeight: Double, contentBottom: Double) {
        self.contentHeight = contentHeight
        self.contentBottom = contentBottom
    }
}

private struct ScrollMetricsKey: PreferenceKey {
    static let defaultValue = ScrollMetrics()
    static func reduce(value: inout ScrollMetrics, nextValue: () -> ScrollMetrics) {
        let next = nextValue()
        value.viewportHeight = next.viewportHeight ?? value.viewportHeight
        value.contentHeight = next.contentHeight ?? value.contentHeight
        value.contentBottom = next.contentBottom ?? value.contentBottom
    }
}

/// Collapsed view of a payload. `text` is a display preview only; `isCut` tells the reader that more text exists.
private struct ModelMonitorPreview {
    static let maxLines = 12
    static let maxCharacters = 600

    let text: String
    let totalCount: Int

    var shownCount: Int { text.count }
    var isCut: Bool { text.count < totalCount }

    init(_ full: String) {
        totalCount = full.count
        var head = String(full.prefix(Self.maxCharacters))
        let lines = head.split(separator: "\n", omittingEmptySubsequences: false)
        if lines.count > Self.maxLines {
            head = lines.prefix(Self.maxLines).joined(separator: "\n")
        }
        text = head
    }
}
#endif
