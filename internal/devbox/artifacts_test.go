package devbox

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCaptureTaskArtifactsStreamsAtomicallyWithHashes(t *testing.T) {
	root := t.TempDir()
	events := readTaskFixture(t, "pi-events-success.jsonl")
	sessionJSONL := readTaskFixture(t, "pi-session-success.jsonl")
	eventsHash := sha256.Sum256(events)
	sessionHash := sha256.Sum256(sessionJSONL)
	session := testSession(t)
	session.SSHDest = "direct.example"
	installLifecycleHarness(t)
	eventsPath := filepath.Join(t.TempDir(), "events")
	sessionPath := filepath.Join(t.TempDir(), "session")
	if err := os.WriteFile(eventsPath, events, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessionPath, sessionJSONL, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEVBOX_EVENTS", eventsPath)
	t.Setenv("DEVBOX_SESSION", sessionPath)
	factory := &Factory{logs: io.Discard}
	result, err := factory.captureTaskArtifacts(t.Context(), session, taskResult{
		OutputSHA256:  hex.EncodeToString(eventsHash[:]),
		SessionSHA256: hex.EncodeToString(sessionHash[:]),
	}, root)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string][]byte{
		result.OutputPath:  events,
		result.SessionPath: sessionJSONL,
	} {
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(contents, want) {
			t.Fatalf("%s contents changed", path)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o", path, info.Mode().Perm())
		}
		resolvedRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Dir(path) != filepath.Join(resolvedRoot, session.ID) {
			t.Fatalf("artifact escaped session directory: %s", path)
		}
	}
	if result.OutputSHA256 != hex.EncodeToString(eventsHash[:]) || result.SessionSHA256 != hex.EncodeToString(sessionHash[:]) {
		t.Fatalf("result hashes = %#v", result)
	}
}

func TestBoundedArtifactWriterRejectsOverflow(t *testing.T) {
	var output bytes.Buffer
	writer := boundedArtifactWriter{Writer: &output, Remaining: 3}
	if _, err := writer.Write([]byte("four")); err == nil {
		t.Fatal("oversized write succeeded")
	}
	if !writer.Exceeded || output.Len() != 0 {
		t.Fatalf("writer = %#v output = %q", writer, output.String())
	}
}

func TestCaptureTaskArtifactsRejectsHashMismatchAndSymlinkDirectory(t *testing.T) {
	root := t.TempDir()
	session := testSession(t)
	session.SSHDest = "direct.example"
	installLifecycleHarness(t)
	artifactPath := filepath.Join(t.TempDir(), "artifact")
	if err := os.WriteFile(artifactPath, []byte("artifact\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEVBOX_EVENTS", artifactPath)
	t.Setenv("DEVBOX_SESSION", artifactPath)
	factory := &Factory{logs: io.Discard}
	if _, err := factory.captureTaskArtifacts(t.Context(), session, taskResult{OutputSHA256: strings.Repeat("0", 64), SessionSHA256: strings.Repeat("0", 64)}, root); err == nil {
		t.Fatal("captureTaskArtifacts() error = nil, want hash mismatch")
	}

	other := t.TempDir()
	symlinkRoot := filepath.Join(t.TempDir(), "artifacts")
	if err := os.Symlink(other, symlinkRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareArtifactDir(symlinkRoot, session.ID); err == nil {
		t.Fatal("accepted symlink artifact directory")
	}
}
