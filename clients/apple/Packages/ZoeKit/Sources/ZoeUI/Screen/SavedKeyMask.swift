import Foundation

/// Display-only summary of the saved instance key. The full key never leaves the store.
enum SavedKeyMask {
    static let minimumLengthForSuffix = 16
    static let suffixLength = 4

    /// Placeholder for the instance key field: the title when nothing is saved, otherwise masked dots.
    static func text(for key: String?) -> String {
        guard let key else { return "Instance key" }
        guard key.count >= minimumLengthForSuffix else { return "••••" }
        return "••••" + String(key.suffix(suffixLength))
    }
}
