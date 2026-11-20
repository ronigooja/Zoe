import Foundation

/// Local presentation policy; it never changes server-owned task or surface data.
struct SurfaceClosePolicy {
    static func validatedDelay(_ seconds: Double) -> Double {
        seconds.isFinite ? min(3600, max(0, seconds)) : 60
    }

    static func shouldDismissPopover(isEscape: Bool) -> Bool {
        isEscape
    }

    static func deadline(lostFocusAt: Date?, delay: Double, taskQuestion: Bool) -> Date? {
        guard !taskQuestion, delay > 0, let lostFocusAt else { return nil }
        return lostFocusAt.addingTimeInterval(delay)
    }

    static func shouldReplay(taskQuestion: Bool, wasPresented: Bool) -> Bool {
        taskQuestion || wasPresented
    }
}
