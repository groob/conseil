package devbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	brokerHost     = "groob-tools"
	controlTimeout = 2 * time.Minute
)

// CreateRequest is a validated request to provision a devbox.
type CreateRequest struct {
	project    string
	task       string
	baseCommit string
	workspace  string
}

// NewCreateRequest validates the user-supplied parts of a create request.
func NewCreateRequest(project, task, baseCommit, workspace string) (CreateRequest, error) {
	if err := validateProject(project); err != nil {
		return CreateRequest{}, err
	}
	if strings.TrimSpace(task) == "" {
		return CreateRequest{}, errors.New("task is empty")
	}
	if err := validateCommit(baseCommit); err != nil {
		return CreateRequest{}, err
	}
	if strings.TrimSpace(workspace) == "" {
		return CreateRequest{}, errors.New("workspace is empty")
	}
	return CreateRequest{project: project, task: task, baseCommit: baseCommit, workspace: workspace}, nil
}

// VMDescription identifies an exe.dev VM returned by the control service.
type VMDescription struct {
	Name     string `json:"name"`
	SSHDest  string `json:"ssh_dest"`
	Identity string `json:"comment"`
}

var (
	errVMAbsent           = errors.New("VM is absent")
	errVMIdentityMismatch = errors.New("VM session identity does not match")
)

