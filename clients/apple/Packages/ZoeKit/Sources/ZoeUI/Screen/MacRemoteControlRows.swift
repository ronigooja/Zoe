#if os(macOS)
import SwiftUI
import AppKit
import ZoeKit

enum PermissionGuidance {
    static func text(for reason: MCPPermissionReason, operatingSystemVersion: OperatingSystemVersion) -> String {
        switch reason {
        case .accessibility:
            if operatingSystemVersion.majorVersion >= 27 {
                return uiText("Turn on Zoe in System Settings > Privacy & Security > Device Control and Data Access, then try again.")
            }
            return uiText("Turn on Zoe in System Settings > Privacy & Security > Accessibility, then try again.")
        case .screenRecording:
            return uiText("Turn on Zoe's screen recording access under System Settings > Privacy & Security. Look for Screen & System Audio Recording; the name may differ by language or macOS version. Then try again.")
        }
    }

    static func settingsURL(for reason: MCPPermissionReason) -> URL {
        let anchor = reason == .accessibility ? "Privacy_Accessibility" : "Privacy_ScreenCapture"
        return URL(string: "x-apple.systempreferences:com.apple.preference.security?\(anchor)")!
    }

    static let settingsRootURL = URL(string: "x-apple.systempreferences:com.apple.preference.security")!
}

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
                Text(statusText)
                    .font(.caption).foregroundStyle(.secondary)
                    .lineLimit(1)
                    .fixedSize(horizontal: true, vertical: false)
                    .frame(minWidth: 130, alignment: .trailing)
            }
            HStack(spacing: 8) {
                Text(uiText("Auto-off after (minutes)"))
                    .font(.caption).foregroundStyle(.secondary)
                TextField("", text: $idleMinutes)
                    .labelsHidden()
                    .accessibilityLabel(uiText("Auto-off after (minutes)"))
                    .frame(width: 70)
                    .onSubmit { applyIdleMinutes() }
                    .onChange(of: minutesFocused) { _, focused in
                        if !focused { applyIdleMinutes() }
                    }
                    .focused($minutesFocused)
                Spacer()
            }
            if let reason = row.permissionNeeded {
                HStack(alignment: .top) {
                    VStack(alignment: .leading, spacing: 4) {
                        Text(PermissionGuidance.text(for: reason, operatingSystemVersion: ProcessInfo.processInfo.operatingSystemVersion))
                        Text("After granting access, if Zoe still shows permission needed, quit and reopen Zoe. A restart may be needed.", bundle: .module)
                    }
                    .font(.caption).foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
                    Button {
                        if !NSWorkspace.shared.open(PermissionGuidance.settingsURL(for: reason)) {
                            NSWorkspace.shared.open(PermissionGuidance.settingsRootURL)
                        }
                    } label: {
                        Text("Open System Settings", bundle: .module)
                    }
                    .controlSize(.small)
                    .accessibilityLabel(uiText("Open System Settings"))
                }
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
