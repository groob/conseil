import Foundation

struct APIConfiguration: Sendable {
    let baseURL: URL
    let token: String

    init(baseURL: URL, token: String) throws {
        let scheme = baseURL.scheme?.lowercased()
        let host = baseURL.host?.lowercased()
        let isExeHost = scheme == "https" && host?.hasSuffix(".exe.xyz") == true
        let isLocalhost = scheme == "http" && ["localhost", "127.0.0.1", "::1"].contains(host ?? "")
        guard !token.isEmpty,
              baseURL.user == nil,
              baseURL.password == nil,
              isExeHost || isLocalhost
        else {
            throw APIClientError.invalidServerURL
        }
        self.baseURL = baseURL
        self.token = token
    }
}

enum APIClientError: LocalizedError {
    case invalidServerURL
    case invalidResponse
    case http(status: Int, message: String)

    var errorDescription: String? {
        switch self {
        case .invalidServerURL:
            return "The server URL is invalid."
        case .invalidResponse:
            return "The server returned an invalid response."
        case let .http(status, message):
            return message.isEmpty ? "The server returned HTTP \(status)." : message
        }
    }
}

struct APIClient: Sendable {
    private static let defaultSession = URLSession(
        configuration: .default,
        delegate: SameOriginRedirectDelegate(),
        delegateQueue: nil
    )

    let configuration: APIConfiguration
    var session: URLSession

    init(configuration: APIConfiguration, session: URLSession? = nil) {
        self.configuration = configuration
        self.session = session ?? Self.defaultSession
    }

    func listConversations() async throws -> [Conversation] {
        let response: ConversationListResponse = try await get("/v1/conversations")
        return response.conversations
    }

    func createConversation() async throws -> Conversation {
        let response: ConversationResponse = try await send(
            path: "/v1/conversations",
            method: "POST",
            body: EmptyRequest()
        )
        return response.conversation
    }

    func conversation(id: String) async throws -> ConversationTraceResponse {
        try await get("/v1/conversations/\(id)")
    }

    func sendMessage(_ content: String, conversationID: String) async throws -> AgentRun {
        let response: RunResponse = try await send(
            path: "/v1/conversations/\(conversationID)/messages",
            method: "POST",
            body: MessageRequest(content: content)
        )
        return response.run
    }

    func streamRunEvents(
        runID: String,
        after eventID: Int64 = 0,
        onEvent: @escaping @MainActor @Sendable (TraceEvent) -> Void
    ) async throws {
        var lastEventID = eventID
        var retryDelayMilliseconds = 250

        while true {
            try Task.checkCancellation()
            do {
                let result = try await streamRunEventsOnce(runID: runID, after: lastEventID, onEvent: onEvent)
                lastEventID = result.lastEventID
                if result.terminal {
                    return
                }
                if result.receivedEvent {
                    retryDelayMilliseconds = 250
                }
            } catch is CancellationError {
                throw CancellationError()
            } catch let error as DecodingError {
                throw error
            } catch let error as APIClientError {
                if case let .http(status, _) = error, (400..<500).contains(status) {
                    throw error
                }
            } catch {
                if Task.isCancelled {
                    throw CancellationError()
                }
            }

            try await Task.sleep(for: .milliseconds(retryDelayMilliseconds))
            retryDelayMilliseconds = min(retryDelayMilliseconds * 2, 5_000)
        }
    }

    private func streamRunEventsOnce(
        runID: String,
        after eventID: Int64,
        onEvent: @escaping @MainActor @Sendable (TraceEvent) -> Void
    ) async throws -> (lastEventID: Int64, terminal: Bool, receivedEvent: Bool) {
        var components = URLComponents(url: try url(path: "/v1/runs/\(runID)/events"), resolvingAgainstBaseURL: false)
        if eventID > 0 {
            components?.queryItems = [URLQueryItem(name: "after", value: String(eventID))]
        }
        guard let streamURL = components?.url else {
            throw APIClientError.invalidServerURL
        }
        var request = authorizedRequest(url: streamURL)
        request.setValue("text/event-stream", forHTTPHeaderField: "Accept")

        let (bytes, response) = try await session.bytes(for: request)
        guard let httpResponse = response as? HTTPURLResponse else {
            throw APIClientError.invalidResponse
        }
        guard (200..<300).contains(httpResponse.statusCode) else {
            throw APIClientError.http(status: httpResponse.statusCode, message: "")
        }

        var parser = ServerSentEventParser()
        let decoder = JSONDecoder()
        var lastEventID = eventID
        var terminal = false
        var receivedEvent = false
        for try await line in bytes.lines {
            guard let data = parser.consume(line: line) else {
                continue
            }
            let event = try decoder.decode(TraceEvent.self, from: data)
            lastEventID = max(lastEventID, event.id)
            terminal = terminal || event.isTerminal
            receivedEvent = true
            await onEvent(event)
            if terminal {
                return (lastEventID, true, true)
            }
        }
        if let data = parser.finish() {
            let event = try decoder.decode(TraceEvent.self, from: data)
            lastEventID = max(lastEventID, event.id)
            terminal = terminal || event.isTerminal
            receivedEvent = true
            await onEvent(event)
        }
        return (lastEventID, terminal, receivedEvent)
    }

