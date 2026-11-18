#if os(macOS)
import SwiftUI
import ZoeKit

public struct MacSettingsView: View {
    @ObservedObject var model: ZoeAppModel
    @State private var address = ""
    @State private var key = ""
    @State private var silenceSeconds = ""
    @State private var confirmingClearKey = false
    @FocusState private var secondsFocused: Bool

    public init(model: ZoeAppModel) { self.model = model }

    public var body: some View {
        TabView {
            Form {
                Section("Voice") {
                    if let config = model.voiceConfig {
                        Picker("Voice", selection: Binding(
                            get: { model.preferredVoice ?? "" },
                            set: { model.setPreferredVoice($0.isEmpty ? nil : $0) }
                        )) {
                            Text("Gateway default").tag("")
                            ForEach(config.voices) { voice in Text(voice.name).tag(voice.id) }
                            if let saved = model.preferredVoice, !config.voices.contains(where: { $0.id == saved }) {
                                Text("Saved voice unavailable").tag(saved)
                            }
                        }
                        Text("Applies to new calls. Unavailable voices use the gateway default.")
                            .font(.caption).foregroundStyle(.secondary)
                    } else {
                        Text("Connect to load available voices.").foregroundStyle(.secondary)
                    }
                }
                Section("Silence") {
                    Toggle("Hang up after silence", isOn: Binding(
                        get: { model.silenceHangupEnabled },
                        set: { model.setSilenceHangupEnabled($0) }
                    ))
                    if model.silenceHangupEnabled {
                        TextField("Silence timeout (seconds)", text: $silenceSeconds)
                            .onSubmit { applySilenceSeconds() }
                            .onChange(of: secondsFocused) { _, focused in
                                if !focused { applySilenceSeconds() }
                            }
                            .focused($secondsFocused)
                    }
                }
                Section {
                    TextField(uiText("Popover close delay (seconds)"), value: Binding(
                        get: { model.popoverCloseDelaySeconds }, set: { model.setPopoverCloseDelaySeconds($0) }
                    ), format: .number)
                    TextField(uiText("Window close delay (seconds)"), value: Binding(
                        get: { model.windowCloseDelaySeconds }, set: { model.setWindowCloseDelaySeconds($0) }
                    ), format: .number)
                } header: {
                    Text("Screen")
                } footer: {
                    VStack(alignment: .leading, spacing: 4) {
                        Text(uiText("After focus loss, 0 means never, and the maximum is 3600 seconds."))
                        Text(uiText("Pending questions never auto-close; outside clicks and Esc close popovers immediately, and the popover delay applies only to other focus loss."))
                    }
                }
            }
            .formStyle(.grouped)
            .tabItem { Label("General", systemImage: "gearshape") }

            Form {
                Section("Server") {
                    TextField("Server address", text: $address)
                    SecureField("Instance key", text: $key)
                    HStack {
                        Button {
                            Task {
                                if await model.connect(serverAddress: address, key: key) { key = "" }
                            }
                        } label: {
                            HStack {
                                if model.isConnecting { ProgressView().controlSize(.small) }
                                Text(model.isConnecting ? "Connecting…" : "Connect")
                            }
                        }
                        .buttonStyle(.borderedProminent)
                        .keyboardShortcut(.defaultAction)
                        Button("Clear saved key", role: .destructive) { confirmingClearKey = true }
                    }
                    .disabled(model.isBusy || model.isConnecting)
                }
                Section("Status") {
                    LabeledContent("Connection") {
                        HStack(spacing: 6) {
                            Circle().fill(connectionStatusColor).frame(width: 8, height: 8)
                            Text(connectionStatus)
                        }
                    }
                    if let notice = model.notice { Text(notice).font(.caption).foregroundStyle(.secondary) }
                }
                if let pending = model.pendingConnectionImport {
                    Section {
                        Text("Installation link ready: \(pending.serverURL.absoluteString)").font(.caption)
                        HStack {
                            Button("Use") {
                                Task {
                                    if await model.connectImportedConnection(pending) { address = model.serverAddress; key = "" }
                                }
                            }
                            Button("Dismiss") { model.dismissConnectionImport() }
                        }.disabled(model.isBusy || model.isConnecting)
                    }
                }
                if let error = model.connectionImportError {
                    Section {
                        Text(error).font(.caption).foregroundStyle(.red)
                        Button("Dismiss") { model.dismissConnectionImportError() }
                    }
                }
            }
            .formStyle(.grouped)
            .tabItem { Label("Connection", systemImage: "network") }
        }
        .frame(minWidth: 520, idealWidth: 520, maxWidth: 520, minHeight: 420, idealHeight: 540)
        .confirmationDialog("Clear saved key?", isPresented: $confirmingClearKey, titleVisibility: .visible) {
            Button("Clear saved key", role: .destructive) {
                Task { await model.clearCredential(); address = model.serverAddress; key = "" }
            }
            Button("Cancel", role: .cancel) {}
        } message: {
            Text("The saved instance key will be removed from this Mac.")
        }
        .onAppear { address = model.serverAddress; refreshSilenceSeconds() }
        .onChange(of: model.silenceTimeoutSeconds) { _, _ in refreshSilenceSeconds() }
        .task(id: model.connectionEpoch) { await model.refreshVoiceReadiness() }
    }

    private func applySilenceSeconds() {
        if let seconds = Double(silenceSeconds.trimmingCharacters(in: .whitespacesAndNewlines)), seconds.isFinite {
            model.setSilenceTimeoutSeconds(seconds)
        }
        refreshSilenceSeconds()
    }

    private func refreshSilenceSeconds() {
        let seconds = model.silenceTimeoutSeconds
        silenceSeconds = seconds.rounded() == seconds ? String(Int(seconds)) : String(seconds)
    }

    private var connectionStatus: String {
        switch model.authentication {
        case .checking: "Checking"
        case .connected: "Connected"
        case .disconnected: "Not connected"
        case .unavailable: "Unavailable"
        }
    }

    private var connectionStatusColor: Color {
        switch model.authentication {
        case .connected: .green
        case .unavailable: .red
        case .checking, .disconnected: .secondary
        }
    }
}
#endif
