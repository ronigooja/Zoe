#if os(macOS)
import SwiftUI
import ZoeKit

public struct MacSettingsView: View {
    @ObservedObject var model: ZoeAppModel
    @ObservedObject var coordinator: ScreenCoordinator
    @State private var address = ""
    @State private var key = ""

    public init(model: ZoeAppModel, coordinator: ScreenCoordinator) {
        self.model = model
        self.coordinator = coordinator
    }

    public var body: some View {
        TabView {
            connection.tabItem { Label("Connection & Key", systemImage: "link") }
            account.tabItem { Label("Account", systemImage: "person") }
            simple("Notifications", "Notification preferences are managed by the system and Zoe inbox.").tabItem { Label("Notifications", systemImage: "bell") }
            simple("Capabilities", "Capability state is authoritative on the connected server.").tabItem { Label("Capabilities", systemImage: "shield") }
            simple("Data", "Local connection data is stored in Zoe’s existing protected storage.").tabItem { Label("Data", systemImage: "externaldrive") }
            aiChanges.tabItem { Label("AI Changes", systemImage: "clock.arrow.circlepath") }
            advanced.tabItem { Label("Advanced", systemImage: "gearshape") }
        }
        .padding(20)
        .frame(width: 620, height: 420)
        .onAppear { address = model.serverAddress }
    }

    private var connection: some View {
        Form {
            Section("Server") { TextField("Server address", text: $address); SecureField("Instance key", text: $key) }
            HStack { Button("Connect") { Task { _ = await model.connect(serverAddress: address, key: key) } }; Button("Clear saved key", role: .destructive) { Task { await model.clearCredential() } } }
            if let notice = model.notice { Text(notice).font(.caption).foregroundStyle(.secondary) }
        }
    }

    private var account: some View {
        Form { LabeledContent("Connection", value: model.authentication.isConnected ? "Connected" : "Not connected"); LabeledContent("Server", value: model.serverAddress.isEmpty ? "—" : model.serverAddress); Toggle("Continuous voice", isOn: Binding(get: { model.continuousVoiceEnabled }, set: { value in model.setContinuousVoiceEnabled(value); try? coordinator.registry.write(key: "voice.enabled", value: .bool(value), by: .user) })) }
    }

    private func simple(_ title: String, _ message: String) -> some View { VStack(alignment: .leading, spacing: 10) { Text(title).font(.title2.bold()); Text(message).foregroundStyle(.secondary); Spacer() }.frame(maxWidth: .infinity, alignment: .leading) }

    private var aiChanges: some View {
        VStack(alignment: .leading) {
            Text("AI Changes").font(.title2.bold())
            if coordinator.registry.audit.isEmpty { Text("No AI setting changes.").foregroundStyle(.secondary) }
            else { List(coordinator.registry.audit) { record in HStack { VStack(alignment: .leading) { Text(record.key); Text(record.at, style: .date).font(.caption).foregroundStyle(.secondary) }; Spacer(); if !record.undone { Button("Undo") { _ = coordinator.registry.undo(recordID: record.id) } } else { Text("Undone").font(.caption).foregroundStyle(.secondary) } } } }
        }
    }

    private var advanced: some View {
        Form {
            ForEach(coordinator.registry.descriptors(), id: \.key) { descriptor in LabeledContent(descriptor.key, value: descriptor.summary) }
        }
    }
}
#endif
