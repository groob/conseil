package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestStoreMigratesOriginalRunsSchemaWithoutLosingTrace(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conseil.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	const createdAt = "2026-08-29T12:00:00Z"
	_, err = db.ExecContext(ctx, `
PRAGMA foreign_keys = ON;
CREATE TABLE conversations (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE runs (
    id TEXT PRIMARY KEY,
    conversation_id TEXT NOT NULL REFERENCES conversations(id),
    model TEXT NOT NULL,
    instructions TEXT NOT NULL,
    status TEXT NOT NULL,
    error TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    started_at TEXT,
    finished_at TEXT
);
CREATE TABLE events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    conversation_id TEXT NOT NULL REFERENCES conversations(id),
    run_id TEXT REFERENCES runs(id),
    type TEXT NOT NULL,
    payload TEXT NOT NULL,
    created_at TEXT NOT NULL
);
INSERT INTO conversations VALUES ('conv_old', 'Old trace', ?, ?);
INSERT INTO runs VALUES ('run_old', 'conv_old', 'old-model', 'old instructions', 'completed', '', ?, ?, ?);
INSERT INTO events (conversation_id, run_id, type, payload, created_at)
VALUES ('conv_old', 'run_old', 'assistant.message', '{"content":"preserved"}', ?);`, createdAt, createdAt, createdAt, createdAt, createdAt, createdAt)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	cleanupStore(t, store)

	run, err := store.run(ctx, "run_old")
	if err != nil {
		t.Fatal(err)
	}
	if run.Ordinal != 1 || run.Agent != "conseil" || run.Model != "old-model" {
		t.Fatalf("migrated run = %#v", run)
	}
	events, err := store.eventsForConversation(ctx, "conv_old")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != "assistant.message" || string(events[0].Payload) != `{"content":"preserved"}` {
		t.Fatalf("migrated events = %#v", events)
	}
	var version int
	if err := store.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 1 {
		t.Fatalf("schema version = %d, want 1", version)
	}
}

