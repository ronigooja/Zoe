#if os(macOS)
import SwiftUI
import ZoeKit

public struct MacSettingsView: View {
    @ObservedObject var model: ZoeAppModel
    @State private var address = ""
    @State private var key = ""

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
        }
        .frame(width: 520, height: 360)
        .onAppear { address = model.serverAddress }
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
