package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const newConversationTitle = "New conversation"

type conversation struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type run struct {
	Ordinal        int64      `json:"-"`
	ID             string     `json:"id"`
	ConversationID string     `json:"conversation_id"`
	ParentRunID    string     `json:"parent_run_id,omitempty"`
	Agent          string     `json:"agent"`
	Model          string     `json:"model"`
	Instructions   string     `json:"instructions"`
	Status         string     `json:"status"`
	Error          string     `json:"error,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
}

type event struct {
	ID             int64           `json:"id"`
	ConversationID string          `json:"conversation_id"`
	RunID          string          `json:"run_id,omitempty"`
	Type           string          `json:"type"`
	Payload        json.RawMessage `json:"payload"`
	CreatedAt      time.Time       `json:"created_at"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type store struct {
	db *sql.DB
}

type sqlQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func openStore(path string) (*store, error) {
	if path == "" {
		return nil, errors.New("database path is empty")
	}
	if path != ":memory:" && !strings.HasPrefix(path, "file:") {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("creating database directory: %w", err)
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}
	db.SetMaxOpenConns(1)

	s := &store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *store) close() error {
	return s.db.Close()
}

func (s *store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
PRAGMA journal_mode = WAL;
PRAGMA busy_timeout = 5000;`); err != nil {
		return fmt.Errorf("configuring database: %w", err)
	}

	var hasRuns bool
	if err := s.db.QueryRowContext(ctx, `
SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'runs')`).Scan(&hasRuns); err != nil {
		return fmt.Errorf("checking runs table: %w", err)
	}
	if hasRuns {
		columns, err := s.runColumns(ctx)
		if err != nil {
			return err
		}
		if !columns["ordinal"] || !columns["agent"] || !columns["parent_run_id"] {
			if err := s.rebuildRuns(ctx, columns); err != nil {
				return err
			}
		}
	}

	const schema = `
CREATE TABLE IF NOT EXISTS conversations (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS runs (
    ordinal INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    conversation_id TEXT NOT NULL REFERENCES conversations(id),
    parent_run_id TEXT REFERENCES runs(id),
    agent TEXT NOT NULL,
    model TEXT NOT NULL,
    instructions TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'completed', 'failed')),
    error TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    started_at TEXT,
    finished_at TEXT
);

CREATE INDEX IF NOT EXISTS runs_status_ordinal_idx ON runs(status, ordinal);
CREATE INDEX IF NOT EXISTS runs_conversation_ordinal_idx ON runs(conversation_id, ordinal);

CREATE TABLE IF NOT EXISTS events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    conversation_id TEXT NOT NULL REFERENCES conversations(id),
    run_id TEXT REFERENCES runs(id),
    type TEXT NOT NULL,
    payload TEXT NOT NULL CHECK (json_valid(payload)),
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS events_conversation_idx ON events(conversation_id, id);
CREATE INDEX IF NOT EXISTS events_run_idx ON events(run_id, id);

