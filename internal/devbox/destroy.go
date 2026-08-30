package devbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

// RepositoryFacts is the guest's account of repository state relevant to safe destruction.
type RepositoryFacts struct {
	Clean           bool     `json:"clean"`
	Head            string   `json:"head"`
	Branch          string   `json:"branch"`
	Stashed         bool     `json:"stashed"`
	OtherBranches   []string `json:"other_branches,omitempty"`
	UnexpectedRefs  []string `json:"unexpected_refs,omitempty"`
	UnpushedCommits []string `json:"unpushed_commits,omitempty"`
	RemotePresent   bool     `json:"remote_present"`
	RemoteHead      string   `json:"remote_head,omitempty"`
}

// Inspection combines durable session state with reachable VM and repository state.
type Inspection struct {
	Session    Session          `json:"session"`
	VM         *VMDescription   `json:"vm,omitempty"`
	VMError    string           `json:"vm_error,omitempty"`
	Repository *RepositoryFacts `json:"repository,omitempty"`
	RepoError  string           `json:"repository_error,omitempty"`
}

func (f *Factory) guestFacts(ctx context.Context, session Session) (RepositoryFacts, error) {
	values := []string{session.Workspace, session.Project, session.Branch, session.BaseCommit}
	quoted := make([]string, len(values))
	for i, v := range values {
		q, err := shellQuote(v)
		if err != nil {
			return RepositoryFacts{}, err
		}
		quoted[i] = q
	}
	if len(session.WorkerSHA256) != 64 {
		return RepositoryFacts{}, errors.New("session has no verified worker SHA-256")
	}
	remote := `set -eu; worker=/usr/local/libexec/conseil-devbox; test "$(stat -c '%u:%a' "$worker")" = "0:755"; actual=$(sha256sum "$worker"); test "${actual%% *}" = ` + session.WorkerSHA256 + `; exec "$worker" inspect-guest --workspace ` + quoted[0] + ` --project ` + quoted[1] + ` --branch ` + quoted[2] + ` --base ` + quoted[3]
	output, err := runOutput(ctx, runControlCommand, command{Name: "ssh", Args: []string{session.SSHDest, remote}, Stderr: f.logs})
	if err != nil {
		return RepositoryFacts{}, fmt.Errorf("reaching guest inspection: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.DisallowUnknownFields()
	var facts RepositoryFacts
	if err := decoder.Decode(&facts); err != nil {
		return RepositoryFacts{}, fmt.Errorf("decoding guest inspection: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return RepositoryFacts{}, errors.New("guest inspection has trailing data")
	}
	if err := validateCommit(facts.Head); err != nil {
		return RepositoryFacts{}, fmt.Errorf("guest inspection HEAD: %w", err)
	}
	if facts.Branch != session.Branch {
		return RepositoryFacts{}, fmt.Errorf("guest inspection branch %q does not match %q", facts.Branch, session.Branch)
	}
	if facts.RemotePresent {
		if err := validateCommit(facts.RemoteHead); err != nil {
			return RepositoryFacts{}, fmt.Errorf("guest inspection remote HEAD: %w", err)
		}
	}
	return facts, nil
}

func checkDestroySafety(session Session, facts RepositoryFacts) error {
	if !facts.Clean {
		return errors.New("workspace has dirty or untracked files")
	}
	if facts.Stashed {
		return errors.New("workspace has stashed work")
	}
	if len(facts.OtherBranches) > 0 {
		return fmt.Errorf("workspace has other local branches: %s", strings.Join(facts.OtherBranches, ", "))
	}
	if len(facts.UnexpectedRefs) > 0 {
		return fmt.Errorf("workspace has unexpected Git refs: %s", strings.Join(facts.UnexpectedRefs, ", "))
	}
	if len(facts.UnpushedCommits) > 0 {
		return fmt.Errorf("workspace has commits reachable only from local refs or reflogs: %s", strings.Join(facts.UnpushedCommits, ", "))
	}
	if facts.RemotePresent {
		if facts.Head != facts.RemoteHead {
			return fmt.Errorf("local HEAD %s does not match remote branch %s", facts.Head, facts.RemoteHead)
		}
		return nil
	}
	if facts.Head != session.BaseCommit {
		return fmt.Errorf("remote branch is absent and local HEAD %s differs from base %s", facts.Head, session.BaseCommit)
	}
	return nil
}

func (f *Factory) revokeGrant(parent context.Context, session Session) error {
	revokeCtx, cancel := f.cleanupContext(parent)
	err := f.revoke(revokeCtx, session)
	cancel()
	if err != nil {
		return fmt.Errorf("revoking broker grant: %w", err)
	}
	persistCtx, cancel := f.cleanupContext(parent)
	defer cancel()
	if err := f.store.appendEvent(persistCtx, session.ID, "grant.revoked", map[string]string{"vm_name": session.VMName}); err != nil {
		return fmt.Errorf("recording revoked grant: %w", err)
	}
	return nil
}

// Destroy removes a devbox after checking that its repository work is safe.
func (f *Factory) Destroy(ctx context.Context, id string, force bool) (resultErr error) {
	unlock, err := f.store.lockSession(ctx, id)
	if err != nil {
		return err
	}
	defer func() {
		if err := unlock(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("unlocking session: %w", err))
		}
	}()
	session, err := f.store.session(ctx, id)
	if err != nil {
		return err
	}
	if session.Status == StatusDestroyed {
		return nil
	}
	grantRevoked, err := f.store.hasEvent(ctx, id, "grant.revoked")
	if err != nil {
		return err
	}
	if grantRevoked && !force {
		return errors.New("previous destroy attempt revoked the grant; inspect the VM manually and retry with --force")
	}
	if !force {
		facts, err := f.guestFacts(ctx, session)
		if err != nil {
			return fmt.Errorf("refusing destroy without trustworthy guest inspection: %w", err)
		}
		if err := checkDestroySafety(session, facts); err != nil {
			return fmt.Errorf("refusing destroy: %w", err)
		}
	}
	if !grantRevoked {
		if _, err := f.lookupVM(ctx, session.VMName, session.VMIdentity); err != nil && !errors.Is(err, errVMAbsent) {
			return fmt.Errorf("verifying VM identity before grant revocation: %w", err)
		}
	}
	if session.Status != StatusDestroying {
		if err := f.store.transition(ctx, id, StatusDestroying, "", "destroy.started", map[string]bool{"force": force}); err != nil {
			return err
		}
	}
	if !grantRevoked {
		if err := f.revokeGrant(ctx, session); err != nil {
			f.persistRollback(ctx, id, "destroy.failed", map[string]string{"operation": "revoke", "error": err.Error()})
			return err
		}
	}
	removeCtx, cancel := f.cleanupContext(ctx)
	removeErr := f.removeVM(removeCtx, session)
	cancel()
	if removeErr != nil {
		wrapped := fmt.Errorf("removing VM: %w", removeErr)
		f.persistRollback(ctx, id, "destroy.failed", map[string]string{"operation": "remove", "error": wrapped.Error()})
		return wrapped
	}
	persistCtx, cancel := f.cleanupContext(ctx)
	defer cancel()
	return f.store.transition(persistCtx, id, StatusDestroyed, "", "session.destroyed", map[string]any{})
}

// Inspect returns durable session state and any reachable live state. The VM
// and guest lookups target independent hosts (the exe.dev control host and
// the session's own VM), so they run concurrently.
func (f *Factory) Inspect(ctx context.Context, id string) (Inspection, error) {
	session, err := f.store.session(ctx, id)
	if err != nil {
		return Inspection{}, err
	}
	inspection := Inspection{Session: session}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if vm, err := f.lookupVM(ctx, session.VMName, session.VMIdentity); err != nil {
			inspection.VMError = err.Error()
		} else {
			inspection.VM = &vm
		}
	}()
	if session.SSHDest != "" && session.Status != StatusDestroyed {
		if facts, err := f.guestFacts(ctx, session); err != nil {
			inspection.RepoError = err.Error()
		} else {
			inspection.Repository = &facts
		}
	}
	wg.Wait()
	return inspection, nil
}