    private func get<Response: Decodable>(_ path: String) async throws -> Response {
        let request = authorizedRequest(url: try url(path: path))
        return try await perform(request)
    }

    private func send<Body: Encodable, Response: Decodable>(
        path: String,
        method: String,
        body: Body
    ) async throws -> Response {
        var request = authorizedRequest(url: try url(path: path))
        request.httpMethod = method
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = try JSONEncoder().encode(body)
        return try await perform(request)
    }

    private func perform<Response: Decodable>(_ request: URLRequest) async throws -> Response {
        let (data, response) = try await session.data(for: request)
        guard let httpResponse = response as? HTTPURLResponse else {
            throw APIClientError.invalidResponse
        }
        guard (200..<300).contains(httpResponse.statusCode) else {
            let message = (try? JSONDecoder().decode(ErrorResponse.self, from: data).error) ?? ""
            throw APIClientError.http(status: httpResponse.statusCode, message: message)
        }
        return try JSONDecoder().decode(Response.self, from: data)
    }

    private func authorizedRequest(url: URL) -> URLRequest {
        var request = URLRequest(url: url)
        request.setValue("Bearer \(configuration.token)", forHTTPHeaderField: "X-Exedev-Authorization")
        return request
    }

    private func url(path: String) throws -> URL {
        guard var components = URLComponents(url: configuration.baseURL, resolvingAgainstBaseURL: false) else {
            throw APIClientError.invalidServerURL
        }
        components.path = components.path.trimmingCharacters(in: CharacterSet(charactersIn: "/"))
        if !components.path.isEmpty {
            components.path = "/" + components.path
        }
        components.path += path
        guard let result = components.url else {
            throw APIClientError.invalidServerURL
        }
        return result
    }
}

struct ServerSentEventParser {
    private var dataLines: [String] = []

    mutating func consume(line: String) -> Data? {
        if line.isEmpty || line.hasPrefix(":") {
            return dispatch()
        }
        let pieces = line.split(separator: ":", maxSplits: 1, omittingEmptySubsequences: false)
        guard let field = pieces.first else {
            return nil
        }
        if field == "id" || field == "event" {
            return dispatch()
        }
        guard field == "data", pieces.count == 2 else {
            return nil
        }
        dataLines.append(String(pieces[1]).trimmingPrefix(" "))
        return nil
    }

    mutating func finish() -> Data? {
        dispatch()
    }

    private mutating func dispatch() -> Data? {
        guard !dataLines.isEmpty else {
            return nil
        }
        defer { dataLines.removeAll(keepingCapacity: true) }
        return dataLines.joined(separator: "\n").data(using: .utf8)
    }
}

func haveSameOrigin(_ first: URL, _ second: URL) -> Bool {
    guard first.user == nil,
          first.password == nil,
          second.user == nil,
          second.password == nil,
          first.scheme?.lowercased() == second.scheme?.lowercased(),
          first.host?.lowercased() == second.host?.lowercased()
    else {
        return false
    }
    return effectivePort(first) == effectivePort(second)
}

private func effectivePort(_ url: URL) -> Int? {
    if let port = url.port {
        return port
    }
    switch url.scheme?.lowercased() {
    case "http":
        return 80
    case "https":
        return 443
    default:
        return nil
    }
}

private final class SameOriginRedirectDelegate: NSObject, URLSessionTaskDelegate, @unchecked Sendable {
    func urlSession(
        _ session: URLSession,
        task: URLSessionTask,
        willPerformHTTPRedirection response: HTTPURLResponse,
        newRequest request: URLRequest,
        completionHandler: @escaping (URLRequest?) -> Void
    ) {
        guard let sourceURL = response.url,
              let destinationURL = request.url,
              haveSameOrigin(sourceURL, destinationURL)
        else {
            completionHandler(nil)
            return
        }
        completionHandler(request)
    }
}

private struct EmptyRequest: Encodable {}

private struct MessageRequest: Encodable {
    let content: String
}

private extension String {
    func trimmingPrefix(_ prefix: Character) -> String {
        guard first == prefix else {
            return self
        }
        return String(dropFirst())
    }
}
