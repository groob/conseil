package devbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecodeVMLobbyJSONRequiresSessionIdentity(t *testing.T) {
	vm, err := decodeVM([]byte(`{"vms":[{"vm_name":"box","ssh_dest":"vm+box@vm.exe.xyz","comment":"identity"}]}`), "box", "identity")
	if err != nil {
		t.Fatal(err)
	}
	if vm.Name != "box" || vm.SSHDest != "vm+box@vm.exe.xyz" || vm.Identity != "identity" {
		t.Errorf("decodeVM() = %#v, want requested VM identity", vm)
	}
	_, err = decodeVM([]byte(`{"vm_name":"box","ssh_dest":"box.exe.xyz","comment":"other"}`), "box", "identity")
	if !errors.Is(err, errVMIdentityMismatch) {
		t.Errorf("decodeVM() error = %v, want %v", err, errVMIdentityMismatch)
	}
}

func TestCreateRunsTaskWithReadyEnvironmentAndAwaitsReview(t *testing.T) {
	store, err := openStore(t.Context(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	cleanupDevboxStore(t, store)
	worker := filepath.Join(t.TempDir(), "worker")
	body := []byte("worker binary")
	if err := os.WriteFile(worker, body, 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	task := "fix 'quotes'; $(touch /tmp/pwned)\nthen continue"
	events := readTaskFixture(t, "pi-events-success.jsonl")
	sessionJSONL := readTaskFixture(t, "pi-session-success.jsonl")
	artifactDir := t.TempDir()
	harness := configureCreateHarness(t, hex.EncodeToString(sum[:]), events, sessionJSONL)
	factory := &Factory{store: store, logs: io.Discard}
	var output strings.Builder
	session, err := factory.Create(t.Context(), testCreateRequest(t, task), worker, artifactDir, &output)
	if err != nil {
		t.Fatal(err)
	}
	if session.Status != StatusAwaitingReview {
		t.Errorf("created session status = %s, want %s", session.Status, StatusAwaitingReview)
	}
	resolvedArtifacts, err := filepath.EvalSymlinks(artifactDir)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(session.PiSessionPath) != filepath.Join(resolvedArtifacts, session.ID) || session.PiEvidence.ReasoningTokens != 16 || !validSHA256(session.PiSessionSHA256) || !validSHA256(session.PiOutputSHA256) {
		t.Errorf("create() session = %#v, want verified Pi artifacts", session)
	}
	var taskRequest taskRequest
	if err := json.Unmarshal(readTestFile(t, harness.request), &taskRequest); err != nil {
		t.Fatal(err)
	}
	if taskRequest.Task != task {
		t.Errorf("remote task = %q, want %q", taskRequest.Task, task)
	}
	if taskRequest.Environment["SAMOVAR_TEST_DATABASE_URL"] != "postgres://test" {
		t.Errorf("remote task environment = %#v, want Samovar database URL", taskRequest.Environment)
	}
	commands := string(readTestFile(t, harness.commands))
	if strings.Contains(commands, "run-task") && strings.Contains(commands, "touch /tmp/pwned") {
		t.Fatalf("task appeared in remote shell command: %q", commands)
	}
	if !strings.Contains(output.String(), `"assistant_messages":2`) {
		t.Errorf("create() protocol output = %s, want verified result", output.String())
	}
}

var errResultOutput = errors.New("result output failed")

type rejectPiResultWriter struct{ store *store }

func (w rejectPiResultWriter) Write(data []byte) (int, error) {
	if !bytes.Contains(data, []byte(`"session_path"`)) {
		return len(data), nil
	}
	sessions, err := w.store.list(context.Background(), "")
	if err != nil {
		return 0, err
	}
	if len(sessions) != 1 {
		return 0, errors.New("completed session is missing")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	unlock, err := w.store.lockSession(ctx, sessions[0].ID)
	if err == nil {
		_ = unlock()
		return 0, errors.New("session lock was released before result output")
	}
	if !errors.Is(err, context.Canceled) {
		return 0, err
	}
	return 0, errResultOutput
}

func TestCreateKeepsCompletedStateWhenResultOutputFails(t *testing.T) {
	store, err := openStore(t.Context(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	cleanupDevboxStore(t, store)
	worker := filepath.Join(t.TempDir(), "worker")
	body := []byte("worker binary")
	if err := os.WriteFile(worker, body, 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	harness := configureCreateHarness(t, hex.EncodeToString(sum[:]), readTaskFixture(t, "pi-events-success.jsonl"), readTaskFixture(t, "pi-session-success.jsonl"))
	factory := &Factory{store: store, logs: io.Discard}
	session, err := factory.Create(t.Context(), testCreateRequest(t, "task"), worker, t.TempDir(), rejectPiResultWriter{store: store})
	if !errors.Is(err, errResultOutput) || !strings.Contains(err.Error(), "writing Pi result") {
		t.Fatalf("Create() error = %v, want result output error", err)
	}
	if session.Status != StatusAwaitingReview {
		t.Fatalf("returned session status = %s, want %s", session.Status, StatusAwaitingReview)
	}
	stored, err := store.session(t.Context(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != StatusAwaitingReview {
		t.Fatalf("stored session status = %s, want %s", stored.Status, StatusAwaitingReview)
	}
	failed, err := store.hasEvent(t.Context(), session.ID, "task.failed")
	if err != nil {
		t.Fatal(err)
	}
	if failed {
		t.Fatal("presentation failure created a task failure event")
	}
	operations, err := os.ReadFile(harness.operations)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if len(operations) != 0 {
		t.Fatalf("presentation failure triggered rollback operations: %q", operations)
	}
}

func TestCreateRollsBackGrantAndVMOnBootstrapFailure(t *testing.T) {
	store, err := openStore(t.Context(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	cleanupDevboxStore(t, store)
	worker := filepath.Join(t.TempDir(), "worker")
	body := []byte("worker binary")
	if err := os.WriteFile(worker, body, 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	harness := configureCreateHarness(t, hex.EncodeToString(sum[:]), readTaskFixture(t, "pi-events-success.jsonl"), readTaskFixture(t, "pi-session-success.jsonl"))
	t.Setenv("DEVBOX_SCENARIO", "bootstrap_failure")
	factory := &Factory{store: store, logs: io.Discard}
	session, err := factory.Create(t.Context(), testCreateRequest(t, "task"), worker, t.TempDir(), io.Discard)
	if err == nil {
		t.Fatal("create() error = nil, want bootstrap failure")
	}
	if got, want := string(readTestFile(t, harness.operations)), "revoke\nremove\n"; got != want {
		t.Errorf("rollback operations = %q, want %q", got, want)
	}
	stored, err := store.session(t.Context(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != StatusFailed {
		t.Errorf("rolled-back session status = %s, want %s", stored.Status, StatusFailed)
	}
	revoked, err := store.hasEvent(t.Context(), session.ID, "grant.revoked")
	if err != nil {
		t.Fatal(err)
	}
	if !revoked {
		t.Fatal("successful revocation event was not persisted")
	}
}

func TestCreateRetainsVMWhenGrantRevocationFails(t *testing.T) {
	store, err := openStore(t.Context(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	cleanupDevboxStore(t, store)
	worker := filepath.Join(t.TempDir(), "worker")
	body := []byte("worker binary")
	if err := os.WriteFile(worker, body, 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	harness := configureCreateHarness(t, hex.EncodeToString(sum[:]), readTaskFixture(t, "pi-events-success.jsonl"), readTaskFixture(t, "pi-session-success.jsonl"))
	t.Setenv("DEVBOX_SCENARIO", "bootstrap_failure")
	t.Setenv("DEVBOX_REVOKE_FAIL", "1")
	factory := &Factory{store: store, logs: io.Discard}
	session, err := factory.Create(t.Context(), testCreateRequest(t, "task"), worker, t.TempDir(), io.Discard)
	if err == nil {
		t.Fatal("Create() error = nil, want bootstrap failure")
	}
	if got, want := string(readTestFile(t, harness.operations)), "revoke\n"; got != want {
		t.Errorf("rollback operations = %q, want %q", got, want)
	}
	stored, err := store.session(t.Context(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != StatusFailed {
		t.Errorf("rolled-back session status = %s, want %s", stored.Status, StatusFailed)
	}
	revoked, err := store.hasEvent(t.Context(), session.ID, "grant.revoked")
	if err != nil {
		t.Fatal(err)
	}
	if revoked {
		t.Fatal("failed revocation was recorded as successful")
	}
}

func TestBootstrapAcceptsEmptyReadyEnvironment(t *testing.T) {
	store, err := openStore(t.Context(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	cleanupDevboxStore(t, store)
	session := testSession(t)
	session.SSHDest = "direct.example"
	if err := store.create(t.Context(), session); err != nil {
		t.Fatal(err)
	}
	installLifecycleHarness(t)
	t.Setenv("DEVBOX_READY", `{"protocol":1,"event":"ready","environment":{}}`)
	factory := &Factory{store: store, logs: io.Discard}
	environment, err := factory.bootstrap(t.Context(), session, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(environment) != 0 {
		t.Errorf("bootstrap() environment = %#v, want empty", environment)
	}
}

func TestCreateTaskFailurePreservesVMAndGrant(t *testing.T) {
	store, err := openStore(t.Context(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	cleanupDevboxStore(t, store)
	worker := filepath.Join(t.TempDir(), "worker")
	body := []byte("worker binary")
	if err := os.WriteFile(worker, body, 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	events := readTaskFixture(t, "pi-events-success.jsonl")
	sessionJSONL := readTaskFixture(t, "pi-session-success.jsonl")
	artifactDir := t.TempDir()
	harness := configureCreateHarness(t, hex.EncodeToString(sum[:]), events, sessionJSONL)
	t.Setenv("DEVBOX_SCENARIO", "task_failure")
	factory := &Factory{store: store, logs: io.Discard}
	session, err := factory.Create(t.Context(), testCreateRequest(t, "task"), worker, artifactDir, io.Discard)
	if err == nil {
		t.Fatal("create() error = nil, want task failure")
	}
	commands := string(readTestFile(t, harness.commands))
	if strings.Contains(commands, "vitrier-broker revoke") || strings.Contains(commands, "exe.dev rm") {
		t.Fatalf("task failure triggered rollback commands:\n%s", commands)
	}
	stored, err := store.session(t.Context(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != StatusFailed || stored.Failure == "" || stored.SSHDest == "" {
		t.Fatalf("stored session = %#v, want retained failed task", stored)
	}
	if stored.PiSessionPath == "" || stored.PiOutputPath == "" || !validSHA256(stored.PiSessionSHA256) || !validSHA256(stored.PiOutputSHA256) {
		t.Fatalf("failed task artifacts = %#v", stored)
	}
	failed, err := store.hasEvent(t.Context(), session.ID, "task.failed")
	if err != nil {
		t.Fatal(err)
	}
	if !failed {
		t.Fatal("task failure event was not persisted")
	}
}

func TestCreateReconcilesTimeoutUsingPersistedIdentity(t *testing.T) {
	harness := installLifecycleHarness(t)
	t.Setenv("DEVBOX_SCENARIO", "reconcile")
	factory := &Factory{logs: io.Discard}
	vm, err := factory.createVM(t.Context(), "box", "identity")
	if err != nil {
		t.Fatal(err)
	}
	commands := string(readTestFile(t, harness.commands))
	if vm.Identity != "identity" || strings.Count(commands, "exe.dev new") != 1 || !strings.Contains(commands, "exe.dev ls -l --json") {
		t.Fatalf("createVM() = %#v; commands:\n%s", vm, commands)
	}
}

func TestRemoveVMRejectsSuccessfulRemovalThatStillListsVM(t *testing.T) {
	installLifecycleHarness(t)
	t.Setenv("DEVBOX_SCENARIO", "keep_vm")
	t.Setenv("DEVBOX_VM_NAME", "box")
	t.Setenv("DEVBOX_VM_IDENTITY", "identity")
	session := Session{VMName: "box", VMIdentity: "identity"}
	factory := &Factory{logs: io.Discard}
	if err := factory.removeVM(t.Context(), session); errString(err) != "VM still exists after successful removal" {
		t.Errorf("removeVM() error = %q, want successful-removal inconsistency", errString(err))
	}
}

func TestRemoveVMRefusesReusedName(t *testing.T) {
	harness := installLifecycleHarness(t)
	t.Setenv("DEVBOX_SCENARIO", "identity_reuse")
	t.Setenv("DEVBOX_VM_NAME", "box")
	t.Setenv("DEVBOX_VM_IDENTITY", "identity")
	session := Session{VMName: "box", VMIdentity: "identity"}
	factory := &Factory{logs: io.Discard}
	err := factory.removeVM(t.Context(), session)
	if !errors.Is(err, errVMIdentityMismatch) {
		t.Errorf("removeVM() error = %v, want %v", err, errVMIdentityMismatch)
	}
	commands := string(readTestFile(t, harness.commands))
	if strings.Contains(commands, "exe.dev rm") {
		t.Fatalf("removed a VM with a reused name; commands:\n%s", commands)
	}
}

func configureCreateHarness(t *testing.T, workerSHA string, events, session []byte) lifecycleHarness {
	t.Helper()
	harness := installLifecycleHarness(t)
	root := t.TempDir()
	eventsPath := filepath.Join(root, "events")
	sessionPath := filepath.Join(root, "session")
	resultPath := filepath.Join(root, "result")
	if err := os.WriteFile(eventsPath, events, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessionPath, session, 0o600); err != nil {
		t.Fatal(err)
	}
	var result strings.Builder
	if err := writeTestTaskResult(&result, events, session); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resultPath, []byte(result.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEVBOX_WORKER_SHA", workerSHA)
	t.Setenv("DEVBOX_EVENTS", eventsPath)
	t.Setenv("DEVBOX_SESSION", sessionPath)
	t.Setenv("DEVBOX_TASK_RESULT", resultPath)
	return harness
}

func writeTestTaskResult(output io.Writer, events, session []byte) error {
	eventsHash := sha256.Sum256(events)
	sessionHash := sha256.Sum256(session)
	return json.NewEncoder(output).Encode(taskResult{
		SessionSHA256: hex.EncodeToString(sessionHash[:]),
		OutputSHA256:  hex.EncodeToString(eventsHash[:]),
		Evidence: PiEvidence{
			Provider:          piProvider,
			Model:             piModel,
			ThinkingLevel:     piThinking,
			AssistantMessages: 2,
			ReasoningTokens:   16,
			Output:            "done",
		},
	})
}

func testCreateRequest(t *testing.T, task string) CreateRequest {
	t.Helper()
	request, err := NewCreateRequest("counseil", task, "0123456789012345678901234567890123456789", "/work")
	if err != nil {
		t.Fatal(err)
	}
	return request
}
