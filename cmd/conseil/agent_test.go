package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLLMAgentStreamsTextAndProviderEvents(t *testing.T) {
	requests := make(chan responsesRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		var request responsesRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		requests <- request
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-ID", "request-123")
		const stream = "event: response.output_text.delta\n" +
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hel\"}\n\n" +
			"event: response.output_text.delta\n" +
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"lo\"}\n\n" +
			"event: response.completed\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"Hello\"}]}]}}\n\n"
		_, _ = io.WriteString(w, stream)
	}))
	defer server.Close()

	agent := &llmAgent{
		baseURL:         server.URL + "/v1",
		model:           "test-model",
		reasoningEffort: "high",
		instructions:    "test instructions",
		client:          server.Client(),
	}
	var emitted []string
	content, err := agent.stream(context.Background(), []chatMessage{{Role: "user", Content: "Say hello"}}, func(eventType string, _ any) error {
		emitted = append(emitted, eventType)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if content != "Hello" {
		t.Fatalf("content = %q, want Hello", content)
	}
	wantEvents := []string{
		"model.request",
		"model.response.started",
		"model.event",
		"assistant.delta",
		"model.event",
		"assistant.delta",
		"model.event",
	}
	if !equalStrings(emitted, wantEvents) {
		t.Fatalf("emitted events = %v, want %v", emitted, wantEvents)
	}

	request := <-requests
	if request.Model != "test-model" || request.Reasoning.Effort != "high" || request.Instructions != "test instructions" {
		t.Fatalf("request = %#v", request)
	}
	if !request.Stream || request.Store {
		t.Fatalf("request stream/store = %v/%v", request.Stream, request.Store)
	}
	if len(request.Input) != 1 || request.Input[0].Content != "Say hello" {
		t.Fatalf("request input = %#v", request.Input)
	}
}

func TestLLMAgentUsesDeltasWhenCompletedOutputIsEmpty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"model-ok\"}\n\n"+
			"data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n")
	}))
	defer server.Close()

	agent := &llmAgent{baseURL: server.URL, model: "test-model", reasoningEffort: "high", client: server.Client()}
	content, err := agent.stream(context.Background(), []chatMessage{{Role: "user", Content: "hello"}}, func(string, any) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if content != "model-ok" {
		t.Fatalf("content = %q, want model-ok", content)
	}
}

func TestLLMAgentRejectsIncompleteStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
	}))
	defer server.Close()

	agent := &llmAgent{baseURL: server.URL, model: "test-model", client: server.Client()}
	_, err := agent.stream(context.Background(), []chatMessage{{Role: "user", Content: "hello"}}, func(string, any) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "before response.completed") {
		t.Fatalf("error = %v", err)
	}
}

func TestScanServerSentEventsJoinsDataLines(t *testing.T) {
	input := "event: example\ndata: first\ndata: second\n\n: keepalive\n\ndata: last\n"
	var got []string
	err := scanServerSentEvents(strings.NewReader(input), func(name string, data []byte) error {
		got = append(got, name+":"+string(data))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"example:first\nsecond", ":last"}
	if !equalStrings(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
}
