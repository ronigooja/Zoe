import SwiftUI
import ZoeKit

@MainActor
final class ConversationTextDraft: ObservableObject {
    @Published var text = "" {
        didSet {
            if text != oldValue { key = nil; status = nil; cancelAcceptedStatusClear() }
        }
    }
    @Published private(set) var sending = false
    @Published private(set) var status: String?
    private(set) var key: String?
    private(set) var acceptedStatusClearTask: Task<Void, Never>?
    private let acceptedStatusDelay: Duration

    init(acceptedStatusDelay: Duration = .seconds(2)) {
        self.acceptedStatusDelay = acceptedStatusDelay
    }

    static func isValid(_ text: String) -> Bool {
        !text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty && text.utf8.count <= 20_000
    }

    func send(using submit: (String, String) async throws -> Void) async {
        guard !sending, Self.isValid(text) else { return }
        let submittedText = text
        let submittedKey = key ?? UUID().uuidString.lowercased()
        key = submittedKey
        cancelAcceptedStatusClear()
        sending = true
        status = "Sending…"
        do {
            try await submit(submittedText, submittedKey)
            if text == submittedText && key == submittedKey {
                text = ""
                key = nil
                status = "Accepted."
                scheduleAcceptedStatusClear()
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

    private func scheduleAcceptedStatusClear() {
        let delay = acceptedStatusDelay
        acceptedStatusClearTask = Task { [weak self] in
            try? await Task.sleep(for: delay)
            guard !Task.isCancelled, let self, self.status == "Accepted." else { return }
            self.status = nil
            self.acceptedStatusClearTask = nil
        }
    }

    private func cancelAcceptedStatusClear() {
        acceptedStatusClearTask?.cancel()
        acceptedStatusClearTask = nil
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
                .scrollContentBackground(.hidden)
                .accessibilityLabel("Conversation text")
                .padding(6)
                .frame(minHeight: 120)
                .background(Color(nsColor: .controlBackgroundColor), in: RoundedRectangle(cornerRadius: 8))
                .overlay(RoundedRectangle(cornerRadius: 8).stroke(Color(nsColor: .separatorColor), lineWidth: 1))
            let remaining = 20_000 - draft.text.utf8.count
            Text(remaining >= 0 ? "\(remaining) bytes remaining" : "\(-remaining) bytes over the limit")
                .font(.caption)
                .foregroundStyle(remaining >= 0 ? Color.secondary : Color.red)
                .accessibilityLabel(remaining >= 0 ? "\(remaining) bytes remaining" : "\(-remaining) bytes over the limit")
            HStack(spacing: 6) {
                if draft.sending { ProgressView().controlSize(.small) }
                if let status = draft.status {
                    Text(status).accessibilityLabel(status)
                } else if !model.canSubmitConversationText {
                    Text("Connect to the server before sending.").foregroundStyle(.secondary)
                }
            }
            .frame(maxWidth: .infinity, minHeight: 16, alignment: .leading)
            HStack {
                Spacer()
                Button("Cancel", action: onCancel)
                Button("Send") { send() }
                    .buttonStyle(.borderedProminent)
                    .keyboardShortcut(.return, modifiers: .command)
                    .disabled(!model.canSubmitConversationText || draft.sending || !ConversationTextDraft.isValid(draft.text))
            }
        }
        .padding()
        .frame(width: 520, height: 360)
    }

    private func send() {
        Task { await draft.send { text, key in try await model.submitConversationText(text, idempotencyKey: key) } }
    }
}
