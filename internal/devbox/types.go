package devbox

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Status describes a persisted devbox lifecycle state.
type Status string

const (
	// StatusProvisioning means VM creation has started.
	StatusProvisioning Status = "provisioning"
	// StatusBootstrapping means the VM exists and guest setup is running.
	StatusBootstrapping Status = "bootstrapping"
	// StatusRunning means Pi is running the task.
	StatusRunning Status = "running"
	// StatusAwaitingReview means the task completed and awaits review.
	StatusAwaitingReview Status = "awaiting_review"
	// StatusFailed means creation, bootstrap, or task execution failed.
	StatusFailed Status = "failed"
	// StatusDestroying means teardown has started.
	StatusDestroying Status = "destroying"
	// StatusDestroyed means teardown completed.
	StatusDestroyed Status = "destroyed"
)

// PiEvidence summarizes verified evidence from a Pi session.
type PiEvidence struct {
	Provider          string `json:"provider"`
	Model             string `json:"model"`
	ThinkingLevel     string `json:"thinking_level"`
	AssistantMessages int    `json:"assistant_messages"`
	ReasoningTokens   int64  `json:"reasoning_tokens"`
	Output            string `json:"output"`
}

type piResult struct {
	SessionPath   string     `json:"session_path"`
	SessionSHA256 string     `json:"session_sha256"`
	OutputPath    string     `json:"output_path"`
	OutputSHA256  string     `json:"output_sha256"`
	Evidence      PiEvidence `json:"evidence"`
}

type taskResult struct {
	SessionSHA256 string     `json:"session_sha256"`
	OutputSHA256  string     `json:"output_sha256"`
	Evidence      PiEvidence `json:"evidence"`
}

// Session is the durable state of a devbox lifecycle.
type Session struct {
	ID              string     `json:"id"`
	Project         string     `json:"project"`
	Task            string     `json:"task"`
	BaseCommit      string     `json:"base_commit"`
	Branch          string     `json:"branch"`
	VMName          string     `json:"vm_name"`
	VMIdentity      string     `json:"vm_identity"`
	SSHDest         string     `json:"ssh_dest"`
	Workspace       string     `json:"workspace"`
	WorkerSHA256    string     `json:"worker_sha256,omitempty"`
	PiSessionPath   string     `json:"pi_session_path,omitempty"`
	PiSessionSHA256 string     `json:"pi_session_sha256,omitempty"`
	PiOutputPath    string     `json:"pi_output_path,omitempty"`
	PiOutputSHA256  string     `json:"pi_output_sha256,omitempty"`
	PiEvidence      PiEvidence `json:"pi_evidence,omitzero"`
	Status          Status     `json:"status"`
	Failure         string     `json:"failure,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	DestroyedAt     *time.Time `json:"destroyed_at,omitempty"`
}

var (
	sessionPattern = regexp.MustCompile(`^devbox_[0-9a-f]{32}$`)
	projectPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,98}[a-z0-9])?$`)
	commitPattern  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	dnsPattern     = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	sshDestPattern = regexp.MustCompile(`^(?:[a-zA-Z0-9_][a-zA-Z0-9_.+-]*@)?[a-zA-Z0-9](?:[a-zA-Z0-9.-]*[a-zA-Z0-9])?$`)
	branchPattern  = regexp.MustCompile(`^devbox/[a-z0-9._-]+-[0-9a-f]{32}$`)
)

func validateSessionID(id string) error {
	if !sessionPattern.MatchString(id) {
		return fmt.Errorf("invalid devbox session ID %q", id)
	}
	return nil
}

func validateProject(project string) error {
	if !projectPattern.MatchString(project) || strings.Contains(project, "..") {
		return fmt.Errorf("invalid lowercase project repository name %q", project)
	}
	return nil
}

func validateCommit(commit string) error {
	if !commitPattern.MatchString(commit) {
		return errors.New("base commit must be a full lowercase 40-hex commit")
	}
	return nil
}

func validateDNSName(name string) error {
	if !dnsPattern.MatchString(name) {
		return fmt.Errorf("invalid exe.dev DNS name %q", name)
	}
	return nil
}

func validateSSHDestination(destination string) error {
	if !sshDestPattern.MatchString(destination) {
		return fmt.Errorf("invalid SSH destination %q", destination)
	}
	return nil
}

func validateBranch(branch string) error {
	if !branchPattern.MatchString(branch) || strings.Contains(branch, "..") {
		return fmt.Errorf("invalid devbox branch %q", branch)
	}
	return nil
}

func newNames(project string) (id, branch, vm string, err error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", "", "", fmt.Errorf("generating session name: %w", err)
	}
	suffix := hex.EncodeToString(random[:])
	id = "devbox_" + suffix
	branch = "devbox/" + project + "-" + suffix
	prefix := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, project)
	prefix = strings.Trim(prefix, "-")
	const tail = "-" + "00000000000000000000000000000000"
	if len(prefix)+len(tail) > 63 {
		prefix = strings.TrimRight(prefix[:63-len(tail)], "-")
	}
	vm = prefix + "-" + suffix
	return id, branch, vm, validateDNSName(vm)
}

func validTransition(from, to Status) bool {
	switch from {
	case StatusProvisioning:
		return to == StatusBootstrapping || to == StatusFailed || to == StatusDestroying
	case StatusBootstrapping:
		return to == StatusRunning || to == StatusFailed || to == StatusDestroying
	case StatusRunning:
		return to == StatusAwaitingReview || to == StatusFailed || to == StatusDestroying
	case StatusAwaitingReview, StatusFailed:
		return to == StatusDestroying
	case StatusDestroying:
		return to == StatusDestroyed || to == StatusFailed
	default:
		return false
	}
}