func retry(ctx context.Context, attempt int) bool {
	if attempt >= 5 {
		return false
	}
	timer := time.NewTimer(time.Duration(attempt) * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (f *Factory) runControl(ctx context.Context, command command) error {
	attemptCtx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	return runControlCommand(attemptCtx, command)
}

func (f *Factory) controlOutput(ctx context.Context, cmd command) ([]byte, error) {
	return runOutput(ctx, f.runControl, cmd)
}

// Create provisions a devbox and runs its task using workerPath.
func (f *Factory) Create(ctx context.Context, request CreateRequest, workerPath, artifactDir string, protocolOut io.Writer) (session Session, resultErr error) {
	id, branch, vm, err := newNames(request.project)
	if err != nil {
		return Session{}, err
	}
	identity := "conseil-session:" + id
	session = Session{
		ID:         id,
		Project:    request.project,
		Task:       request.task,
		BaseCommit: request.baseCommit,
		Branch:     branch,
		VMName:     vm,
		VMIdentity: identity,
		Workspace:  request.workspace,
		Status:     StatusProvisioning,
	}
	unlock, err := f.store.lockSession(ctx, session.ID)
	if err != nil {
		return Session{}, err
	}
	defer func() {
		if err := unlock(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("unlocking session: %w", err))
		}
	}()
	if err := f.store.create(ctx, session); err != nil {
		return Session{}, fmt.Errorf("persisting session before provisioning: %w", err)
	}
	vmAttempted, grantAttempted := false, false
	retainOnFailure, taskFinished := false, false
	defer func() {
		if resultErr == nil {
			return
		}
		original := resultErr
		if taskFinished {
			return
		}
		if retainOnFailure {
			f.persistRollback(ctx, session.ID, "task.failed", map[string]string{"error": original.Error()})
			finalCtx, cancel := f.cleanupContext(ctx)
			current, err := f.store.session(finalCtx, session.ID)
			if err == nil && current.Status == StatusRunning {
				err = f.store.transition(finalCtx, session.ID, StatusFailed, original.Error(), "session.failed", map[string]string{"error": original.Error()})
			}
			cancel()
			if err != nil {
				f.logf("persisting failed task state: %v\n", err)
			}
			resultErr = original
			return
		}
		f.persistRollback(ctx, session.ID, "create.failed", map[string]string{"error": original.Error()})
		grantRevoked := !grantAttempted
		if grantAttempted {
			revokeErr := f.revokeGrant(ctx, session)
			f.recordRollback(ctx, session.ID, "grant.revoke", revokeErr)
			grantRevoked = revokeErr == nil
		}
		if vmAttempted && grantRevoked {
			removeCtx, cancel := f.cleanupContext(ctx)
			removeErr := f.removeVM(removeCtx, session)
			cancel()
			f.recordRollback(ctx, session.ID, "vm.remove", removeErr)
		}
		finalCtx, cancel := f.cleanupContext(ctx)
		current, err := f.store.session(finalCtx, session.ID)
		if err == nil && current.Status != StatusFailed && current.Status != StatusDestroyed {
			err = f.store.transition(finalCtx, session.ID, StatusFailed, original.Error(), "session.failed", map[string]string{"error": original.Error()})
		}
		cancel()
		if err != nil {
			f.logf("persisting failed create state: %v\n", err)
		}
		resultErr = original
	}()

	if err := f.store.appendEvent(ctx, session.ID, "vm.create.requested", map[string]string{"vm_name": session.VMName}); err != nil {
		return session, fmt.Errorf("recording VM create intent: %w", err)
	}
	vmAttempted = true
	description, err := f.createVM(ctx, session.VMName, session.VMIdentity)
	if err != nil {
		return session, fmt.Errorf("creating VM: %w", err)
	}
	session.SSHDest = description.SSHDest
	if err := f.store.setSSHDest(ctx, session.ID, session.SSHDest); err != nil {
		return session, fmt.Errorf("persisting VM destination: %w", err)
	}
	if err := f.attachIntegrations(ctx, session); err != nil {
		return session, fmt.Errorf("attaching exe.dev integrations: %w", err)
	}
	if err := f.store.appendEvent(ctx, session.ID, "grant.requested", map[string]string{"repository": session.Project}); err != nil {
		return session, fmt.Errorf("recording repository grant intent: %w", err)
	}
	grantAttempted = true
	if err := f.runControl(ctx, command{Name: "ssh", Args: []string{"exe.dev", "ssh", brokerHost, "sudo", "/usr/local/bin/vitrier-broker", "grant", session.VMName, session.Project}, Stdout: f.logs, Stderr: f.logs}); err != nil {
		return session, fmt.Errorf("granting repository access: %w", err)
	}
	if err := f.store.appendEvent(ctx, session.ID, "grant.created", map[string]string{"repository": session.Project}); err != nil {
		return session, fmt.Errorf("recording repository grant: %w", err)
	}
	if err := f.waitSSH(ctx, session.SSHDest); err != nil {
		return session, fmt.Errorf("waiting for SSH: %w", err)
	}
	if err := f.upload(ctx, session, workerPath); err != nil {
		return session, fmt.Errorf("uploading worker: %w", err)
	}
	if err := f.store.appendEvent(ctx, session.ID, "worker.uploaded", map[string]string{"sha256_verified": "true"}); err != nil {
		return session, fmt.Errorf("recording worker upload: %w", err)
	}
	if err := f.store.transition(ctx, session.ID, StatusBootstrapping, "", "bootstrap.started", map[string]string{}); err != nil {
		return session, err
	}
	readyEnvironment, err := f.bootstrap(ctx, session, protocolOut)
	if err != nil {
		return session, fmt.Errorf("bootstrapping guest: %w", err)
	}
	if err := f.store.transition(ctx, session.ID, StatusRunning, "", "pi.started", map[string]string{}); err != nil {
		return session, err
	}
	retainOnFailure = true
	piResult, err := f.runTask(ctx, session, readyEnvironment, artifactDir)
	if err != nil {
		return session, fmt.Errorf("running task with Pi: %w", err)
	}
	if err := f.store.setPiResult(ctx, session.ID, piResult); err != nil {
		return session, fmt.Errorf("persisting Pi result: %w", err)
	}
	if err := f.store.transition(ctx, session.ID, StatusAwaitingReview, "", "session.awaiting_review", map[string]string{}); err != nil {
		return session, err
	}
	taskFinished = true
	session, err = f.store.session(ctx, session.ID)
	if err != nil {
		return session, err
	}
	if err := json.NewEncoder(protocolOut).Encode(piResult); err != nil {
		return session, fmt.Errorf("writing Pi result: %w", err)
	}
	return session, nil
}

func (f *Factory) createVM(ctx context.Context, name, identity string) (VMDescription, error) {
	args := []string{"exe.dev", "new", "--json", "--name", name, "--comment", identity}
	var last error
	for attempt := 1; ; attempt++ {
		output, runErr := f.controlOutput(ctx, command{Name: "ssh", Args: args, Stderr: f.logs})
		if runErr == nil {
			if decoded, decodeErr := decodeVM(output, name, identity); decodeErr == nil {
				return decoded, nil
			} else {
				last = decodeErr
			}
		} else {
			last = runErr
		}
		if found, reconcileErr := f.lookupVM(ctx, name, identity); reconcileErr == nil {
			return found, nil
		} else if errors.Is(reconcileErr, errVMIdentityMismatch) {
			return VMDescription{}, reconcileErr
		}
		if !retry(ctx, attempt) {
			return VMDescription{}, last
		}
	}
}

func decodeVM(data []byte, expectedName, expectedIdentity string) (VMDescription, error) {
	type wireVM struct {
		Name    string `json:"name"`
		VMName  string `json:"vm_name"`
		SSHDest string `json:"ssh_dest"`
		Comment string `json:"comment"`
	}
	var candidates []wireVM
	if err := json.Unmarshal(data, &candidates); err != nil {
		var response struct {
			VM  *wireVM  `json:"vm"`
			VMs []wireVM `json:"vms"`
			wireVM
		}
		if err := json.Unmarshal(data, &response); err != nil {
			return VMDescription{}, fmt.Errorf("decoding VM JSON: %w", err)
		}
		candidates = response.VMs
		if response.VM != nil {
			candidates = append(candidates, *response.VM)
		}
		if response.Name != "" || response.VMName != "" || response.SSHDest != "" {
			candidates = append(candidates, response.wireVM)
		}
	}
	for _, candidate := range candidates {
		name := candidate.VMName
		if name == "" {
			name = candidate.Name
		}
		if name != expectedName {
			continue
		}
		if candidate.Comment != expectedIdentity {
			return VMDescription{}, fmt.Errorf("%w: VM %q has comment %q, want %q", errVMIdentityMismatch, name, candidate.Comment, expectedIdentity)
		}
		if strings.TrimSpace(candidate.SSHDest) == "" {
			return VMDescription{}, errors.New("VM response has empty ssh_dest")
		}
		if err := validateSSHDestination(candidate.SSHDest); err != nil {
			return VMDescription{}, err
		}
		return VMDescription{Name: name, SSHDest: candidate.SSHDest, Identity: candidate.Comment}, nil
	}
	return VMDescription{}, errVMAbsent
}

func (f *Factory) lookupVM(ctx context.Context, name, identity string) (VMDescription, error) {
	output, err := f.controlOutput(ctx, command{Name: "ssh", Args: []string{"exe.dev", "ls", "-l", "--json", name}, Stderr: f.logs})
	if err != nil {
		return VMDescription{}, err
	}
	return decodeVM(output, name, identity)
}

func (f *Factory) attachIntegrations(ctx context.Context, session Session) error {
	for _, integration := range []string{"vitrier", "llm", "reflection"} {
		if err := f.runControl(ctx, command{
			Name:   "ssh",
			Args:   []string{"exe.dev", "integrations", "attach", integration, "vm:" + session.VMName},
			Stdout: f.logs,
			Stderr: f.logs,
		}); err != nil {
			return fmt.Errorf("attaching %s: %w", integration, err)
		}
		if err := f.store.appendEvent(ctx, session.ID, "integration.attached", map[string]string{"integration": integration}); err != nil {
			return fmt.Errorf("recording %s attachment: %w", integration, err)
		}
	}
	return nil
}

func (f *Factory) revoke(ctx context.Context, session Session) error {
	return f.runControl(ctx, command{Name: "ssh", Args: []string{"exe.dev", "ssh", brokerHost, "sudo", "/usr/local/bin/vitrier-broker", "revoke", session.VMName}, Stdout: f.logs, Stderr: f.logs})
}

func (f *Factory) waitSSH(ctx context.Context, dest string) error {
	var last error
	for attempt := 1; ; attempt++ {
		last = f.runControl(ctx, command{Name: "ssh", Args: []string{"-o", "StrictHostKeyChecking=accept-new", dest, "true"}, Stderr: f.logs})
		if last == nil {
			return nil
		}
		if !retry(ctx, attempt) {
			return last
		}
	}
}

func (f *Factory) upload(ctx context.Context, session Session, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening worker: %w", err)
	}
	defer func() { _ = file.Close() }()
	temporary := "/tmp/devbox-" + session.ID
	quoted, err := shellQuote(temporary)
	if err != nil {
		return err
	}
	upload := "umask 077; set -C; cat > " + quoted
	hash := sha256.New()
	if err := f.runControl(ctx, command{Name: "ssh", Args: []string{"-o", "StrictHostKeyChecking=accept-new", session.SSHDest, upload}, Stdin: io.TeeReader(file, hash), Stderr: f.logs}); err != nil {
		return err
	}
	want := hex.EncodeToString(hash.Sum(nil))
	output, err := f.controlOutput(ctx, command{Name: "ssh", Args: []string{session.SSHDest, "sha256sum " + quoted}, Stderr: f.logs})
	if err != nil {
		return fmt.Errorf("hashing remote worker: %w", err)
	}
	got := strings.Fields(string(output))
	if len(got) == 0 || got[0] != want {
		return fmt.Errorf("remote worker SHA-256 mismatch: got %q want %q", strings.TrimSpace(string(output)), want)
	}
	staged := "/usr/local/libexec/.conseil-devbox-" + session.ID + ".new"
	installed := "/usr/local/libexec/conseil-devbox"
	install := "sudo install -d -o root -g root -m 0755 /usr/local/libexec" +
		" && sudo install -o root -g root -m 0755 " + quoted + " " + staged +
		" && sudo mv -f " + staged + " " + installed + " && rm -f " + quoted
	if err := f.runControl(ctx, command{Name: "ssh", Args: []string{session.SSHDest, install}, Stderr: f.logs}); err != nil {
		return fmt.Errorf("installing remote worker: %w", err)
	}
	if err := f.store.setWorkerSHA256(ctx, session.ID, want); err != nil {
		return fmt.Errorf("recording worker SHA-256: %w", err)
	}
	return nil
}

