/// A local preference, independent of server-owned surface and task state.
public enum NewUISurfaceBehavior: String, CaseIterable, Sendable {
    case addWindow
    case closeOldWindows

    var title: String {
        switch self {
        case .addWindow: "Add a new window"
        case .closeOldWindows: "Close old windows, then show"
        }
    }
}

struct NewUISurfacePlan: Equatable {
    let closeIDs: Set<String>
    let showID: String

    init(behavior: NewUISurfaceBehavior, presentedIDs: Set<String>, newSurfaceID: String) {
        // Updates to an existing presentation are not arrivals of a new UI.
        closeIDs = behavior == .closeOldWindows && !presentedIDs.contains(newSurfaceID)
            ? presentedIDs : []
        showID = newSurfaceID
    }
}
