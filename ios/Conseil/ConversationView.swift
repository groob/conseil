import SwiftUI

struct ConversationView: View {
    @EnvironmentObject private var model: AppModel

    let conversationID: String

    @State private var conversation: Conversation?
    @State private var events: [TraceEvent] = []
    @State private var activeRuns: [AgentRun] = []
    @State private var terminalRunIDs: Set<String> = []
    @State private var draft = ""
    @State private var isSubmitting = false
    @State private var showingTrace = false
    @State private var errorMessage: String?

    private var entries: [ChatEntry] {
        chatEntries(from: events)
    }

    private var isBusy: Bool {
        isSubmitting || activeRuns.contains { !terminalRunIDs.contains($0.id) }
    }

    var body: some View {
        VStack(spacing: 0) {
            transcript
            Divider()
            composer
        }
        .navigationTitle(conversation?.title ?? "Conversation")
        .navigationBarTitleDisplayMode(.inline)
        .task { await reload() }
        .task(id: activeRuns.map(\.id)) { await watchFirstActiveRun() }
        .toolbar {
            ToolbarItem(placement: .topBarTrailing) {
                Button("Trace", systemImage: "list.bullet.rectangle") {
                    showingTrace = true
                }
            }
        }
        .sheet(isPresented: $showingTrace) {
            NavigationStack {
                TraceView(events: events)
            }
        }
        .alert("Conseil", isPresented: errorBinding) {
            Button("OK") { errorMessage = nil }
        } message: {
            Text(errorMessage ?? "")
        }
    }

    private var transcript: some View {
        ScrollViewReader { proxy in
            ScrollView {
                LazyVStack(spacing: 12) {
                    ForEach(entries) { entry in
                        MessageBubble(entry: entry)
                            .id(entry.id)
                    }
                    if isBusy {
                        HStack {
                            ProgressView()
                            Text(isSubmitting ? "Sending" : "Agent running")
                                .font(.caption)
                                .foregroundStyle(.secondary)
                            Spacer()
                        }
                        .accessibilityIdentifier("run-progress")
                        .id("running")
                    }
                }
                .padding()
            }
            .onChange(of: entries.last?.id) { _, id in
                guard let id else { return }
                withAnimation { proxy.scrollTo(id, anchor: .bottom) }
            }
            .onChange(of: isBusy) { _, busy in
                if busy {
                    withAnimation { proxy.scrollTo("running", anchor: .bottom) }
                }
            }
        }
    }

    private var composer: some View {
        HStack(alignment: .bottom, spacing: 10) {
            TextField("Message", text: $draft, axis: .vertical)
                .accessibilityIdentifier("message-field")
                .lineLimit(1...6)
                .textFieldStyle(.roundedBorder)
            Button("Send", systemImage: "arrow.up.circle.fill") {
                send()
            }
            .accessibilityIdentifier("send-button")
            .labelStyle(.iconOnly)
            .font(.title2)
            .disabled(isBusy || draft.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
        }
        .padding()
        .background(.bar)
    }

    private var errorBinding: Binding<Bool> {
        Binding(
            get: { errorMessage != nil },
            set: { if !$0 { errorMessage = nil } }
        )
    }

    @MainActor
    private func reload() async {
        do {
            let trace = try await model.conversation(id: conversationID)
            conversation = trace.conversation
            events = trace.events.sorted(by: { $0.id < $1.id })
            activeRuns = trace.activeRuns
            terminalRunIDs.formIntersection(Set(activeRuns.map(\.id)))
        } catch is CancellationError {
        } catch {
            errorMessage = error.localizedDescription
        }
    }

    private func send() {
        let content = draft.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !content.isEmpty else { return }
        isSubmitting = true

        Task {
            defer { isSubmitting = false }
            do {
                let run = try await model.sendMessage(content, conversationID: conversationID)
                if draft.trimmingCharacters(in: .whitespacesAndNewlines) == content {
                    draft = ""
                }
                terminalRunIDs.remove(run.id)
                if !activeRuns.contains(where: { $0.id == run.id }) {
                    activeRuns.append(run)
                }
            } catch is CancellationError {
            } catch {
                errorMessage = error.localizedDescription
            }
        }
    }

    @MainActor
    private func watchFirstActiveRun() async {
        guard let run = activeRuns.first(where: { !terminalRunIDs.contains($0.id) }) else {
            return
        }
        let afterEventID = events.lazy
            .filter { $0.runID == run.id }
            .map(\.id)
            .max() ?? 0
        do {
            try await model.streamRun(id: run.id, after: afterEventID) { event in
                merge(event)
            }
            await model.loadConversations()
            await reload()
        } catch is CancellationError {
        } catch {
            errorMessage = error.localizedDescription
            await reload()
        }
    }

    private func merge(_ event: TraceEvent) {
        if event.isTerminal, let runID = event.runID {
            terminalRunIDs.insert(runID)
        }
        if let index = events.firstIndex(where: { $0.id == event.id }) {
            events[index] = event
        } else {
            events.append(event)
            events.sort(by: { $0.id < $1.id })
        }
    }
}

private struct MessageBubble: View {
    let entry: ChatEntry

    var body: some View {
        HStack {
            if entry.role == .user {
                Spacer(minLength: 44)
            }
            Text(entry.content)
                .textSelection(.enabled)
                .padding(.horizontal, 14)
                .padding(.vertical, 10)
                .background(background, in: RoundedRectangle(cornerRadius: 16, style: .continuous))
                .foregroundStyle(foreground)
            if entry.role != .user {
                Spacer(minLength: 44)
            }
        }
    }

    private var background: Color {
        switch entry.role {
        case .user:
            return .accentColor
        case .assistant:
            return Color(.secondarySystemBackground)
        case .system:
            return Color.orange.opacity(0.18)
        }
    }

    private var foreground: Color {
        entry.role == .user ? .white : .primary
    }
}