func TestStorePersistsCompleteTrace(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conseil.db")
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}

	conversation, _, err := store.createConversation(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	run, queuedEvents, err := store.enqueueRun(ctx, conversation.ID, "What is next?", "conseil", "", "test-model", "test instructions")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := eventTypes(queuedEvents), []string{"user.message", "run.queued"}; !equalStrings(got, want) {
		t.Fatalf("queued event types = %v, want %v", got, want)
	}

	claimed, startedEvent, ok, err := store.claimNextRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || claimed.ID != run.ID {
		t.Fatalf("claimed run = %#v, %v; want %s", claimed, ok, run.ID)
	}
	if startedEvent.Type != "run.started" {
		t.Fatalf("started event type = %q", startedEvent.Type)
	}
	if _, err := store.appendRunEvent(ctx, claimed, "model.event", map[string]string{"data": "raw provider event"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.completeRun(ctx, claimed, "Do the smallest useful thing."); err != nil {
		t.Fatal(err)
	}
	duplicateEvents, err := store.completeRun(ctx, claimed, "Do the smallest useful thing.")
	if err != nil {
		t.Fatal(err)
	}
	if len(duplicateEvents) != 0 {
		t.Fatalf("idempotent completion added events: %#v", duplicateEvents)
	}
	if err := store.close(); err != nil {
		t.Fatal(err)
	}

	store, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	cleanupStore(t, store)

	events, err := store.eventsForConversation(ctx, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantTypes := []string{
		"conversation.created",
		"user.message",
		"run.queued",
		"run.started",
		"model.event",
		"assistant.message",
		"run.completed",
	}
	if got := eventTypes(events); !equalStrings(got, wantTypes) {
		t.Fatalf("persisted event types = %v, want %v", got, wantTypes)
	}
	storedRun, err := store.run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := store.messagesForRun(ctx, storedRun)
	if err != nil {
		t.Fatal(err)
	}
	wantMessages := []chatMessage{
		{Role: "user", Content: "What is next?"},
		{Role: "assistant", Content: "Do the smallest useful thing."},
	}
	if len(messages) != len(wantMessages) {
		t.Fatalf("messages = %#v, want %#v", messages, wantMessages)
	}
	for i := range messages {
		if messages[i] != wantMessages[i] {
			t.Fatalf("message %d = %#v, want %#v", i, messages[i], wantMessages[i])
		}
	}
}

func TestStoreMarksInterruptedRunFailed(t *testing.T) {
	ctx := context.Background()
	store, err := openStore(filepath.Join(t.TempDir(), "conseil.db"))
	if err != nil {
		t.Fatal(err)
	}
	cleanupStore(t, store)

	conversation, _, err := store.createConversation(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	queued, _, err := store.enqueueRun(ctx, conversation.ID, "Keep this trace", "conseil", "", "test-model", "instructions")
	if err != nil {
		t.Fatal(err)
	}
	claimed, _, ok, err := store.claimNextRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || claimed.ID != queued.ID {
		t.Fatal("run was not claimed")
	}

	recovered, err := store.recoverInterrupted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 || recovered[0].Type != "run.interrupted" {
		t.Fatalf("recovery events = %#v", recovered)
	}
	storedRun, err := store.run(ctx, queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedRun.Status != "failed" || storedRun.Error == "" {
		t.Fatalf("recovered run = %#v", storedRun)
	}
}

func TestMessagesForRunKeepsQueuedTurnsInCausalOrder(t *testing.T) {
	ctx := context.Background()
	store, err := openStore(filepath.Join(t.TempDir(), "conseil.db"))
	if err != nil {
		t.Fatal(err)
	}
	cleanupStore(t, store)

	conversation, _, err := store.createConversation(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := store.enqueueRun(ctx, conversation.ID, "first", "conseil", "", "model", "instructions")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := store.enqueueRun(ctx, conversation.ID, "second", "conseil", "", "model", "instructions")
	if err != nil {
		t.Fatal(err)
	}
	active, err := store.activeRunsForConversation(ctx, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 2 || active[0].ID != first.ID || active[1].ID != second.ID {
		t.Fatalf("active runs = %#v", active)
	}

	claimedFirst, _, ok, err := store.claimNextRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || claimedFirst.ID != first.ID {
		t.Fatalf("first claimed run = %q, want %q", claimedFirst.ID, first.ID)
	}
	firstMessages, err := store.messagesForRun(ctx, claimedFirst)
	if err != nil {
		t.Fatal(err)
	}
	if want := []chatMessage{{Role: "user", Content: "first"}}; !equalMessages(firstMessages, want) {
		t.Fatalf("first run messages = %#v, want %#v", firstMessages, want)
	}
	if _, err := store.completeRun(ctx, claimedFirst, "first answer"); err != nil {
		t.Fatal(err)
	}

	claimedSecond, _, ok, err := store.claimNextRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || claimedSecond.ID != second.ID {
		t.Fatalf("second claimed run = %q, want %q", claimedSecond.ID, second.ID)
	}
	secondMessages, err := store.messagesForRun(ctx, claimedSecond)
	if err != nil {
		t.Fatal(err)
	}
	wantSecond := []chatMessage{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "first answer"},
		{Role: "user", Content: "second"},
	}
	if !equalMessages(secondMessages, wantSecond) {
		t.Fatalf("second run messages = %#v, want %#v", secondMessages, wantSecond)
	}
}

func TestStoreRejectsRunForMissingConversation(t *testing.T) {
	store, err := openStore(filepath.Join(t.TempDir(), "conseil.db"))
	if err != nil {
		t.Fatal(err)
	}
	cleanupStore(t, store)

	_, _, err = store.enqueueRun(context.Background(), "missing", "hello", "conseil", "", "model", "instructions")
	if err == nil {
		t.Fatal("enqueueRun succeeded for a missing conversation")
	}
}

func eventTypes(events []event) []string {
	types := make([]string, len(events))
	for i, event := range events {
		types[i] = event.Type
		if !json.Valid(event.Payload) {
			panic("test received invalid event payload")
		}
	}
	return types
}

func equalMessages(left, right []chatMessage) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func cleanupStore(t *testing.T, store *store) {
	t.Helper()
	t.Cleanup(func() {
		if err := store.close(); err != nil {
			t.Errorf("closing store: %v", err)
		}
	})
}
