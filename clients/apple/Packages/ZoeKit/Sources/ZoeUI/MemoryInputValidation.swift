import Foundation

public struct MemoryInput: Equatable, Sendable {
    public let subject: String
    public let key: String
    public let content: String

    public init(subject: String, key: String, content: String) {
        self.subject = subject
        self.key = key
        self.content = content
    }
}

public enum MemoryInputValidation {
    public static func validate(subject: String, key: String, content: String) -> MemoryInput? {
        let subject = subject.trimmingCharacters(in: .whitespacesAndNewlines)
        let key = key.trimmingCharacters(in: .whitespacesAndNewlines)
        let content = content.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !subject.isEmpty, !key.isEmpty, !content.isEmpty,
              subject.count <= 80, key.count <= 80, content.count <= 2000 else { return nil }
        return MemoryInput(subject: subject, key: key, content: content)
    }
}
