import Combine
import Foundation

@MainActor
final class AppModel: ObservableObject {
    @Published private(set) var serverURL: String
    @Published private(set) var token: String
    @Published private(set) var conversations: [Conversation] = []
    @Published private(set) var isLoading = false
    @Published var errorMessage: String?

    private let serverURLKey = "conseil.server-url"

    init() {
        serverURL = UserDefaults.standard.string(forKey: serverURLKey) ?? "https://yolk-adze.exe.xyz"
        do {
            token = try KeychainStore.token() ?? ""
        } catch {
            token = ""
            errorMessage = error.localizedDescription
        }
    }

    var isConfigured: Bool {
        (try? configuration()) != nil
    }

    func saveSettings(serverURL: String, token: String) throws {
        let normalizedURL = serverURL.trimmingCharacters(in: .whitespacesAndNewlines).trimmingCharacters(in: CharacterSet(charactersIn: "/"))
        let normalizedToken = token.trimmingCharacters(in: .whitespacesAndNewlines)
        _ = try configuration(serverURL: normalizedURL, token: normalizedToken)
        try KeychainStore.setToken(normalizedToken)
        UserDefaults.standard.set(normalizedURL, forKey: serverURLKey)
        self.serverURL = normalizedURL
        self.token = normalizedToken
    }

    func loadConversations() async {
        guard isConfigured else {
            return
        }
        isLoading = true
        defer { isLoading = false }
        do {
            conversations = try await client().listConversations()
        } catch {
            errorMessage = error.localizedDescription
        }
    }

    func createConversation() async throws -> Conversation {
        let conversation = try await client().createConversation()
        conversations.removeAll(where: { $0.id == conversation.id })
        conversations.insert(conversation, at: 0)
        return conversation
    }

    func conversation(id: String) async throws -> ConversationTraceResponse {
        try await client().conversation(id: id)
    }

    func sendMessage(_ content: String, conversationID: String) async throws -> AgentRun {
        try await client().sendMessage(content, conversationID: conversationID)
    }

    func streamRun(
        id: String,
        after eventID: Int64 = 0,
        onEvent: @escaping @MainActor @Sendable (TraceEvent) -> Void
    ) async throws {
        try await client().streamRunEvents(runID: id, after: eventID, onEvent: onEvent)
    }

    private func client() throws -> APIClient {
        APIClient(configuration: try configuration())
    }

    private func configuration() throws -> APIConfiguration {
        try configuration(serverURL: serverURL, token: token)
    }

    private func configuration(serverURL: String, token: String) throws -> APIConfiguration {
        guard let url = URL(string: serverURL) else {
            throw APIClientError.invalidServerURL
        }
        return try APIConfiguration(baseURL: url, token: token)
    }
}
