import Foundation
import Testing
@testable import ConseilCore

@Test
func apiConfigurationLimitsTokenToExeHosts() throws {
    _ = try APIConfiguration(baseURL: URL(string: "https://yolk-adze.exe.xyz")!, token: "token")
    _ = try APIConfiguration(baseURL: URL(string: "http://localhost:8000")!, token: "token")

    #expect(throws: APIClientError.self) {
        try APIConfiguration(baseURL: URL(string: "https://example.com")!, token: "token")
    }
    #expect(throws: APIClientError.self) {
        try APIConfiguration(baseURL: URL(string: "https://user@yolk-adze.exe.xyz")!, token: "token")
    }
}

@Test
func redirectOriginsMustMatch() {
    // APIClient's redirect delegate relies on this check before forwarding a
    // request that contains the VM bearer token.
    let base = URL(string: "https://yolk-adze.exe.xyz/v1/conversations")!

    #expect(haveSameOrigin(base, URL(string: "https://yolk-adze.exe.xyz:443/other")!))
    #expect(!haveSameOrigin(base, URL(string: "https://other.exe.xyz/other")!))
    #expect(!haveSameOrigin(base, URL(string: "http://yolk-adze.exe.xyz/other")!))
    #expect(!haveSameOrigin(base, URL(string: "https://yolk-adze.exe.xyz:444/other")!))
}

@Test
func traceEventKeepsNestedPayload() throws {
    let data = Data(#"{"id":7,"conversation_id":"conv_1","run_id":"run_1","type":"model.event","payload":{"event":"response.completed","nested":{"count":2,"ok":true}},"created_at":"2026-08-29T00:00:00Z"}"#.utf8)
    let event = try JSONDecoder().decode(TraceEvent.self, from: data)

    #expect(event.id == 7)
    #expect(event.payload.string(for: "event") == "response.completed")
    #expect(event.payload.formatted.contains("nested"))
}

@Test
func chatEntriesReplaceDeltasWithCompletedMessage() throws {
    let json = #"""
    [
      {"id":1,"conversation_id":"conv_1","run_id":"run_1","type":"user.message","payload":{"content":"Hello"},"created_at":"now"},
      {"id":2,"conversation_id":"conv_1","run_id":"run_1","type":"assistant.delta","payload":{"delta":"par"},"created_at":"now"},
      {"id":3,"conversation_id":"conv_1","run_id":"run_1","type":"assistant.delta","payload":{"delta":"tial"},"created_at":"now"},
      {"id":4,"conversation_id":"conv_1","run_id":"run_1","type":"assistant.message","payload":{"content":"final"},"created_at":"now"}
    ]
    """#
    let events = try JSONDecoder().decode([TraceEvent].self, from: Data(json.utf8))

    let entries = chatEntries(from: events)
    #expect(entries.map(\.content) == ["Hello", "final"])
}

@Test
func serverSentEventParserHandlesCommentsAndMultipleDataLines() {
    var parser = ServerSentEventParser()

    #expect(parser.consume(line: ": keepalive") == nil)
    #expect(parser.consume(line: "event: trace") == nil)
    #expect(parser.consume(line: "data: {\"first\":") == nil)
    #expect(parser.consume(line: "data: true}") == nil)
    #expect(parser.consume(line: "") == Data("{\"first\":\ntrue}".utf8))
    #expect(parser.finish() == nil)
}

@Test
func serverSentEventParserHandlesDroppedBlankLines() {
    var parser = ServerSentEventParser()

    #expect(parser.consume(line: "id: 1") == nil)
    #expect(parser.consume(line: "event: trace") == nil)
    #expect(parser.consume(line: "data: {\"id\":1}") == nil)
    #expect(parser.consume(line: "id: 2") == Data("{\"id\":1}".utf8))
    #expect(parser.consume(line: "event: trace") == nil)
    #expect(parser.consume(line: "data: {\"id\":2}") == nil)
    #expect(parser.finish() == Data("{\"id\":2}".utf8))
}
