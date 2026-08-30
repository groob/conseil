package devbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

type store struct {
	db      *sql.DB
	path    string
	lockDir string
}

const (
	storeBusyTimeoutMS     = 5000
	storeMigrationTimeout  = 10 * time.Second
	storeLockRetryInterval = 10 * time.Millisecond
)

func openStore(ctx context.Context, path string) (*store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("database path is empty")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolving database path: %w", err)
	}
	directory := filepath.Dir(absolute)
	if err := prepareStoreDirectory(directory); err != nil {
		return nil, fmt.Errorf("preparing database directory: %w", err)
	}
	if err := preparePrivateFile(absolute, privateFileCreate); err != nil {
		return nil, fmt.Errorf("preparing database file: %w", err)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if err := preparePrivateFile(absolute+suffix, privateFileOptional); err != nil {
			return nil, fmt.Errorf("preparing SQLite file %s: %w", suffix, err)
		}
	}
	lockDir := absolute + ".locks"
	if err := prepareStoreDirectory(lockDir); err != nil {
		return nil, fmt.Errorf("preparing session lock directory: %w", err)
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}
	query := u.Query()
	query.Set("_busy_timeout", strconv.Itoa(storeBusyTimeoutMS))
	query.Set("_foreign_keys", "on")
	u.RawQuery = query.Encode()

	// modernc applies these non-locking settings while opening each physical
	// connection. WAL is configured later because changing it takes a file lock.
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("opening devbox database: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &store{db: db, path: absolute, lockDir: lockDir}
	migrationCtx, cancel := context.WithTimeout(ctx, storeMigrationTimeout)
	defer cancel()
	if err := store.migrate(migrationCtx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := store.secureSQLiteFiles(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func prepareStoreDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("path is not a directory")
	}
	return validateStoreDirectory(info)
}

type privateFilePolicy uint8

const (
	privateFileCreate privateFilePolicy = iota
	privateFileRequired
	privateFileOptional
)

func preparePrivateFile(path string, policy privateFilePolicy) error {
	vanished := func(err error) bool {
		return errors.Is(err, os.ErrNotExist) && policy == privateFileOptional
	}

	before, err := os.Lstat(path)
	created := errors.Is(err, os.ErrNotExist)
	if created && policy == privateFileOptional {
		return nil
	}
	if err != nil && !created {
		return err
	}
	if !created && (!before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0) {
		return errors.New("path is not a regular file")
	}
	flags := os.O_RDWR
	if policy == privateFileCreate {
		flags |= os.O_CREATE
	}
	file, err := os.OpenFile(path, flags, 0o600)
	if vanished(err) {
		return nil
	}
	if err != nil {
		return err
	}
	closeFile := true
	defer func() {
		if closeFile {
			_ = file.Close()
		}
	}()
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	after, err := os.Lstat(path)
	if vanished(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, after) || (!created && !os.SameFile(before, opened)) {
		return errors.New("file changed while opening")
	}
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	closeFile = false
	if created {
		directory, err := os.Open(filepath.Dir(path))
		if err != nil {
			return err
		}
		syncErr := directory.Sync()
		closeErr := directory.Close()
		if syncErr != nil {
			return syncErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func (s *store) secureSQLiteFiles() error {
	if err := preparePrivateFile(s.path, privateFileRequired); err != nil {
		return fmt.Errorf("securing SQLite database: %w", err)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if err := preparePrivateFile(s.path+suffix, privateFileOptional); err != nil {
			return fmt.Errorf("securing SQLite file %s: %w", suffix, err)
		}
	}
	return nil
}

func (s *store) close() error { return s.db.Close() }

func (s *store) migrate(ctx context.Context) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("opening migration connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	err = retrySQLiteLock(ctx, func() error {
		var mode string
		if err := conn.QueryRowContext(ctx, `PRAGMA journal_mode=WAL`).Scan(&mode); err != nil {
			return err
		}
		if !strings.EqualFold(mode, "wal") {
			return fmt.Errorf("journal mode is %q, want WAL", mode)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("setting devbox journal mode: %w", err)
	}
	if err := retrySQLiteLock(ctx, func() error {
		_, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`)
		return err
	}); err != nil {
		return fmt.Errorf("beginning devbox migration: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeMigrationTimeout)
		defer cancel()
		_, _ = conn.ExecContext(rollbackCtx, `ROLLBACK`)
	}()
	_, err = conn.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS devbox_sessions (
 id TEXT PRIMARY KEY,
 project TEXT NOT NULL,
 task TEXT NOT NULL,
 base_commit TEXT NOT NULL,
 branch TEXT NOT NULL UNIQUE,
 vm_name TEXT NOT NULL UNIQUE,
 vm_identity TEXT NOT NULL,
 ssh_dest TEXT NOT NULL,
 workspace TEXT NOT NULL,
 worker_sha256 TEXT NOT NULL,
 pi_session_path TEXT NOT NULL,
 pi_session_sha256 TEXT NOT NULL,
 pi_output_path TEXT NOT NULL,
 pi_output_sha256 TEXT NOT NULL,
 pi_evidence TEXT NOT NULL CHECK(json_valid(pi_evidence)),
 status TEXT NOT NULL CHECK(status IN ('provisioning','bootstrapping','running','awaiting_review','failed','destroying','destroyed')),
 failure TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL,
 destroyed_at TEXT
);
CREATE INDEX IF NOT EXISTS devbox_sessions_project_updated ON devbox_sessions(project, updated_at DESC);
CREATE TABLE IF NOT EXISTS devbox_events (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 session_id TEXT NOT NULL REFERENCES devbox_sessions(id),
 type TEXT NOT NULL,
 data TEXT NOT NULL CHECK(json_valid(data)),
 created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS devbox_events_session_id ON devbox_events(session_id, id);`)
	if err != nil {
		return fmt.Errorf("migrating devbox database: %w", err)
	}
	for _, name := range []string{"pi_session_sha256", "pi_output_sha256"} {
		if err := ensureSessionColumn(ctx, conn, name, "TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("committing devbox migration: %w", err)
	}
	committed = true
	return nil
}

func retrySQLiteLock(ctx context.Context, operation func() error) error {
	for {
		err := operation()
		if err == nil || !isSQLiteLockError(err) {
			return err
		}
		timer := time.NewTimer(storeLockRetryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("%w after SQLite lock: %w", ctx.Err(), err)
		case <-timer.C:
		}
	}
}

func isSQLiteLockError(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	switch sqliteErr.Code() & 0xff {
	case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
		return true
	default:
		return false
	}
}

func ensureSessionColumn(ctx context.Context, conn *sql.Conn, name, definition string) error {
	rows, err := conn.QueryContext(ctx, `PRAGMA table_info(devbox_sessions)`)
	if err != nil {
		return fmt.Errorf("inspecting devbox session columns: %w", err)
	}
	found := false
	for rows.Next() {
		var cid int
		var column, dataType string
		var notNull, primaryKey int
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &column, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return fmt.Errorf("reading devbox session column: %w", err)
		}
		found = found || column == name
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("reading devbox session columns: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("closing devbox session columns: %w", err)
	}
	if found {
		return nil
	}
	if _, err := conn.ExecContext(ctx, `ALTER TABLE devbox_sessions ADD COLUMN `+name+` `+definition); err != nil {
		return fmt.Errorf("adding devbox session column %s: %w", name, err)
	}
	return nil
}

func (s *store) create(ctx context.Context, session Session) error {
	now := time.Now().UTC()
	session.CreatedAt, session.UpdatedAt = now, now
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning session transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	evidence, err := json.Marshal(session.PiEvidence)
	if err != nil {
		return fmt.Errorf("encoding initial Pi evidence: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO devbox_sessions
(id,project,task,base_commit,branch,vm_name,vm_identity,ssh_dest,workspace,worker_sha256,pi_session_path,pi_session_sha256,pi_output_path,pi_output_sha256,pi_evidence,status,failure,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, session.ID, session.Project, session.Task, session.BaseCommit, session.Branch, session.VMName, session.VMIdentity, session.SSHDest, session.Workspace, session.WorkerSHA256, session.PiSessionPath, session.PiSessionSHA256, session.PiOutputPath, session.PiOutputSHA256, string(evidence), session.Status, session.Failure, formatTime(now), formatTime(now))
	if err != nil {
		return fmt.Errorf("inserting session: %w", err)
	}
	if err := appendEventTx(ctx, tx, session.ID, "session.created", session); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing session: %w", err)
	}
	return nil
}

func (s *store) setSSHDest(ctx context.Context, id, dest string) error {
	return s.updateWithEvent(ctx, id, "vm.created", map[string]string{"ssh_dest": dest}, func(tx *sql.Tx, now string) error {
		result, err := tx.ExecContext(ctx, `UPDATE devbox_sessions SET ssh_dest=?,updated_at=? WHERE id=?`, dest, now, id)
		return oneRow(result, err)
	})
}

func (s *store) setWorkerSHA256(ctx context.Context, id, digest string) error {
	return s.updateWithEvent(ctx, id, "worker.verified", map[string]string{"sha256": digest}, func(tx *sql.Tx, now string) error {
		result, err := tx.ExecContext(ctx, `UPDATE devbox_sessions SET worker_sha256=?,updated_at=? WHERE id=?`, digest, now, id)
		return oneRow(result, err)
	})
}

func (s *store) setPiResult(ctx context.Context, id string, result piResult) error {
	evidence, err := json.Marshal(result.Evidence)
	if err != nil {
		return fmt.Errorf("encoding Pi evidence: %w", err)
	}
	return s.updateWithEvent(ctx, id, "pi.verified", result, func(tx *sql.Tx, now string) error {
		query := `UPDATE devbox_sessions SET pi_session_path=?,pi_session_sha256=?,pi_output_path=?,pi_output_sha256=?,pi_evidence=?,updated_at=? WHERE id=?`
		updated, err := tx.ExecContext(ctx, query, result.SessionPath, result.SessionSHA256, result.OutputPath, result.OutputSHA256, string(evidence), now, id)
		return oneRow(updated, err)
	})
}

func (s *store) setPiArtifacts(ctx context.Context, id string, result piResult) error {
	return s.updateWithEvent(ctx, id, "pi.artifacts.preserved", result, func(tx *sql.Tx, now string) error {
		query := `UPDATE devbox_sessions SET pi_session_path=?,pi_session_sha256=?,pi_output_path=?,pi_output_sha256=?,updated_at=? WHERE id=?`
		updated, err := tx.ExecContext(ctx, query, result.SessionPath, result.SessionSHA256, result.OutputPath, result.OutputSHA256, now, id)
		return oneRow(updated, err)
	})
}

func (s *store) transition(ctx context.Context, id string, to Status, failure string, eventType string, data any) error {
	return s.updateWithEvent(ctx, id, eventType, data, func(tx *sql.Tx, now string) error {
		var from Status
		if err := tx.QueryRowContext(ctx, `SELECT status FROM devbox_sessions WHERE id=?`, id).Scan(&from); err != nil {
			return fmt.Errorf("reading session status: %w", err)
		}
		if !validTransition(from, to) {
			return fmt.Errorf("invalid session transition %s to %s", from, to)
		}
		var destroyed any
		if to == StatusDestroyed {
			destroyed = now
		}
		result, err := tx.ExecContext(ctx, `UPDATE devbox_sessions SET status=?,failure=?,updated_at=?,destroyed_at=COALESCE(?,destroyed_at) WHERE id=? AND status=?`, to, failure, now, destroyed, id, from)
		return oneRow(result, err)
	})
}

func (s *store) appendEvent(ctx context.Context, id, eventType string, data any) error {
	return s.updateWithEvent(ctx, id, eventType, data, func(*sql.Tx, string) error { return nil })
}

func (s *store) updateWithEvent(ctx context.Context, id, eventType string, data any, update func(*sql.Tx, string) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning devbox transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := formatTime(time.Now().UTC())
	if err := update(tx, now); err != nil {
		return err
	}
	if err := appendEventTx(ctx, tx, id, eventType, data); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing devbox transaction: %w", err)
	}
	return nil
}

func appendEventTx(ctx context.Context, tx *sql.Tx, id, eventType string, data any) error {
	encoded, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("encoding %s event: %w", eventType, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO devbox_events(session_id,type,data,created_at) VALUES(?,?,?,?)`, id, eventType, encoded, formatTime(time.Now().UTC())); err != nil {
		return fmt.Errorf("inserting %s event: %w", eventType, err)
	}
	return nil
}

func oneRow(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking updated session: %w", err)
	}
	if count != 1 {
		return errors.New("session update did not affect one row")
	}
	return nil
}

func (s *store) session(ctx context.Context, id string) (Session, error) {
	return scanSession(s.db.QueryRowContext(ctx, `SELECT id,project,task,base_commit,branch,vm_name,vm_identity,ssh_dest,workspace,worker_sha256,pi_session_path,pi_session_sha256,pi_output_path,pi_output_sha256,pi_evidence,status,failure,created_at,updated_at,destroyed_at FROM devbox_sessions WHERE id=?`, id))
}

func (s *store) list(ctx context.Context, project string) ([]Session, error) {
	query := `SELECT id,project,task,base_commit,branch,vm_name,vm_identity,ssh_dest,workspace,worker_sha256,pi_session_path,pi_session_sha256,pi_output_path,pi_output_sha256,pi_evidence,status,failure,created_at,updated_at,destroyed_at FROM devbox_sessions`
	var args []any
	if project != "" {
		query += ` WHERE project=?`
		args = append(args, project)
	}
	query += ` ORDER BY created_at DESC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var sessions []Session
	for rows.Next() {
		session, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating sessions: %w", err)
	}
	return sessions, nil
}

type scanner interface{ Scan(...any) error }

func scanSession(row scanner) (Session, error) {
	var s Session
	var created, updated, evidence string
	var destroyed sql.NullString
	if err := row.Scan(&s.ID, &s.Project, &s.Task, &s.BaseCommit, &s.Branch, &s.VMName, &s.VMIdentity, &s.SSHDest, &s.Workspace, &s.WorkerSHA256, &s.PiSessionPath, &s.PiSessionSHA256, &s.PiOutputPath, &s.PiOutputSHA256, &evidence, &s.Status, &s.Failure, &created, &updated, &destroyed); err != nil {
		return Session{}, fmt.Errorf("reading session: %w", err)
	}
	if err := json.Unmarshal([]byte(evidence), &s.PiEvidence); err != nil {
		return Session{}, fmt.Errorf("decoding Pi evidence: %w", err)
	}
	var err error
	if s.CreatedAt, err = parseTime(created); err != nil {
		return Session{}, err
	}
	if s.UpdatedAt, err = parseTime(updated); err != nil {
		return Session{}, err
	}
	if destroyed.Valid {
		value, err := parseTime(destroyed.String)
		if err != nil {
			return Session{}, err
		}
		s.DestroyedAt = &value
	}
	return s, nil
}

func (s *store) hasEvent(ctx context.Context, id, eventType string) (bool, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM devbox_events WHERE session_id=? AND type=?)`, id, eventType).Scan(&exists); err != nil {
		return false, fmt.Errorf("checking %s event: %w", eventType, err)
	}
	return exists, nil
}

func formatTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }
func parseTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing database time %q: %w", value, err)
	}
	return parsed, nil
}
