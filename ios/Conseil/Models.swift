import Foundation

struct Conversation: Codable, Hashable, Identifiable, Sendable {
    let id: String
    let title: String
    let createdAt: String
    let updatedAt: String

    enum CodingKeys: String, CodingKey {
        case id, title
        case createdAt = "created_at"
        case updatedAt = "updated_at"
    }
}

struct AgentRun: Codable, Hashable, Identifiable, Sendable {
    let id: String
    let conversationID: String
    let parentRunID: String?
    let agent: String
    let model: String
    let instructions: String
    let status: String
    let error: String?
    let createdAt: String
    let startedAt: String?
    let finishedAt: String?

    enum CodingKeys: String, CodingKey {
        case id, agent, model, instructions, status, error
        case conversationID = "conversation_id"
        case parentRunID = "parent_run_id"
        case createdAt = "created_at"
        case startedAt = "started_at"
        case finishedAt = "finished_at"
    }
}

struct TraceEvent: Codable, Hashable, Identifiable, Sendable {
    let id: Int64
    let conversationID: String
    let runID: String?
    let type: String
    let payload: JSONValue
    let createdAt: String

    enum CodingKeys: String, CodingKey {
        case id, type, payload
        case conversationID = "conversation_id"
        case runID = "run_id"
        case createdAt = "created_at"
    }

    var isTerminal: Bool {
        type == "run.completed" || type == "run.failed" || type == "run.interrupted"
    }
}

enum JSONValue: Codable, Hashable, Sendable {
    case object([String: JSONValue])
    case array([JSONValue])
    case string(String)
    case number(Double)
    case bool(Bool)
    case null

    init(from decoder: Decoder) throws {
        let container = try decoder.singleValueContainer()
        if container.decodeNil() {
            self = .null
        } else if let value = try? container.decode(Bool.self) {
            self = .bool(value)
        } else if let value = try? container.decode(Double.self) {
            self = .number(value)
        } else if let value = try? container.decode(String.self) {
            self = .string(value)
        } else if let value = try? container.decode([String: JSONValue].self) {
            self = .object(value)
        } else if let value = try? container.decode([JSONValue].self) {
            self = .array(value)
        } else {
            throw DecodingError.dataCorruptedError(in: container, debugDescription: "Unsupported JSON value")
        }
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.singleValueContainer()
        switch self {
        case let .object(value):
            try container.encode(value)
        case let .array(value):
            try container.encode(value)
        case let .string(value):
            try container.encode(value)
        case let .number(value):
            try container.encode(value)
        case let .bool(value):
            try container.encode(value)
        case .null:
            try container.encodeNil()
        }
    }

    func string(for key: String) -> String? {
        guard case let .object(value) = self, case let .string(result) = value[key] else {
            return nil
        }
        return result
    }

    var formatted: String {
        guard JSONSerialization.isValidJSONObject(foundationValue),
              let data = try? JSONSerialization.data(
                withJSONObject: foundationValue,
                options: [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes]
              ),
              let text = String(data: data, encoding: .utf8)
        else {
            return String(describing: foundationValue)
        }
        return text
    }

    private var foundationValue: Any {
        switch self {
        case let .object(value):
            return value.mapValues(\.foundationValue)
        case let .array(value):
            return value.map(\.foundationValue)
        case let .string(value):
            return value
        case let .number(value):
            return value
        case let .bool(value):
            return value
        case .null:
            return NSNull()
        }
    }
}

struct ConversationListResponse: Codable, Sendable {
    let conversations: [Conversation]
}

struct ConversationResponse: Codable, Sendable {
    let conversation: Conversation
}

struct ConversationTraceResponse: Codable, Sendable {
    let conversation: Conversation
    let events: [TraceEvent]
    let activeRuns: [AgentRun]

    enum CodingKeys: String, CodingKey {
        case conversation, events
        case activeRuns = "active_runs"
    }
}

struct RunResponse: Codable, Sendable {
    let run: AgentRun
}

struct ErrorResponse: Codable, Sendable {
    let error: String
}

struct ChatEntry: Hashable, Identifiable, Sendable {
    enum Role: Hashable, Sendable {
        case user
        case assistant
        case system
    }

    let id: String
    let role: Role
    let content: String
    let eventID: Int64
}

func chatEntries(from events: [TraceEvent]) -> [ChatEntry] {
    let completedRuns = Set(events.compactMap { event in
        event.type == "assistant.message" ? event.runID : nil
    })
    var entries: [ChatEntry] = []
    var liveDeltas: [String: (eventID: Int64, content: String)] = [:]

    for event in events.sorted(by: { $0.id < $1.id }) {
        switch event.type {
        case "user.message":
            if let content = event.payload.string(for: "content") {
                entries.append(ChatEntry(id: "event-\(event.id)", role: .user, content: content, eventID: event.id))
            }
        case "assistant.message":
            if let content = event.payload.string(for: "content") {
                entries.append(ChatEntry(id: "event-\(event.id)", role: .assistant, content: content, eventID: event.id))
            }
        case "assistant.delta":
            guard let runID = event.runID,
                  !completedRuns.contains(runID),
                  let delta = event.payload.string(for: "delta")
            else {
                continue
            }
            let current = liveDeltas[runID]
            liveDeltas[runID] = (current?.eventID ?? event.id, (current?.content ?? "") + delta)
        case "run.failed", "run.interrupted":
            if let error = event.payload.string(for: "error") {
                entries.append(ChatEntry(id: "event-\(event.id)", role: .system, content: error, eventID: event.id))
            }
        default:
            continue
        }
    }

    for (runID, delta) in liveDeltas {
        entries.append(ChatEntry(id: "live-\(runID)", role: .assistant, content: delta.content, eventID: delta.eventID))
    }
    return entries.sorted(by: { $0.eventID < $1.eventID })
}