func (f *Factory) bootstrap(ctx context.Context, session Session, protocolOut io.Writer) (map[string]string, error) {
	values := []string{session.ID, session.Project, session.BaseCommit, session.Branch, session.Workspace}
	quoted := make([]string, len(values))
	for i, v := range values {
		q, err := shellQuote(v)
		if err != nil {
			return nil, err
		}
		quoted[i] = q
	}
	remote := "/usr/local/libexec/conseil-devbox bootstrap --session " + quoted[0] + " --project " + quoted[1] + " --ref " + quoted[2] + " --branch " + quoted[3] + " --workspace " + quoted[4]
	var readyEnvironment map[string]string
	protocolErr := runProjectSetupProtocol(ctx, runControlCommand, command{Name: "ssh", Args: []string{session.SSHDest, remote}, Stderr: f.logs}, func(event projectEvent, canonical json.RawMessage) error {
		if event.Event == "ready" {
			readyEnvironment = maps.Clone(event.Environment)
		}
		if err := f.store.appendEvent(ctx, session.ID, "project."+event.Event, json.RawMessage(canonical)); err != nil {
			return err
		}
		_, err := protocolOut.Write(append(canonical, '\n'))
		return err
	})
	if protocolErr != nil {
		return nil, fmt.Errorf("remote bootstrap: %w", protocolErr)
	}
	return readyEnvironment, nil
}

