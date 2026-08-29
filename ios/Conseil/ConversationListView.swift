import SwiftUI

struct ConversationListView: View {
    @EnvironmentObject private var model: AppModel

    @State private var path: [String] = []
    @State private var showingSettings = false
    @State private var isCreating = false

    var body: some View {
        NavigationStack(path: $path) {
            List(model.conversations) { conversation in
                NavigationLink(value: conversation.id) {
                    VStack(alignment: .leading, spacing: 4) {
                        Text(conversation.title)
                            .lineLimit(2)
                        Text(conversation.updatedAt)
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                }
            }
            .overlay {
                if model.conversations.isEmpty && !model.isLoading {
                    ContentUnavailableView(
                        "No conversations",
                        systemImage: "bubble.left.and.bubble.right",
                        description: Text("Start one from the compose button.")
                    )
                }
            }
            .navigationTitle("Conseil")
            .navigationDestination(for: String.self) { conversationID in
                ConversationView(conversationID: conversationID)
            }
            .refreshable { await model.loadConversations() }
            .task { await model.loadConversations() }
            .toolbar {
                ToolbarItem(placement: .topBarLeading) {
                    Button("Settings", systemImage: "gearshape") {
                        showingSettings = true
                    }
                }
                ToolbarItem(placement: .topBarTrailing) {
                    Button("New conversation", systemImage: "square.and.pencil") {
                        createConversation()
                    }
                    .accessibilityIdentifier("new-conversation-button")
                    .disabled(isCreating)
                }
            }
            .sheet(isPresented: $showingSettings) {
                NavigationStack {
                    SettingsView(required: false)
                }
            }
            .alert("Conseil", isPresented: errorBinding) {
                Button("OK") { model.errorMessage = nil }
            } message: {
                Text(model.errorMessage ?? "")
            }
        }
    }

    private var errorBinding: Binding<Bool> {
        Binding(
            get: { model.errorMessage != nil },
            set: { if !$0 { model.errorMessage = nil } }
        )
    }

    private func createConversation() {
        isCreating = true
        Task {
            defer { isCreating = false }
            do {
                let conversation = try await model.createConversation()
                path.append(conversation.id)
            } catch {
                model.errorMessage = error.localizedDescription
            }
        }
    }
}
