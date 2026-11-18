import SwiftUI
import ZoeKit

@MainActor
final class ConversationTextDraft: ObservableObject {
    @Published var text = "" {
        didSet {
            if text != oldValue { key = nil; status = nil }
        }
    }
    @Published private(set) var sending = false
    @Published private(set) var status: String?
    private(set) var key: String?

    static func isValid(_ text: String) -> Bool {
        !text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty && text.utf8.count <= 20_000
    }

    func send(using submit: (String, String) async throws -> Void) async {
        guard !sending, Self.isValid(text) else { return }
        let submittedText = text
        let submittedKey = key ?? UUID().uuidString.lowercased()
        key = submittedKey
        sending = true
        status = "Sending…"
        do {
            try await submit(submittedText, submittedKey)
            if text == submittedText && key == submittedKey {
                text = ""
                key = nil
                status = "Accepted. It is sent now if a call is active, otherwise when the next call starts."
            }
        } catch {
            if text == submittedText && key == submittedKey {
                if let error = error as? ZoeError, case let .http(code, _, _, _) = error, [400, 401, 403, 404, 409, 410, 413].contains(code) {
                    status = "Server rejected the text (\(code))."
                } else if let error = error as? ZoeError, case .persistence = error {
                    status = "Could not confirm or save the submission. Try again."
                } else {
                    status = "Not confirmed yet. It will be retried."
                }
            }
        }
        sending = false
    }
}

public struct ConversationTextInputView: View {
    @ObservedObject private var model: ZoeAppModel
    @StateObject private var draft = ConversationTextDraft()
    private let onCancel: () -> Void

    public init(model: ZoeAppModel, onCancel: @escaping () -> Void) {
        self.model = model
        self.onCancel = onCancel
    }

    public var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            TextEditor(text: $draft.text)
                .accessibilityLabel("Conversation text")
                .border(.secondary)
            let remaining = 20_000 - draft.text.utf8.count
            Text(remaining >= 0 ? "\(remaining) bytes remaining" : "\(-remaining) bytes over the limit")
                .accessibilityLabel(remaining >= 0 ? "\(remaining) bytes remaining" : "\(-remaining) bytes over the limit")
            if !model.canSubmitConversationText { Text("Connect to the server before sending.") }
            if let status = draft.status { Text(status).accessibilityLabel(status) }
            HStack {
                Spacer()
                Button("Cancel", action: onCancel)
                Button("Send") { send() }
                    .keyboardShortcut(.return, modifiers: .command)
                    .disabled(!model.canSubmitConversationText || draft.sending || !ConversationTextDraft.isValid(draft.text))
            }
        }
        .padding()
    }

    private func send() {
        Task { await draft.send { text, key in try await model.submitConversationText(text, idempotencyKey: key) } }
    }
}