func (f *Factory) runTask(ctx context.Context, session Session, environment map[string]string, artifactDir string) (piResult, error) {
	request, err := json.Marshal(taskRequest{Task: session.Task, Environment: environment})
	if err != nil {
		return piResult{}, fmt.Errorf("encoding task request: %w", err)
	}
	sessionID, err := shellQuote(session.ID)
	if err != nil {
		return piResult{}, err
	}
	workspace, err := shellQuote(session.Workspace)
	if err != nil {
		return piResult{}, err
	}
	remote := "exec /usr/local/libexec/conseil-devbox run-task --session " + sessionID + " --workspace " + workspace
	output, err := runOutput(ctx, runControlCommand, command{Name: "ssh", Args: []string{session.SSHDest, remote}, Stdin: bytes.NewReader(request), Stderr: f.logs})
	if err != nil {
		f.preserveFailedTaskArtifacts(ctx, session, artifactDir)
		return piResult{}, fmt.Errorf("remote task worker: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.DisallowUnknownFields()
	var remoteResult taskResult
	if err := decoder.Decode(&remoteResult); err != nil {
		f.preserveFailedTaskArtifacts(ctx, session, artifactDir)
		return piResult{}, fmt.Errorf("decoding remote Pi result: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		f.preserveFailedTaskArtifacts(ctx, session, artifactDir)
		return piResult{}, errors.New("remote Pi result has trailing data")
	}
	if !validSHA256(remoteResult.OutputSHA256) {
		f.preserveFailedTaskArtifacts(ctx, session, artifactDir)
		return piResult{}, errors.New("remote Pi output SHA-256 is invalid")
	}
	if !validSHA256(remoteResult.SessionSHA256) {
		f.preserveFailedTaskArtifacts(ctx, session, artifactDir)
		return piResult{}, errors.New("remote Pi session SHA-256 is invalid")
	}
	result, err := f.captureTaskArtifacts(ctx, session, remoteResult, artifactDir)
	if err != nil {
		f.preserveFailedTaskArtifacts(ctx, session, artifactDir)
		return piResult{}, fmt.Errorf("capturing verified Pi artifacts: %w", err)
	}
	return result, nil
}

func (f *Factory) preserveFailedTaskArtifacts(parent context.Context, session Session, artifactDir string) {
	captureCtx, cancelCapture := f.cleanupContext(parent)
	result, _ := f.captureTaskArtifacts(captureCtx, session, taskResult{}, artifactDir)
	cancelCapture()
	if result.SessionPath == "" && result.OutputPath == "" {
		return
	}
	storeCtx, cancelStore := f.cleanupContext(parent)
	defer cancelStore()
	if err := f.store.setPiArtifacts(storeCtx, session.ID, result); err != nil {
		f.logf("persisting failed task artifacts: %v\n", err)
	}
}

func (f *Factory) logf(format string, args ...any) {
	_, _ = fmt.Fprintf(f.logs, format, args...)
}

func (f *Factory) cleanupContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), controlTimeout)
}

