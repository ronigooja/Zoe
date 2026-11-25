import ApplicationServices
import CoreGraphics

public struct MCPMacPermissions: MCPPermissionChecking {
    public init() {}

    public func requestPermission(_ reason: MCPPermissionReason) {
        switch reason {
        case .accessibility:
            let options = [kAXTrustedCheckOptionPrompt.takeUnretainedValue() as String: true] as CFDictionary
            _ = AXIsProcessTrustedWithOptions(options)
        case .screenRecording:
            _ = CGRequestScreenCaptureAccess()
        }
    }

    public func missingPermission(for entry: ClientMCPEntry) -> MCPPermissionReason? {
        guard entry.id == ClientMCPEntry.agentDesktop.id else { return nil }
        guard AXIsProcessTrusted() else { return .accessibility }
        guard CGPreflightScreenCaptureAccess() else { return .screenRecording }
        return nil
    }
}
