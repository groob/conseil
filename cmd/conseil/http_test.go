package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

type recordingRunner struct {
	messages chan []chatMessage
}

func (r *recordingRunner) stream(_ context.Context, messages []chatMessage, emit emitEvent) (string, error) {
	copied := append([]chatMessage(nil), messages...)
	r.messages <- copied
	if err := emit("model.request", map[string]string{"body": "exact request"}); err != nil {
		return "", err
	}
	if err := emit("assistant.delta", map[string]string{"delta": "Hello"}); err != nil {
		return "", err
	}
	return "Hello", nil
}

func TestPhoneToPersistedTraceFlow(t *testing.T) {
	store, err := openStore(filepath.Join(t.TempDir(), "conseil.db"))
	if err != nil {
		t.Fatal(err)
	}
	cleanupStore(t, store)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := newEventHub()
	runner := &recordingRunner{messages: make(chan []chatMessage, 1)}
	worker := newWorker(store, runner, hub, logger, time.Minute)
	api := &api{
		store:                store,
		worker:               worker,
		hub:                  hub,
		logger:               logger,
		agentName:            "conseil",
		model:                "test-model",
		instructions:         "test instructions",
		allowUnauthenticated: true,
	}
	server := httptest.NewServer(api.handler())
	defer server.Close()

	workerCtx, stopWorker := context.WithCancel(context.Background())
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.run(workerCtx) }()
	t.Cleanup(func() {
		stopWorker()
		if err := <-workerDone; err != nil {
			t.Errorf("worker stopped with error: %v", err)
		}
	})

	createResponse := doJSON(t, server.Client(), http.MethodPost, server.URL+"/v1/conversations", `{}`)
	if createResponse.StatusCode != http.StatusCreated {
		t.Fatalf("create conversation status = %d", createResponse.StatusCode)
	}
	var created struct {
		Conversation conversation `json:"conversation"`
	}
	decodeResponse(t, createResponse, &created)

	messageResponse := doJSON(t, server.Client(), http.MethodPost, server.URL+"/v1/conversations/"+created.Conversation.ID+"/messages", `{"content":"Say hello"}`)
	if messageResponse.StatusCode != http.StatusAccepted {
		t.Fatalf("create message status = %d", messageResponse.StatusCode)
	}
	var queued struct {
		Run run `json:"run"`
	}
	decodeResponse(t, messageResponse, &queued)

	streamCtx, cancelStream := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelStream()
	request, err := http.NewRequestWithContext(streamCtx, http.MethodGet, server.URL+"/v1/runs/"+queued.Run.ID+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer closeResponse(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d", response.StatusCode)
	}
	var streamed []event
	if err := scanServerSentEvents(response.Body, func(_ string, data []byte) error {
		var event event
		if err := json.Unmarshal(data, &event); err != nil {
			return err
		}
		streamed = append(streamed, event)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	wantStreamed := []string{
		"user.message",
		"run.queued",
		"run.started",
		"model.request",
		"assistant.delta",
		"assistant.message",
		"run.completed",
	}
	if got := eventTypes(streamed); !equalStrings(got, wantStreamed) {
		t.Fatalf("streamed events = %v, want %v", got, wantStreamed)
	}

	messages := <-runner.messages
	if len(messages) != 1 || messages[0] != (chatMessage{Role: "user", Content: "Say hello"}) {
		t.Fatalf("runner messages = %#v", messages)
	}

	traceResponse, err := server.Client().Get(server.URL + "/v1/conversations/" + created.Conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if traceResponse.StatusCode != http.StatusOK {
		t.Fatalf("trace status = %d", traceResponse.StatusCode)
	}
	var trace struct {
		Conversation conversation `json:"conversation"`
		Events       []event      `json:"events"`
		ActiveRuns   []run        `json:"active_runs"`
	}
	decodeResponse(t, traceResponse, &trace)
	wantTrace := append([]string{"conversation.created"}, wantStreamed...)
	if got := eventTypes(trace.Events); !equalStrings(got, wantTrace) {
		t.Fatalf("trace events = %v, want %v", got, wantTrace)
	}
	if len(trace.ActiveRuns) != 0 {
		t.Fatalf("active runs after completion = %#v", trace.ActiveRuns)
	}
}

func TestAPIDeniesUnknownExeUser(t *testing.T) {
	store, err := openStore(filepath.Join(t.TempDir(), "conseil.db"))
	if err != nil {
		t.Fatal(err)
	}
	cleanupStore(t, store)

	api := &api{
		store:      store,
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		ownerEmail: "owner@example.com",
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/conversations", nil)
	request.Header.Set("X-ExeDev-Email", "other@example.com")
	response := httptest.NewRecorder()
	api.handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusForbidden)
	}
}

func doJSON(t *testing.T, client *http.Client, method, url, body string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func decodeResponse(t *testing.T, response *http.Response, destination any) {
	t.Helper()
	defer closeResponse(t, response)
	if err := json.NewDecoder(response.Body).Decode(destination); err != nil {
		t.Fatal(err)
	}
}

func closeResponse(t *testing.T, response *http.Response) {
	t.Helper()
	if err := response.Body.Close(); err != nil {
		t.Errorf("closing response body: %v", err)
	}
}
