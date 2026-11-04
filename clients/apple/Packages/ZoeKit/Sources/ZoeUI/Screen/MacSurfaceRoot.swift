#if os(macOS)
import SwiftUI
import ZoeKit

public struct MacSurfaceRoot: View {
    @ObservedObject var model: ZoeAppModel
    @ObservedObject var coordinator: ScreenCoordinator
    @State private var address = ""
    @State private var key = ""
    @State private var draft = ""

    public init(model: ZoeAppModel, coordinator: ScreenCoordinator) {
        self.model = model
        self.coordinator = coordinator
        _address = State(initialValue: model.serverAddress)
        _draft = State(initialValue: model.selectedTaskDraft)
    }

    public var body: some View {
        Group {
            if !model.authentication.isConnected { ConnectionSurface(model: model, address: $address, key: $key).frame(width: 460, height: 380) }
            else { AmbientPlaceholder(model: model, coordinator: coordinator, draft: $draft).frame(minWidth: 520, idealWidth: 960, minHeight: 360, idealHeight: 640) }
        }
        .onAppear { address = model.serverAddress; coordinator.start(); if model.authentication.isConnected { coordinator.openDefault() } }
        .onChange(of: model.authentication) { _, state in if state.isConnected { coordinator.openDefault() } }
        .onChange(of: model.tasks) { _, _ in if model.authentication.isConnected { coordinator.openDefault() } }
        .onChange(of: model.approvals) { _, _ in if model.authentication.isConnected { coordinator.openDefault() } }
        .onChange(of: model.notifications) { _, _ in if model.authentication.isConnected { coordinator.openDefault() } }
        .onChange(of: model.voiceStatus) { _, status in coordinator.surfaces.markVoiceActive(status.isActive) }
    }
}

private struct ConnectionSurface: View {
    @ObservedObject var model: ZoeAppModel
    @Binding var address: String
    @Binding var key: String
    @State private var connecting = false

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text("Connect to Zoe").font(.title2.bold())
            Text("Enter your server address and instance key to connect.").font(.subheadline).foregroundStyle(.secondary)
            TextField("Server address", text: $address)
            SecureField("Instance key", text: $key)
            HStack { Button(connecting ? "Connecting…" : "Connect") { connecting = true; Task { _ = await model.connect(serverAddress: address, key: key); connecting = false } }.buttonStyle(.borderedProminent).disabled(connecting); if let notice = model.notice { Text(notice).font(.caption).foregroundStyle(.red) } }
            if let pending = model.pendingConnectionImport { HStack { Text("Installation link ready").font(.caption); Button("Use") { Task { _ = await model.connectImportedConnection(pending) } }; Button("Dismiss") { model.dismissConnectionImport() } } }
            if let error = model.connectionImportError { Text(error).font(.caption).foregroundStyle(.red) }
        }
        .frame(maxWidth: 420)
        .padding(28)
    }
}

private struct AmbientPlaceholder: View {
    @ObservedObject var model: ZoeAppModel
    @ObservedObject var coordinator: ScreenCoordinator
    @Binding var draft: String

    var body: some View {
        ZStack(alignment: .bottomTrailing) {
            Color.clear
            if coordinator.textInputVisible { TextInputBar(model: model, coordinator: coordinator, draft: $draft).padding(16) }
            else { Button { coordinator.showTextInput() } label: { Image(systemName: "text.cursor").padding(8) }.buttonStyle(.plain).opacity(0.45).padding(16) }
            if let state = coordinator.sendState { Text(state).font(.caption).padding(8).background(.thinMaterial, in: Capsule()).padding(.trailing, 16).padding(.bottom, 60) }
            if coordinator.aiChange != nil { HStack(spacing: 8) { Text("AI changed a setting").font(.caption); Button("Undo") { coordinator.undoAIChange() }.buttonStyle(.plain) }.padding(8).background(.thinMaterial, in: Capsule()).padding(.trailing, 16).padding(.bottom, 92) }
        }
        .background(.clear)
        .onAppear { coordinator.openDefault() }
    }
}

private struct TextInputBar: View {
    @ObservedObject var model: ZoeAppModel
    @ObservedObject var coordinator: ScreenCoordinator
    @Binding var draft: String

    var body: some View {
        HStack { TextField("What should Zoe do?", text: $draft).onSubmit { coordinator.setDraft(draft); coordinator.sendDraft() }; Button("Send") { coordinator.setDraft(draft); coordinator.sendDraft() }.keyboardShortcut(.return, modifiers: [.command]); Button { coordinator.hideTextInput() } label: { Image(systemName: "xmark") }.buttonStyle(.plain) }.textFieldStyle(.roundedBorder).frame(maxWidth: 520)
            .onAppear { draft = model.selectedTaskDraft }
            .onChange(of: model.selectedTaskDraft) { _, value in draft = value }
            .onExitCommand { coordinator.hideTextInput() }
    }
}
#endif
