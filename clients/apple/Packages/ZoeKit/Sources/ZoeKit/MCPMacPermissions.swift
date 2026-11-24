import ApplicationServices
import CoreGraphics

public struct MCPMacPermissions: MCPPermissionChecking {
    public init() {}

    public func missingPermission(for entry: ClientMCPEntry) -> MCPPermissionReason? {
        guard entry.id == ClientMCPEntry.agentDesktop.id else { return nil }
        guard AXIsProcessTrusted() else { return .accessibility }
        guard CGPreflightScreenCaptureAccess() else { return .screenRecording }
        return nil
    }
}
