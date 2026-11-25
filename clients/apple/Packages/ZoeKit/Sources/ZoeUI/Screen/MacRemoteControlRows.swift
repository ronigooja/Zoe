#if os(macOS)
import SwiftUI
import ZoeKit

enum RemoteControlIdleTimeout {
    static let range = 1...1440

    static func clampedMinutes(_ value: Int) -> Int {
        min(max(value, range.lowerBound), range.upperBound)
    }

    static func minutes(fromSeconds seconds: TimeInterval) -> Int {
        guard seconds.isFinite else { return range.lowerBound }
        let minutes = min(max((seconds / 60).rounded(), Double(range.lowerBound)), Double(range.upperBound))
        return Int(minutes)
    }

    static func seconds(fromMinutes minutes: Int) -> TimeInterval {
        TimeInterval(clampedMinutes(minutes) * 60)
    }
}

struct MacRemoteControlRows: View {
    @ObservedObject var model: RemoteControlModel

    var body: some View {
        ForEach(model.rows) { row in
            MacRemoteControlRow(model: model, row: row)
        }
    }
}

private struct MacRemoteControlRow: View {
    @ObservedObject var model: RemoteControlModel
    let row: RemoteControlModel.Row
    @State private var idleMinutes = ""
    @FocusState private var minutesFocused: Bool

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack {
                Toggle(isOn: Binding(
                    get: { row.enabled },
                    set: { value in Task { await model.setEnabled(value, id: row.id) } }
                )) {
                    Text(String(format: uiText("Enable %@"), row.title))
                }
                .disabled(row.status == .starting)
                Spacer()
                TextField(uiText("Auto-off after (minutes)"), text: $idleMinutes)
                    .frame(width: 150)
                    .onSubmit { applyIdleMinutes() }
                    .onChange(of: minutesFocused) { _, focused in
                        if !focused { applyIdleMinutes() }
                    }
                    .focused($minutesFocused)
                Text(statusText)
                    .font(.caption).foregroundStyle(.secondary)
                    .frame(width: 110, alignment: .trailing)
            }
            if let reason = row.permissionNeeded {
                Text(permissionText(reason))
                    .font(.caption).foregroundStyle(.secondary)
            }
            if let error = row.lastError {
                Text(error)
                    .font(.caption).foregroundStyle(.red)
            }
            if row.launchOptions.keys.contains("headed") {
                Toggle(isOn: Binding(
                    get: { row.headed },
                    set: { value in Task { await model.setLaunchOption("headed", value: value, id: row.id) } }
                )) {
                    Text("Use real mouse and keyboard (headed)", bundle: .module)
                }
                Text("Headed mode moves your real mouse and takes keyboard focus.", bundle: .module)
                    .font(.caption).foregroundStyle(.secondary)
            }
        }
        .onAppear { refreshIdleMinutes() }
        .onChange(of: row.idleTimeoutSeconds) { _, _ in refreshIdleMinutes() }
    }

    private var statusText: String {
        switch row.status {
        case .off: uiText("Off")
        case .starting: uiText("Starting")
        case .on: uiText("On")
        case .permissionNeeded: uiText("Permission needed")
        case .error: uiText("Error")
        }
    }

    private func permissionText(_ reason: MCPPermissionReason) -> String {
        switch reason {
        case .accessibility: uiText("Grant Zoe Accessibility access in System Settings > Privacy & Security > Accessibility, then try again.")
        case .screenRecording: uiText("Grant Zoe screen recording access in System Settings > Privacy & Security > Screen & System Audio Recording, then try again.")
        }
    }

    private func applyIdleMinutes() {
        if let minutes = Int(idleMinutes.trimmingCharacters(in: .whitespacesAndNewlines)) {
            let clamped = RemoteControlIdleTimeout.clampedMinutes(minutes)
            idleMinutes = String(clamped)
            let seconds = RemoteControlIdleTimeout.seconds(fromMinutes: clamped)
            Task { await model.setIdleTimeout(seconds, id: row.id) }
        } else {
            refreshIdleMinutes()
        }
    }

    private func refreshIdleMinutes() {
        idleMinutes = String(RemoteControlIdleTimeout.minutes(fromSeconds: row.idleTimeoutSeconds))
    }
}
#endif
