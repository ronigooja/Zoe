import Foundation

public protocol SilenceClock { func now() -> Date }

public struct SystemSilenceClock: SilenceClock {
    public init() {}
    public func now() -> Date { Date() }
}

@MainActor public final class SilenceController {
    public enum TickResult: Equatable, Sendable { case none, timedOut }

    public var timeoutSeconds: TimeInterval
    public var isEnabled: Bool
    private let clock: SilenceClock
    private var lastVoiceActivity: Date?
    private var interactionInProgress = false
    private var didTimeOut = false

    public init(timeoutSeconds: TimeInterval = 60, isEnabled: Bool = true, clock: SilenceClock = SystemSilenceClock()) {
        self.timeoutSeconds = timeoutSeconds
        self.isEnabled = isEnabled
        self.clock = clock
    }

    public func noteVoiceActivity() {
        lastVoiceActivity = clock.now()
        didTimeOut = false
    }

    public func setInteractionInProgress(_ inProgress: Bool) {
        if inProgress == interactionInProgress { return }
        interactionInProgress = inProgress
        if !inProgress, lastVoiceActivity != nil { lastVoiceActivity = clock.now() }
    }

    public func tick() -> TickResult {
        guard isEnabled, !interactionInProgress, !didTimeOut, let lastVoiceActivity else { return .none }
        guard clock.now().timeIntervalSince(lastVoiceActivity) >= timeoutSeconds else { return .none }
        didTimeOut = true
        return .timedOut
    }

    public func reset() {
        lastVoiceActivity = nil
        didTimeOut = false
        interactionInProgress = false
    }
}
