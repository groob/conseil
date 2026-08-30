package devbox

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func testSession(t *testing.T) Session {
	t.Helper()
	id, branch, vm, err := newNames("counseil")
	if err != nil {
		t.Fatal(err)
	}
	identity := "conseil-session:" + id
	return Session{ID: id, Project: "counseil", Task: "test", BaseCommit: "0123456789012345678901234567890123456789", Branch: branch, VMName: vm, VMIdentity: identity, Workspace: "/tmp/work", Status: StatusProvisioning}
}

func TestOpenStoreUsesPrivateFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devbox.db")
	store, err := openStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	cleanupDevboxStore(t, store)
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) && candidate != path {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want regular 0600 file", candidate, info.Mode())
		}
	}
	info, err := os.Lstat(path + ".locks")
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Errorf("lock directory mode = %v, want directory 0700", info.Mode())
	}
}

func TestOpenStoreTightensExistingDatabaseMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devbox.db")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := openStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	cleanupDevboxStore(t, store)
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("database mode = %04o, want 0600", info.Mode().Perm())
	}
}

func TestOpenStoreRejectsPubliclyWritableDirectory(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(t.Context(), filepath.Join(directory, "devbox.db")); err == nil {
		t.Fatal("openStore() accepted a publicly writable database directory")
	}
}

func TestOpenStoreRejectsDatabaseSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "devbox.db")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(t.Context(), path); err == nil {
		t.Fatal("openStore() accepted a database symlink")
	}
}

func TestOpenStoreRejectsSQLiteSidecarSymlinks(t *testing.T) {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		t.Run(suffix, func(t *testing.T) {
			directory := t.TempDir()
			target := filepath.Join(directory, "target")
			if err := os.WriteFile(target, []byte("unchanged"), 0o600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(directory, "devbox.db")
			if err := os.Symlink(target, path+suffix); err != nil {
				t.Fatal(err)
			}
			if _, err := openStore(t.Context(), path); err == nil {
				t.Fatalf("openStore() accepted a %s symlink", suffix)
			}
			contents, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if string(contents) != "unchanged" {
				t.Fatalf("sidecar symlink target = %q, want unchanged", contents)
			}
		})
	}
}

func TestStorePersistsSessionsAndAppendOnlyEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devbox.db")
	ctx := t.Context()
	store, err := openStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	session := testSession(t)
	if err := store.create(ctx, session); err != nil {
		t.Fatal(err)
	}
	if err := store.appendEvent(ctx, session.ID, "test", map[string]string{"value": "one"}); err != nil {
		t.Fatal(err)
	}
	if err := store.close(); err != nil {
		t.Fatal(err)
	}
	store, err = openStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	cleanupDevboxStore(t, store)
	got, err := store.session(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Project != session.Project || got.Status != StatusProvisioning {
		t.Fatalf("session = %#v", got)
	}
	exists, err := store.hasEvent(ctx, session.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Error("persisted event is missing")
	}
}

func concurrentStoreSession(index int) Session {
	suffix := fmt.Sprintf("%032x", index+1)
	return Session{
		ID:         "devbox_" + suffix,
		Project:    "counseil",
		Task:       "concurrent startup",
		BaseCommit: "0123456789012345678901234567890123456789",
		Branch:     "devbox/counseil-" + suffix,
		VMName:     "counseil-" + suffix,
		VMIdentity: "conseil-session:devbox_" + suffix,
		Workspace:  "/tmp/work",
		Status:     StatusProvisioning,
	}
}