PRAGMA user_version = 1;
PRAGMA foreign_keys = ON;`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("migrating database: %w", err)
	}
	if err := s.checkForeignKeys(ctx); err != nil {
		return err
	}
	return nil
}

func (s *store) runColumns(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(runs)`)
	if err != nil {
		return nil, fmt.Errorf("reading runs table columns: %w", err)
	}
	defer closeRows(rows)

	columns := make(map[string]bool)
	for rows.Next() {
		var position, notNull, primaryKey int
		var name, columnType string
		var defaultValue sql.NullString
		if err := rows.Scan(&position, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return nil, fmt.Errorf("scanning runs table column: %w", err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating runs table columns: %w", err)
	}
	return columns, nil
}

func (s *store) rebuildRuns(ctx context.Context, columns map[string]bool) (migrateErr error) {
	if _, err := s.db.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("disabling foreign keys for runs migration: %w", err)
	}
	defer func() {
		if _, err := s.db.ExecContext(context.WithoutCancel(ctx), `PRAGMA foreign_keys = ON`); migrateErr == nil && err != nil {
			migrateErr = fmt.Errorf("reenabling foreign keys after runs migration: %w", err)
		}
	}()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning runs migration: %w", err)
	}
	defer rollback(tx)
	if _, err := tx.ExecContext(ctx, `
DROP TABLE IF EXISTS runs_migration;
CREATE TABLE runs_migration (
    ordinal INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    conversation_id TEXT NOT NULL REFERENCES conversations(id),
    parent_run_id TEXT REFERENCES runs_migration(id),
    agent TEXT NOT NULL,
    model TEXT NOT NULL,
    instructions TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'completed', 'failed')),
    error TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    started_at TEXT,
    finished_at TEXT
);`); err != nil {
		return fmt.Errorf("creating migrated runs table: %w", err)
	}

	parentExpression := "NULL"
	if columns["parent_run_id"] {
		parentExpression = "parent_run_id"
	}
	agentExpression := "'conseil'"
	if columns["agent"] {
		agentExpression = "COALESCE(NULLIF(agent, ''), 'conseil')"
	}
	orderExpression := "created_at, id"
	if columns["ordinal"] {
		orderExpression = "ordinal"
	}
	copyRuns := fmt.Sprintf(`
INSERT INTO runs_migration (
    id, conversation_id, parent_run_id, agent, model, instructions,
    status, error, created_at, started_at, finished_at
)
SELECT
    id, conversation_id, %s, %s, model, instructions,
    status, error, created_at, started_at, finished_at
FROM runs
ORDER BY %s`, parentExpression, agentExpression, orderExpression)
	if _, err := tx.ExecContext(ctx, copyRuns); err != nil {
		return fmt.Errorf("copying runs during migration: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
DROP TABLE runs;
ALTER TABLE runs_migration RENAME TO runs;`); err != nil {
		return fmt.Errorf("replacing runs table: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing runs migration: %w", err)
	}
	return nil
}

func (s *store) checkForeignKeys(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("checking foreign keys: %w", err)
	}
	defer closeRows(rows)
	if rows.Next() {
		var table, parent string
		var rowID sql.NullInt64
		var foreignKeyID int
		if err := rows.Scan(&table, &rowID, &parent, &foreignKeyID); err != nil {
			return fmt.Errorf("scanning foreign key violation: %w", err)
		}
		return fmt.Errorf("foreign key violation in table %s row %v referencing %s", table, rowID, parent)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating foreign key check: %w", err)
	}
	return nil
}

func (s *store) createConversation(ctx context.Context, title string) (conversation, event, error) {
	id, err := newID("conv")
	if err != nil {
		return conversation{}, event{}, err
	}
	title = strings.TrimSpace(title)
	if title == "" {
		title = newConversationTitle
	}
	now := time.Now().UTC()
	c := conversation{ID: id, Title: title, CreatedAt: now, UpdatedAt: now}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return conversation{}, event{}, fmt.Errorf("beginning conversation transaction: %w", err)
	}
	defer rollback(tx)

	if _, err := tx.ExecContext(ctx, `
INSERT INTO conversations (id, title, created_at, updated_at)
VALUES (?, ?, ?, ?)`, c.ID, c.Title, formatTime(c.CreatedAt), formatTime(c.UpdatedAt)); err != nil {
		return conversation{}, event{}, fmt.Errorf("inserting conversation: %w", err)
	}
	e, err := appendEventTx(ctx, tx, c.ID, "", "conversation.created", map[string]string{"title": c.Title})
	if err != nil {
		return conversation{}, event{}, err
	}
	if err := tx.Commit(); err != nil {
		return conversation{}, event{}, fmt.Errorf("committing conversation: %w", err)
	}
	return c, e, nil
}

func (s *store) listConversations(ctx context.Context) ([]conversation, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, title, created_at, updated_at
FROM conversations
ORDER BY updated_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("listing conversations: %w", err)
	}
	defer closeRows(rows)

	var conversations []conversation
	for rows.Next() {
		var c conversation
		var createdAt, updatedAt string
		if err := rows.Scan(&c.ID, &c.Title, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scanning conversation: %w", err)
		}
		if c.CreatedAt, err = parseTime(createdAt); err != nil {
			return nil, err
		}
		if c.UpdatedAt, err = parseTime(updatedAt); err != nil {
			return nil, err
		}
		conversations = append(conversations, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating conversations: %w", err)
	}
	return conversations, nil
}

func (s *store) conversation(ctx context.Context, id string) (conversation, error) {
	return queryConversation(ctx, s.db, id)
}

func queryConversation(ctx context.Context, database sqlQueryer, id string) (conversation, error) {
	var c conversation
	var createdAt, updatedAt string
	err := database.QueryRowContext(ctx, `
SELECT id, title, created_at, updated_at
FROM conversations
WHERE id = ?`, id).Scan(&c.ID, &c.Title, &createdAt, &updatedAt)
	if err != nil {
		return conversation{}, fmt.Errorf("getting conversation: %w", err)
	}
	c.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return conversation{}, err
	}
	c.UpdatedAt, err = parseTime(updatedAt)
	if err != nil {
		return conversation{}, err
	}
	return c, nil
}

func (s *store) eventsForConversation(ctx context.Context, conversationID string) ([]event, error) {
	return queryEvents(ctx, s.db, `
SELECT id, conversation_id, COALESCE(run_id, ''), type, payload, created_at
FROM events
WHERE conversation_id = ?
ORDER BY id`, conversationID)
}

func (s *store) conversationSnapshot(ctx context.Context, conversationID string) (conversation, []event, []run, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return conversation{}, nil, nil, fmt.Errorf("beginning conversation snapshot: %w", err)
	}
	defer rollback(tx)

	c, err := queryConversation(ctx, tx, conversationID)
	if err != nil {
		return conversation{}, nil, nil, err
	}
	events, err := queryEvents(ctx, tx, `
SELECT id, conversation_id, COALESCE(run_id, ''), type, payload, created_at
FROM events
WHERE conversation_id = ?
ORDER BY id`, conversationID)
	if err != nil {
		return conversation{}, nil, nil, err
	}
	activeRuns, err := queryActiveRuns(ctx, tx, conversationID)
	if err != nil {
		return conversation{}, nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return conversation{}, nil, nil, fmt.Errorf("committing conversation snapshot: %w", err)
	}
	return c, events, activeRuns, nil
}

func (s *store) eventsForRunAfter(ctx context.Context, runID string, after int64) ([]event, error) {
	return queryEvents(ctx, s.db, `
SELECT id, conversation_id, COALESCE(run_id, ''), type, payload, created_at
FROM events
WHERE run_id = ? AND id > ?
ORDER BY id`, runID, after)
}

func queryEvents(ctx context.Context, database sqlQueryer, query string, args ...any) ([]event, error) {
	rows, err := database.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying events: %w", err)
	}
	defer closeRows(rows)

	var events []event
	for rows.Next() {
		var e event
		var payload, createdAt string
		if err := rows.Scan(&e.ID, &e.ConversationID, &e.RunID, &e.Type, &payload, &createdAt); err != nil {
			return nil, fmt.Errorf("scanning event: %w", err)
		}
		e.Payload = json.RawMessage(payload)
		e.CreatedAt, err = parseTime(createdAt)
		if err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating events: %w", err)
	}
	return events, nil
}

func (s *store) enqueueRun(ctx context.Context, conversationID, content, agent, parentRunID, model, instructions string) (run, []event, error) {
	runID, err := newID("run")
	if err != nil {
		return run{}, nil, err
	}
	now := time.Now().UTC()
	r := run{
		ID:             runID,
		ConversationID: conversationID,
		ParentRunID:    parentRunID,
		Agent:          agent,
		Model:          model,
		Instructions:   instructions,
		Status:         "queued",
		CreatedAt:      now,
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return run{}, nil, fmt.Errorf("beginning run transaction: %w", err)
	}
	defer rollback(tx)

	var nullableParentRunID any
	if r.ParentRunID != "" {
		nullableParentRunID = r.ParentRunID
	}
	result, err := tx.ExecContext(ctx, `
INSERT INTO runs (id, conversation_id, parent_run_id, agent, model, instructions, status, created_at)
VALUES (?, ?, ?, ?, ?, ?, 'queued', ?)`, r.ID, r.ConversationID, nullableParentRunID, r.Agent, r.Model, r.Instructions, formatTime(r.CreatedAt))
	if err != nil {
		return run{}, nil, fmt.Errorf("inserting run: %w", err)
	}
	r.Ordinal, err = result.LastInsertId()
	if err != nil {
		return run{}, nil, fmt.Errorf("reading run ordinal: %w", err)
	}

	userEvent, err := appendEventTx(ctx, tx, conversationID, runID, "user.message", map[string]string{"content": content})
	if err != nil {
		return run{}, nil, err
	}
	queuedEvent, err := appendEventTx(ctx, tx, conversationID, runID, "run.queued", map[string]string{
		"agent":         agent,
		"parent_run_id": parentRunID,
		"model":         model,
		"instructions":  instructions,
	})
	if err != nil {
		return run{}, nil, err
	}

	if _, err := tx.ExecContext(ctx, `
UPDATE conversations
SET title = CASE WHEN title = ? THEN ? ELSE title END,
    updated_at = ?
WHERE id = ?`, newConversationTitle, titleFromMessage(content), formatTime(now), conversationID); err != nil {
		return run{}, nil, fmt.Errorf("updating conversation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return run{}, nil, fmt.Errorf("committing run: %w", err)
	}
	return r, []event{userEvent, queuedEvent}, nil
}

func (s *store) claimNextRun(ctx context.Context) (run, event, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return run{}, event{}, false, fmt.Errorf("beginning claim transaction: %w", err)
	}
	defer rollback(tx)

	var r run
	var createdAt string
	err = tx.QueryRowContext(ctx, `
SELECT ordinal, id, conversation_id, COALESCE(parent_run_id, ''), agent, model, instructions, status, error, created_at
FROM runs
WHERE status = 'queued'
ORDER BY ordinal
LIMIT 1`).Scan(&r.Ordinal, &r.ID, &r.ConversationID, &r.ParentRunID, &r.Agent, &r.Model, &r.Instructions, &r.Status, &r.Error, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return run{}, event{}, false, nil
	}
	if err != nil {
		return run{}, event{}, false, fmt.Errorf("selecting queued run: %w", err)
	}
	if r.CreatedAt, err = parseTime(createdAt); err != nil {
		return run{}, event{}, false, err
	}

	startedAt := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `
UPDATE runs
SET status = 'running', started_at = ?
WHERE id = ? AND status = 'queued'`, formatTime(startedAt), r.ID)
	if err != nil {
		return run{}, event{}, false, fmt.Errorf("claiming run: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return run{}, event{}, false, fmt.Errorf("checking claimed run: %w", err)
	}
	if changed != 1 {
		return run{}, event{}, false, nil
	}
	r.Status = "running"
	r.StartedAt = &startedAt
	startedEvent, err := appendEventTx(ctx, tx, r.ConversationID, r.ID, "run.started", map[string]string{"model": r.Model})
	if err != nil {
		return run{}, event{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return run{}, event{}, false, fmt.Errorf("committing claimed run: %w", err)
	}
	return r, startedEvent, true, nil
}

func (s *store) recoverInterrupted(ctx context.Context) ([]event, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("beginning recovery transaction: %w", err)
	}
	defer rollback(tx)

	rows, err := tx.QueryContext(ctx, `
SELECT id, conversation_id
FROM runs
WHERE status = 'running'
ORDER BY ordinal`)
	if err != nil {
		return nil, fmt.Errorf("listing interrupted runs: %w", err)
	}
	var interrupted []run
	for rows.Next() {
		var r run
		if err := rows.Scan(&r.ID, &r.ConversationID); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scanning interrupted run: %w", err)
		}
		interrupted = append(interrupted, r)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("closing interrupted runs: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating interrupted runs: %w", err)
	}

	const message = "service stopped before the run completed"
	var events []event
	for _, r := range interrupted {
		now := time.Now().UTC()
		if _, err := tx.ExecContext(ctx, `
UPDATE runs
SET status = 'failed', error = ?, finished_at = ?
WHERE id = ? AND status = 'running'`, message, formatTime(now), r.ID); err != nil {
			return nil, fmt.Errorf("failing interrupted run: %w", err)
		}
		e, err := appendEventTx(ctx, tx, r.ConversationID, r.ID, "run.interrupted", map[string]string{"error": message})
		if err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing interrupted runs: %w", err)
	}
	return events, nil
}

func (s *store) appendRunEvent(ctx context.Context, r run, eventType string, payload any) (event, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return event{}, fmt.Errorf("encoding %s event: %w", eventType, err)
	}
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `
INSERT INTO events (conversation_id, run_id, type, payload, created_at)
VALUES (?, ?, ?, ?, ?)`, r.ConversationID, r.ID, eventType, string(encoded), formatTime(now))
	if err != nil {
		return event{}, fmt.Errorf("inserting %s event: %w", eventType, err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return event{}, fmt.Errorf("reading %s event ID: %w", eventType, err)
	}
	return event{
		ID:             id,
		ConversationID: r.ConversationID,
		RunID:          r.ID,
		Type:           eventType,
		Payload:        encoded,
		CreatedAt:      now,
	}, nil
}

func (s *store) completeRun(ctx context.Context, r run, content string) ([]event, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("beginning completion transaction: %w", err)
	}
	defer rollback(tx)

	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM runs WHERE id = ?`, r.ID).Scan(&status); err != nil {
		return nil, fmt.Errorf("checking run before completion: %w", err)
	}
	if status == "completed" {
		return nil, nil
	}
	if status != "running" {
		return nil, fmt.Errorf("completing run %s: status is %s", r.ID, status)
	}

	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `
UPDATE runs
SET status = 'completed', finished_at = ?
WHERE id = ? AND status = 'running'`, formatTime(now), r.ID)
	if err != nil {
		return nil, fmt.Errorf("completing run: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("checking completed run: %w", err)
	}
	if changed != 1 {
		return nil, fmt.Errorf("completing run %s: run is not running", r.ID)
	}

	messageEvent, err := appendEventTx(ctx, tx, r.ConversationID, r.ID, "assistant.message", map[string]string{"content": content})
	if err != nil {
		return nil, err
	}
	completedEvent, err := appendEventTx(ctx, tx, r.ConversationID, r.ID, "run.completed", map[string]string{"status": "completed"})
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE conversations SET updated_at = ? WHERE id = ?`, formatTime(now), r.ConversationID); err != nil {
		return nil, fmt.Errorf("updating completed conversation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing completed run: %w", err)
	}
	return []event{messageEvent, completedEvent}, nil
}

func (s *store) failRun(ctx context.Context, r run, runErr error, output string) (event, error) {
	message := runErr.Error()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return event{}, fmt.Errorf("beginning failure transaction: %w", err)
	}
	defer rollback(tx)

	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `
UPDATE runs
SET status = 'failed', error = ?, finished_at = ?
WHERE id = ? AND status IN ('queued', 'running')`, message, formatTime(now), r.ID)
	if err != nil {
		return event{}, fmt.Errorf("failing run: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return event{}, fmt.Errorf("checking failed run: %w", err)
	}
	if changed != 1 {
		return event{}, fmt.Errorf("failing run %s: run is not active", r.ID)
	}
	payload := map[string]string{"error": message}
	if output != "" {
		payload["output"] = output
	}
	e, err := appendEventTx(ctx, tx, r.ConversationID, r.ID, "run.failed", payload)
	if err != nil {
		return event{}, err
	}
	if err := tx.Commit(); err != nil {
		return event{}, fmt.Errorf("committing failed run: %w", err)
	}
	return e, nil
}

func (s *store) run(ctx context.Context, id string) (run, error) {
	var r run
	var createdAt string
	var startedAt, finishedAt sql.NullString
	err := s.db.QueryRowContext(ctx, `
SELECT ordinal, id, conversation_id, COALESCE(parent_run_id, ''), agent, model, instructions, status, error, created_at, started_at, finished_at
FROM runs
WHERE id = ?`, id).Scan(
		&r.Ordinal,
		&r.ID,
		&r.ConversationID,
		&r.ParentRunID,
		&r.Agent,
		&r.Model,
		&r.Instructions,
		&r.Status,
		&r.Error,
		&createdAt,
		&startedAt,
		&finishedAt,
	)
	if err != nil {
		return run{}, fmt.Errorf("getting run: %w", err)
	}
	if r.CreatedAt, err = parseTime(createdAt); err != nil {
		return run{}, err
	}
	if startedAt.Valid {
		parsed, err := parseTime(startedAt.String)
		if err != nil {
			return run{}, err
		}
		r.StartedAt = &parsed
	}
	if finishedAt.Valid {
		parsed, err := parseTime(finishedAt.String)
		if err != nil {
			return run{}, err
		}
		r.FinishedAt = &parsed
	}
	return r, nil
}

func (s *store) activeRunsForConversation(ctx context.Context, conversationID string) ([]run, error) {
	return queryActiveRuns(ctx, s.db, conversationID)
}

func queryActiveRuns(ctx context.Context, database sqlQueryer, conversationID string) ([]run, error) {
	rows, err := database.QueryContext(ctx, `
SELECT ordinal, id, conversation_id, COALESCE(parent_run_id, ''), agent, model, instructions, status, error, created_at, started_at, finished_at
FROM runs
WHERE conversation_id = ? AND status IN ('queued', 'running')
ORDER BY ordinal`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("querying active runs: %w", err)
	}
	defer closeRows(rows)

	var runs []run
	for rows.Next() {
		var r run
		var createdAt string
		var startedAt, finishedAt sql.NullString
		if err := rows.Scan(
			&r.Ordinal,
			&r.ID,
			&r.ConversationID,
			&r.ParentRunID,
			&r.Agent,
			&r.Model,
			&r.Instructions,
			&r.Status,
			&r.Error,
			&createdAt,
			&startedAt,
			&finishedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning active run: %w", err)
		}
		if r.CreatedAt, err = parseTime(createdAt); err != nil {
			return nil, err
		}
		if startedAt.Valid {
			parsed, err := parseTime(startedAt.String)
			if err != nil {
				return nil, err
			}
			r.StartedAt = &parsed
		}
		if finishedAt.Valid {
			parsed, err := parseTime(finishedAt.String)
			if err != nil {
				return nil, err
			}
			r.FinishedAt = &parsed
		}
		runs = append(runs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating active runs: %w", err)
	}
	return runs, nil
}

func (s *store) messagesForRun(ctx context.Context, current run) ([]chatMessage, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT events.type, events.payload
FROM runs
JOIN events ON events.run_id = runs.id
WHERE runs.conversation_id = ?
  AND runs.ordinal <= ?
  AND events.type IN ('user.message', 'assistant.message')
ORDER BY runs.ordinal,
         CASE events.type WHEN 'user.message' THEN 0 ELSE 1 END,
         events.id`, current.ConversationID, current.Ordinal)
	if err != nil {
		return nil, fmt.Errorf("querying messages: %w", err)
	}
	defer closeRows(rows)

	var messages []chatMessage
	for rows.Next() {
		var eventType string
		var payload []byte
		if err := rows.Scan(&eventType, &payload); err != nil {
			return nil, fmt.Errorf("scanning message: %w", err)
		}
		var body struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal(payload, &body); err != nil {
			return nil, fmt.Errorf("decoding %s: %w", eventType, err)
		}
		role := "user"
		if eventType == "assistant.message" {
			role = "assistant"
		}
		messages = append(messages, chatMessage{Role: role, Content: body.Content})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating messages: %w", err)
	}
	return messages, nil
}

