package devbox

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDestroyRefusesUnreachableUnlessForced(t *testing.T) {
	store, err := openStore(t.Context(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	cleanupDevboxStore(t, store)
	ctx := t.Context()
	session := testSession(t)
	session.SSHDest = "direct"
	session.WorkerSHA256 = strings.Repeat("0", 64)
	if err := store.create(ctx, session); err != nil {
		t.Fatal(err)
	}
	if err := store.transition(ctx, session.ID, StatusBootstrapping, "", "x", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if err := store.transition(ctx, session.ID, StatusRunning, "", "x", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	harness := configureDestroyHarness(t, session)
	t.Setenv("DEVBOX_SCENARIO", "unreachable")
	factory := &Factory{store: store, logs: io.Discard}
	if err := factory.Destroy(ctx, session.ID, false); err == nil {
		t.Error("destroy() error = nil, want unreachable guest error")
	}
	commands := string(readTestFile(t, harness.commands))
	if strings.Count(commands, "inspect-guest") != 1 {
		t.Fatalf("commands = %q, want one inspection", commands)
	}
	if err := factory.Destroy(ctx, session.ID, true); err != nil {
		t.Fatal(err)
	}
	got, err := store.session(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDestroyed {
		t.Errorf("destroyed session status = %s, want %s", got.Status, StatusDestroyed)
	}
}

func TestDestroyRevokesBeforeRemovingVM(t *testing.T) {
	store, err := openStore(t.Context(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	cleanupDevboxStore(t, store)
	ctx := t.Context()
	session := testSession(t)
	session.SSHDest = "direct"
	session.WorkerSHA256 = strings.Repeat("0", 64)
	if err := store.create(ctx, session); err != nil {
		t.Fatal(err)
	}
	if err := store.transition(ctx, session.ID, StatusBootstrapping, "", "x", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if err := store.transition(ctx, session.ID, StatusRunning, "", "x", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	harness := configureDestroyHarness(t, session)
	factory := &Factory{store: store, logs: io.Discard}
	if err := factory.Destroy(ctx, session.ID, false); err != nil {
		t.Fatal(err)
	}
	if got, want := string(readTestFile(t, harness.operations)), "revoke\nremove\n"; got != want {
		t.Fatalf("destroy operations = %q, want %q", got, want)
	}
}

func TestDestroyChecksVMIdentityBeforeRevoking(t *testing.T) {
	store, err := openStore(t.Context(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	cleanupDevboxStore(t, store)
	session := testSession(t)
	session.SSHDest = "direct"
	if err := store.create(t.Context(), session); err != nil {
		t.Fatal(err)
	}
	harness := configureDestroyHarness(t, session)
	t.Setenv("DEVBOX_SCENARIO", "identity_reuse")
	factory := &Factory{store: store, logs: io.Discard}
	err = factory.Destroy(t.Context(), session.ID, true)
	if !errors.Is(err, errVMIdentityMismatch) {
		t.Fatalf("Destroy() error = %v, want VM identity mismatch", err)
	}
	commands := string(readTestFile(t, harness.commands))
	if strings.Contains(commands, "vitrier-broker revoke") || strings.Contains(commands, "exe.dev rm") {
		t.Fatalf("identity mismatch changed worker resources:\n%s", commands)
	}
}

func cleanupDevboxStore(t *testing.T, store *store) {
	t.Helper()
	t.Cleanup(func() {
		if err := store.close(); err != nil {
			t.Error(err)
		}
	})
}

func TestDestroyRetryRequiresForceAfterRevocation(t *testing.T) {
	store, err := openStore(t.Context(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	cleanupDevboxStore(t, store)
	ctx := t.Context()
	session := testSession(t)
	session.SSHDest = "direct"
	session.WorkerSHA256 = strings.Repeat("0", 64)
	if err := store.create(ctx, session); err != nil {
		t.Fatal(err)
	}
	if err := store.transition(ctx, session.ID, StatusBootstrapping, "", "x", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if err := store.transition(ctx, session.ID, StatusRunning, "", "x", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	harness := configureDestroyHarness(t, session)
	t.Setenv("DEVBOX_SCENARIO", "remove_fail_once")
	t.Setenv("DEVBOX_REMOVE_ATTEMPT", filepath.Join(t.TempDir(), "remove-attempt"))
	factory := &Factory{store: store, logs: io.Discard}
	if err := factory.Destroy(ctx, session.ID, false); err == nil {
		t.Fatal("first destroy succeeded")
	}
	stored, err := store.session(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != StatusDestroying {
		t.Fatalf("status after failed remove = %s", stored.Status)
	}
	want := "previous destroy attempt revoked the grant; inspect the VM manually and retry with --force"
	if err := factory.Destroy(ctx, session.ID, false); errString(err) != want {
		t.Errorf("safe retry error = %q, want %q", errString(err), want)
	}
	if err := factory.Destroy(ctx, session.ID, true); err != nil {
		t.Fatal(err)
	}
	commands := string(readTestFile(t, harness.commands))
	operations := string(readTestFile(t, harness.operations))
	if strings.Count(commands, "inspect-guest") != 1 || strings.Count(operations, "revoke\n") != 1 || strings.Count(operations, "remove\n") != 2 {
		t.Fatalf("destroy commands:\n%s\noperations:\n%s", commands, operations)
	}
}

func configureDestroyHarness(t *testing.T, session Session) lifecycleHarness {
	t.Helper()
	harness := installLifecycleHarness(t)
	factsPath := filepath.Join(t.TempDir(), "facts")
	facts := `{"clean":true,"head":"` + session.BaseCommit + `","branch":"` + session.Branch + `","remote_present":false}`
	if err := os.WriteFile(factsPath, []byte(facts), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEVBOX_FACTS", factsPath)
	t.Setenv("DEVBOX_VM_NAME", session.VMName)
	t.Setenv("DEVBOX_VM_IDENTITY", session.VMIdentity)
	t.Setenv("DEVBOX_SSH_DEST", session.SSHDest)
	return harness
}