func TestOpenStoreConcurrentFreshDatabase(t *testing.T) {
	const count = 12
	path := filepath.Join(t.TempDir(), "concurrent.db")
	start := make(chan struct{})
	type openResult struct {
		store *store
		err   error
	}
	opened := make(chan openResult, count)
	for range count {
		go func() {
			<-start
			store, err := openStore(t.Context(), path)
			opened <- openResult{store: store, err: err}
		}()
	}
	close(start)

	stores := make([]*store, 0, count)
	for range count {
		result := <-opened
		if result.err != nil {
			t.Errorf("opening store: %v", result.err)
			continue
		}
		stores = append(stores, result.store)
	}
	if t.Failed() {
		for _, store := range stores {
			_ = store.close()
		}
		t.FailNow()
	}
	for index, store := range stores {
		var busyTimeout, foreignKeys int
		var journalMode string
		if err := store.db.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
			t.Fatalf("store %d busy timeout: %v", index, err)
		}
		if err := store.db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
			t.Fatalf("store %d foreign keys: %v", index, err)
		}
		if err := store.db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
			t.Fatalf("store %d journal mode: %v", index, err)
		}
		if busyTimeout != storeBusyTimeoutMS || foreignKeys != 1 || journalMode != "wal" {
			t.Fatalf("store %d pragmas = busy:%d foreign_keys:%d journal:%s", index, busyTimeout, foreignKeys, journalMode)
		}
	}

	writeStart := make(chan struct{})
	writes := make(chan error, count)
	for index, store := range stores {
		go func() {
			<-writeStart
			err := store.create(t.Context(), concurrentStoreSession(index))
			if closeErr := store.close(); err == nil {
				err = closeErr
			}
			writes <- err
		}()
	}
	close(writeStart)
	for range count {
		if err := <-writes; err != nil {
			t.Errorf("writing store: %v", err)
		}
	}
	if t.Failed() {
		t.FailNow()
	}

	store, err := openStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	cleanupDevboxStore(t, store)
	sessions, err := store.list(t.Context(), "counseil")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != count {
		t.Fatalf("sessions = %d, want %d", len(sessions), count)
	}
}

const (
	storeProcessHelperEnv = "CONSEIL_STORE_PROCESS_HELPER"
	storeProcessPathEnv   = "CONSEIL_STORE_PROCESS_PATH"
	storeProcessIndexEnv  = "CONSEIL_STORE_PROCESS_INDEX"
)

func TestOpenStoreProcessHelper(t *testing.T) {
	if os.Getenv(storeProcessHelperEnv) != "1" {
		return
	}
	var signal [1]byte
	if _, err := io.ReadFull(os.Stdin, signal[:]); err != nil {
		t.Fatal(err)
	}
	index, err := strconv.Atoi(os.Getenv(storeProcessIndexEnv))
	if err != nil {
		t.Fatal(err)
	}
	store, err := openStore(t.Context(), os.Getenv(storeProcessPathEnv))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.create(t.Context(), concurrentStoreSession(index)); err != nil {
		_ = store.close()
		t.Fatal(err)
	}
	if err := store.close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenStoreConcurrentProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "processes.db")
	readSignal, writeSignal, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = readSignal.Close()
		_ = writeSignal.Close()
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var outputs [2]bytes.Buffer
	commands := make([]*exec.Cmd, len(outputs))
	started := 0
	for index := range commands {
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOpenStoreProcessHelper$")
		command.Env = append(os.Environ(),
			storeProcessHelperEnv+"=1",
			storeProcessPathEnv+"="+path,
			storeProcessIndexEnv+"="+strconv.Itoa(index+100),
		)
		command.Stdin = readSignal
		command.Stdout = &outputs[index]
		command.Stderr = &outputs[index]
		if err := command.Start(); err != nil {
			_ = writeSignal.Close()
			for _, startedCommand := range commands[:started] {
				_ = startedCommand.Process.Kill()
				_ = startedCommand.Wait()
			}
			t.Fatalf("starting child %d: %v", index, err)
		}
		commands[index] = command
		started++
	}
	if err := readSignal.Close(); err != nil {
		t.Error(err)
	}
	signalErr := error(nil)
	if _, err := writeSignal.Write([]byte{1, 1}); err != nil {
		signalErr = err
	}
	if err := writeSignal.Close(); err != nil && signalErr == nil {
		signalErr = err
	}

	type childResult struct {
		index int
		err   error
	}
	results := make(chan childResult, len(commands))
	for index, command := range commands {
		go func() { results <- childResult{index: index, err: command.Wait()} }()
	}
	for range commands {
		result := <-results
		if result.err != nil {
			t.Errorf("child %d: %v\n%s", result.index, result.err, outputs[result.index].String())
		}
	}
	if signalErr != nil {
		t.Errorf("releasing children: %v", signalErr)
	}
	if t.Failed() {
		t.FailNow()
	}

	store, err := openStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	cleanupDevboxStore(t, store)
	sessions, err := store.list(t.Context(), "counseil")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != len(commands) {
		t.Fatalf("sessions = %d, want %d", len(sessions), len(commands))
	}
}

