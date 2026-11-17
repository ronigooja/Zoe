#if os(macOS)
import SwiftUI
import ZoeKit

public struct MacSettingsView: View {
    @ObservedObject var model: ZoeAppModel
    @State private var address = ""
    @State private var key = ""
    @State private var silenceSeconds = ""

    public init(model: ZoeAppModel) { self.model = model }

    public var body: some View {
        TabView {
            Form {
                TextField("Server address", text: $address)
                SecureField("Instance key", text: $key)
                HStack {
                    Button(model.isConnecting ? "Connecting…" : "Connect") {
                        Task {
                            if await model.connect(serverAddress: address, key: key) { key = "" }
                        }
                    }
                    Button("Clear saved key", role: .destructive) {
                        Task { await model.clearCredential(); address = model.serverAddress; key = "" }
                    }
                }.disabled(model.isBusy || model.isConnecting)
                LabeledContent("Connection", value: connectionStatus)
                if let notice = model.notice { Text(notice).font(.caption).foregroundStyle(.secondary) }
                if let pending = model.pendingConnectionImport {
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
                if let error = model.connectionImportError {
                    Text(error).font(.caption).foregroundStyle(.red)
                    Button("Dismiss") { model.dismissConnectionImportError() }
                }
            }
            .padding(20)
            .tabItem { Text("Connection") }
            Form {
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
                Toggle("Hang up after silence", isOn: Binding(
                    get: { model.silenceHangupEnabled },
                    set: { model.setSilenceHangupEnabled($0) }
                ))
                TextField("Seconds", text: $silenceSeconds)
                    .onSubmit { applySilenceSeconds() }
                    .onChange(of: secondsFocused) { _, focused in
                        if !focused { applySilenceSeconds() }
                    }
                    .focused($secondsFocused)
            }
            .padding(20)
            .tabItem { Text("General") }
        }
        .frame(width: 520, height: 360)
        .onAppear { address = model.serverAddress; refreshSilenceSeconds() }
        .onChange(of: model.silenceTimeoutSeconds) { _, _ in refreshSilenceSeconds() }
        .task(id: model.connectionEpoch) { await model.refreshVoiceReadiness() }
    }

    @FocusState private var secondsFocused: Bool

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
}
#endif