func appendEventTx(ctx context.Context, tx *sql.Tx, conversationID, runID, eventType string, payload any) (event, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return event{}, fmt.Errorf("encoding %s event: %w", eventType, err)
	}
	now := time.Now().UTC()
	var nullableRunID any
	if runID != "" {
		nullableRunID = runID
	}
	result, err := tx.ExecContext(ctx, `
INSERT INTO events (conversation_id, run_id, type, payload, created_at)
VALUES (?, ?, ?, ?, ?)`, conversationID, nullableRunID, eventType, string(encoded), formatTime(now))
	if err != nil {
		return event{}, fmt.Errorf("inserting %s event: %w", eventType, err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return event{}, fmt.Errorf("reading %s event ID: %w", eventType, err)
	}
	return event{
		ID:             id,
		ConversationID: conversationID,
		RunID:          runID,
		Type:           eventType,
		Payload:        encoded,
		CreatedAt:      now,
	}, nil
}

func newID(prefix string) (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generating %s ID: %w", prefix, err)
	}
	return prefix + "_" + hex.EncodeToString(value[:]), nil
}

func titleFromMessage(content string) string {
	title := strings.Join(strings.Fields(content), " ")
	runes := []rune(title)
	if len(runes) > 60 {
		title = string(runes[:60]) + "…"
	}
	if title == "" {
		return newConversationTitle
	}
	return title
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func parseTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing database time %q: %w", value, err)
	}
	return parsed, nil
}

func rollback(tx *sql.Tx) {
	_ = tx.Rollback()
}

func closeRows(rows *sql.Rows) {
	_ = rows.Close()
}
