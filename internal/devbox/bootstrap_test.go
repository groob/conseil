package devbox

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWaitForVitrierRetriesTransientFailures(t *testing.T) {
	failures := []error{
		errors.New("token endpoint returned 403 Forbidden"),
		errors.New("requesting token: transient transport failure"),
	}
	attempts := 0
	err := waitForVitrier(t.Context(), func(context.Context) (string, error) {
		attempts++
		if len(failures) == 0 {
			return "readiness-token", nil
		}
		err := failures[0]
		failures = failures[1:]
		return "", err
	}, time.Minute, 10*time.Second, 0)
	if err != nil {
		t.Fatalf("waitForVitrier() returned error: %v", err)
	}
	if attempts != 3 {
		t.Errorf("waitForVitrier() attempts = %d, want 3", attempts)
	}
}

func TestWaitForVitrierRetriesAfterAttemptDeadline(t *testing.T) {
	attempts := 0
	err := waitForVitrier(t.Context(), func(ctx context.Context) (string, error) {
		attempts++
		if _, ok := ctx.Deadline(); !ok {
			t.Error("mint context has no deadline")
		}
		if attempts == 1 {
			return "", context.DeadlineExceeded
		}
		return "readiness-token", nil
	}, time.Minute, 5*time.Second, 0)
	if err != nil {
		t.Fatalf("waitForVitrier() returned error: %v", err)
	}
	if attempts != 2 {
		t.Errorf("waitForVitrier() attempts = %d, want 2", attempts)
	}
}

func TestWaitForVitrierHonorsCancellationAndDeadline(t *testing.T) {
	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		want := errors.New("not ready")
		err := waitForVitrier(ctx, func(context.Context) (string, error) {
			cancel()
			return "", want
		}, time.Minute, time.Second, 0)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("waitForVitrier() error = %v, want context.Canceled", err)
		}
	})

	t.Run("deadline", func(t *testing.T) {
		last := errors.New("not ready")
		err := waitForVitrier(t.Context(), func(context.Context) (string, error) {
			return "", last
		}, 0, time.Second, 0)
		if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, last) {
			t.Errorf("waitForVitrier() error = %v, want deadline and last error", err)
		}
	})
}

func TestProjectSetupUsesFixedSamovarContract(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	request := bootstrapRequest{workspace: workspace}
	got := projectSetupCommand(request, []string{"PATH=/bin"}, io.Discard)
	wantArgs := "run . setup --protocol 1 --workspace " + workspace
	if got.Name != "go" || got.Dir != filepath.Join(workspace, "devbox") || strings.Join(got.Args, " ") != wantArgs {
		t.Errorf("runProjectSetup() command = %#v, want go command in workspace", got)
	}
}

func TestExactCheckoutPinsRepositoryAndCommit(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "work")
	commit := "0123456789012345678901234567890123456789"
	var calls []string
	run := func(_ bool, args ...string) ([]byte, error) {
		call := strings.Join(args, " ")
		calls = append(calls, call)
		if strings.Contains(call, "remote get-url --all origin") || strings.Contains(call, "remote get-url --push --all origin") {
			return []byte("https://x-access-token@github.com/maintenancewindows/counseil.git\n"), nil
		}
		if strings.Contains(call, "rev-parse FETCH_HEAD^{commit}") || strings.Contains(call, "rev-parse HEAD") {
			return []byte(commit + "\n"), nil
		}
		return nil, nil
	}
	request := bootstrapRequest{project: "counseil", ref: commit, branch: "devbox/counseil-random", workspace: workspace}
	if err := exactCheckoutWithGit(request, run); err != nil {
		t.Fatalf("exactCheckout() returned error: %v", err)
	}
	all := strings.Join(calls, "\n")
	for _, want := range []string{
		"config --replace-all remote.origin.url https://x-access-token@github.com/maintenancewindows/counseil.git",
		"config --replace-all remote.origin.pushurl https://x-access-token@github.com/maintenancewindows/counseil.git",
		"fetch --no-tags --depth=1 origin " + commit,
		"checkout -B devbox/counseil-random " + commit,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("exactCheckout() calls lack %q:\n%s", want, all)
		}
	}
}

func TestExactCheckoutRefusesExistingDifferentCommit(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(filepath.Join(workspace, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	base := "0123456789012345678901234567890123456789"
	run := func(_ bool, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "status --porcelain") {
			return nil, nil
		}
		if strings.Contains(joined, "rev-parse HEAD") {
			return []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"), nil
		}
		return nil, errors.New("unexpected mutation")
	}
	request := bootstrapRequest{project: "counseil", ref: base, branch: "devbox/counseil-01234567890123456789012345678901", workspace: workspace}
	if err := exactCheckoutWithGit(request, run); err == nil {
		t.Error("exactCheckout() error = nil, want refusal")
	}
}
