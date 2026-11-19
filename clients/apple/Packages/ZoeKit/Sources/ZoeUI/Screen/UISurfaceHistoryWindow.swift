#if os(macOS)
import A2UISwiftCore
import A2UISwiftUI
import AppKit
import Combine
import SwiftUI
import ZoeKit

/// Read-only history list with pages of 50, lazy loading at the end, delete, and clear-all with confirmation.
@MainActor public final class UISurfaceHistoryModel: ObservableObject {
    @Published public private(set) var entries: [UISurfaceHistoryEntry] = []
    @Published public private(set) var hasMore = false
    @Published public private(set) var selected: UISurfaceHistoryRecord?
    @Published public var notice: String?

    private let store: UISurfaceHistoryStore
    private let scope: () -> UISurfaceHistoryScope?
    private var loading = false
    /// A reload starts a new generation; results from an older generation are discarded.
    private var generation = 0

    public init(store: UISurfaceHistoryStore, scope: @escaping () -> UISurfaceHistoryScope?) {
        self.store = store
        self.scope = scope
    }

    public func reload() async {
        generation += 1
        entries = []
        hasMore = false
        loading = false
        await loadNextPage()
    }

    public func loadNextPage() async {
        guard let scope = scope(), !loading, hasMore || entries.isEmpty else { return }
        loading = true
        let current = generation
        defer { if current == generation { loading = false } }
        do {
            let page = try await store.page(scope: scope, offset: entries.count)
            guard current == generation else { return }
            entries += page.entries
            hasMore = page.hasMore
        } catch {
            guard current == generation else { return }
            notice = uiText("History could not be read.")
            hasMore = false
        }
    }

    /// Content is read only here, when the user opens an entry.
    public func open(_ entry: UISurfaceHistoryEntry) async {
        guard let scope = scope() else { return }
        do {
            selected = try await store.record(scope: scope, id: entry.id)
        } catch {
            notice = uiText("This history entry could not be opened.")
        }
    }

    public func close() { selected = nil }

    public func delete(_ entry: UISurfaceHistoryEntry) async {
        guard let scope = scope() else { return }
        do {
            try await store.delete(scope: scope, id: entry.id)
            entries.removeAll { $0.id == entry.id }
        } catch {
            notice = uiText("This history entry could not be deleted.")
        }
    }

    public func clearAll() async {
        guard let scope = scope() else { return }
        do {
            try await store.clearAll(scope: scope)
            await reload()
        } catch {
            notice = uiText("History could not be cleared.")
        }
    }
}

public struct UISurfaceHistoryView: View {
    @ObservedObject var model: UISurfaceHistoryModel
    @State private var confirmingClear = false

    public init(model: UISurfaceHistoryModel) {
        self.model = model
    }

    public var body: some View {
        Group {
            if let record = model.selected {
                VStack(alignment: .leading, spacing: 0) {
                    HStack {
                        Button(uiText("Back")) { model.close() }
                        Spacer()
                        Text(uiText("History record, not interactive")).font(.caption).foregroundStyle(.secondary)
                    }
                    .padding(8)
                    Divider()
                    ReadOnlyRecordView(record: record)
                }
            } else {
                list
            }
        }
        .frame(minWidth: 360, minHeight: 300)
        .task { await model.reload() }
        .alert(uiText("Clear all history?"), isPresented: $confirmingClear) {
            SwiftUI.Button(uiText("Clear All"), role: .destructive) { Task { await model.clearAll() } }
            SwiftUI.Button(uiText("Cancel"), role: .cancel) {}
        } message: {
            Text(uiText("This removes every saved history entry for this server and workspace."))
        }
    }

    private var list: some View {
        VStack(spacing: 0) {
            HStack {
                Spacer()
                SwiftUI.Button(uiText("Clear All…")) { confirmingClear = true }
                    .disabled(model.entries.isEmpty)
            }
            .padding(8)
            Divider()
            if model.entries.isEmpty {
                Text(uiText("No history yet.")).foregroundStyle(.secondary).frame(maxWidth: .infinity, maxHeight: .infinity)
            } else {
                ScrollView {
                    LazyVStack(alignment: .leading, spacing: 0) {
                        ForEach(model.entries) { entry in
                            row(entry)
                                .onAppear {
                                    if entry.id == model.entries.last?.id {
                                        Task { await model.loadNextPage() }
                                    }
                                }
                        }
                    }
                }
            }
            if let notice = model.notice {
                Text(notice).font(.caption).foregroundStyle(.red).padding(4)
            }
        }
    }

    private func row(_ entry: UISurfaceHistoryEntry) -> some View {
        HStack {
            Button {
                Task { await model.open(entry) }
            } label: {
                VStack(alignment: .leading, spacing: 2) {
                    Text(entry.title).lineLimit(1)
                    Text(entry.time.formatted(date: .abbreviated, time: .shortened))
                        .font(.caption).foregroundStyle(.secondary)
                }
                .frame(maxWidth: .infinity, alignment: .leading)
            }
            .buttonStyle(.plain)
            SwiftUI.Button(role: .destructive) {
                Task { await model.delete(entry) }
            } label: {
                Image(systemName: "trash")
            }
            .buttonStyle(.borderless)
            .help(uiText("Delete"))
        }
        .padding(.horizontal, 10)
        .padding(.vertical, 6)
        .contentShape(Rectangle())
    }
}

/// Renders a saved record with the same catalog as live surfaces, but every control is disabled.
struct ReadOnlyRecordView: View {
    let record: UISurfaceHistoryRecord
    @State private var model: SurfaceViewModel?

    var body: some View {
        ScrollView {
            if let model {
                A2UISurfaceView(viewModel: model, scrolls: false)
                    .a2uiCatalogItems(SurfaceCatalogOverrides.items(tracker: nil))
                    .textSelection(.enabled)
                    .padding(8)
            }
        }
        .task(id: record.id) {
            let view = SurfaceViewModel(catalog: basicCatalog)
            view.processMessages(record.messages)
            view.a2uiStyle = SurfaceManager.fixedStyle
            model = view
        }
    }
}
#endif