func (f *Factory) persistRollback(parent context.Context, id, eventType string, data any) {
	ctx, cancel := f.cleanupContext(parent)
	defer cancel()
	if err := f.store.appendEvent(ctx, id, eventType, data); err != nil {
		f.logf("persisting rollback event %q: %v\n", eventType, err)
	}
}

func (f *Factory) recordRollback(parent context.Context, id, operation string, err error) {
	outcome := map[string]any{"operation": operation, "ok": err == nil}
	if err != nil {
		outcome["error"] = err.Error()
	}
	f.persistRollback(parent, id, "rollback.outcome", outcome)
}

func (f *Factory) removeVM(ctx context.Context, session Session) error {
	if _, err := f.lookupVM(ctx, session.VMName, session.VMIdentity); errors.Is(err, errVMAbsent) {
		return nil
	} else if err != nil {
		return fmt.Errorf("verifying VM identity before removal: %w", err)
	}
	removeErr := f.runControl(ctx, command{Name: "ssh", Args: []string{"exe.dev", "rm", session.VMName}, Stdout: f.logs, Stderr: f.logs})
	if _, lookupErr := f.lookupVM(ctx, session.VMName, session.VMIdentity); errors.Is(lookupErr, errVMAbsent) {
		return nil
	} else if lookupErr != nil {
		if removeErr != nil {
			return fmt.Errorf("removing VM failed (%v) and reconciliation failed: %w", removeErr, lookupErr)
		}
		return fmt.Errorf("verifying VM removal: %w", lookupErr)
	}
	if removeErr != nil {
		return removeErr
	}
	return errors.New("VM still exists after successful removal")
}

// DefaultDBPath returns the default devbox database path.
func DefaultDBPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return filepath.Join(".", "devbox.db")
	}
	return filepath.Join(dir, "conseil", "devbox.db")
}
