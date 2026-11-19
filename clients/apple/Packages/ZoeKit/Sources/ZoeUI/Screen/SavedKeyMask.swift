import Foundation

/// Display-only summary of the saved instance key. The full key never leaves the store.
enum SavedKeyMask {
    static let minimumLengthForSuffix = 16
    static let suffixLength = 4

    static func text(for key: String?) -> String {
        guard let key else { return "Saved key: none" }
        guard key.count >= minimumLengthForSuffix else { return "Saved key: ••••" }
        return "Saved key: ••••" + String(key.suffix(suffixLength))
    }
}