func TestRetrySQLiteLockDoesNotRetryArbitraryErrors(t *testing.T) {
	want := errors.New("arbitrary failure")
	attempts := 0
	err := retrySQLiteLock(t.Context(), func() error {
		attempts++
		return want
	})
	if !errors.Is(err, want) {
		t.Errorf("retrySQLiteLock() error = %v, want %v", err, want)
	}
	if attempts != 1 {
		t.Errorf("retrySQLiteLock() attempts = %d, want 1", attempts)
	}
}

func TestStoreMigratesPiArtifactHashColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE devbox_sessions (
 id TEXT PRIMARY KEY, project TEXT NOT NULL, task TEXT NOT NULL, base_commit TEXT NOT NULL,
 branch TEXT NOT NULL UNIQUE, vm_name TEXT NOT NULL UNIQUE, vm_identity TEXT NOT NULL,
 ssh_dest TEXT NOT NULL, workspace TEXT NOT NULL, worker_sha256 TEXT NOT NULL,
 pi_session_path TEXT NOT NULL, pi_output_path TEXT NOT NULL,
 pi_evidence TEXT NOT NULL CHECK(json_valid(pi_evidence)), status TEXT NOT NULL, failure TEXT NOT NULL,
 created_at TEXT NOT NULL, updated_at TEXT NOT NULL, destroyed_at TEXT
)`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	factory, err := OpenFactory(t.Context(), path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := factory.Close(); err != nil {
			t.Error(err)
		}
	})
	rows, err := factory.store.db.QueryContext(t.Context(), `PRAGMA table_info(devbox_sessions)`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Error(err)
		}
	}()
	found := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, dataType string
		var notNull, primaryKey int
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if name == "pi_session_sha256" || name == "pi_output_sha256" {
			found[name] = notNull == 1 && defaultValue.Valid && defaultValue.String == "''"
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pi_session_sha256", "pi_output_sha256"} {
		if !found[name] {
			t.Errorf("migrated %s = false, want non-null column with empty default", name)
		}
	}
}

func TestStorePersistsVerifiedPiResult(t *testing.T) {
	store, err := openStore(t.Context(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	cleanupDevboxStore(t, store)
	ctx := t.Context()
	session := testSession(t)
	if err := store.create(ctx, session); err != nil {
		t.Fatal(err)
	}
	result := piResult{
		SessionPath:   "/home/exe/session.jsonl",
		SessionSHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		OutputPath:    "/home/exe/events.jsonl",
		OutputSHA256:  "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		Evidence:      PiEvidence{Provider: piProvider, Model: piModel, ThinkingLevel: piThinking, AssistantMessages: 3, ReasoningTokens: 99, Output: "done"},
	}
	if err := store.setPiResult(ctx, session.ID, result); err != nil {
		t.Fatal(err)
	}
	got, err := store.session(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PiSessionPath != result.SessionPath || got.PiSessionSHA256 != result.SessionSHA256 || got.PiOutputPath != result.OutputPath || got.PiOutputSHA256 != result.OutputSHA256 || got.PiEvidence.ReasoningTokens != 99 || got.PiEvidence.Output != "done" {
		t.Fatalf("session = %#v", got)
	}
}

func TestStoreTransitions(t *testing.T) {
	store, err := openStore(t.Context(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	cleanupDevboxStore(t, store)
	ctx := t.Context()
	session := testSession(t)
	if err := store.create(ctx, session); err != nil {
		t.Fatal(err)
	}
	if err := store.transition(ctx, session.ID, StatusRunning, "", "bad", map[string]any{}); err == nil {
		t.Fatal("illegal transition succeeded")
	}
	for _, status := range []Status{StatusBootstrapping, StatusRunning, StatusAwaitingReview, StatusDestroying, StatusDestroyed} {
		if err := store.transition(ctx, session.ID, status, "", "transition", map[string]any{}); err != nil {
			t.Fatalf("transition to %s: %v", status, err)
		}
	}
}
