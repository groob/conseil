package devbox

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const (
	sessionLockHelperEnv = "CONSEIL_SESSION_LOCK_HELPER"
	sessionLockPathEnv   = "CONSEIL_SESSION_LOCK_PATH"
)

func TestSessionLockProcessHelper(t *testing.T) {
	if os.Getenv(sessionLockHelperEnv) != "1" {
		return
	}
	store, err := openStore(t.Context(), os.Getenv(sessionLockPathEnv))
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := store.lockSession(t.Context(), "devbox_01234567890123456789012345678901")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(os.Stdout, "locked"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatal(err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	if err := store.close(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionLockSerializesProcessesAndHonorsContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestSessionLockProcessHelper$")
	command.Env = append(os.Environ(), sessionLockHelperEnv+"=1", sessionLockPathEnv+"="+path)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	childRunning := true
	t.Cleanup(func() {
		_ = stdin.Close()
		if childRunning {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("waiting for child lock: %v: %s", err, stderr.String())
	}
	if line != "locked\n" {
		t.Fatalf("child output = %q, want lock confirmation", line)
	}

	store, err := openStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	cleanupDevboxStore(t, store)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := store.lockSession(ctx, "devbox_01234567890123456789012345678901"); !errors.Is(err, context.Canceled) {
		t.Fatalf("contended lock error = %v, want context cancellation", err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("child lock process: %v: %s", err, stderr.String())
	}
	childRunning = false

	unlock, err := store.lockSession(t.Context(), "devbox_01234567890123456789012345678901")
	if err != nil {
		t.Fatal(err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
}
