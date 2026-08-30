package devbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type bootstrapRequest struct {
	session   string
	project   string
	ref       string
	branch    string
	workspace string
}

const (
	vitrierReadinessTimeout        = 2 * time.Minute
	vitrierReadinessAttemptTimeout = 10 * time.Second
	vitrierReadinessPollInterval   = 2 * time.Second
)

func bootstrap(ctx context.Context, request bootstrapRequest, stdout, stderr io.Writer) error {
	if err := validateProject(request.project); err != nil {
		return err
	}
	if err := validateCommit(request.ref); err != nil {
		return err
	}
	if err := validateSessionID(request.session); err != nil {
		return err
	}
	if request.workspace == "" {
		return errors.New("bootstrap has empty workspace")
	}
	if err := validateBranch(request.branch); err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("finding home: %w", err)
	}
	if after, ok := strings.CutPrefix(request.workspace, "$HOME/"); ok {
		request.workspace = filepath.Join(home, after)
	}
	request.workspace, err = filepath.Abs(request.workspace)
	if err != nil {
		return fmt.Errorf("resolving workspace: %w", err)
	}
	binary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding devbox binary: %w", err)
	}
	binDir := filepath.Join(home, ".local", "bin")
	if err := installPrerequisites(ctx, stderr); err != nil {
		return err
	}
	if err := installHelpers(binDir, binary); err != nil {
		return err
	}
	mint := func(ctx context.Context) (string, error) { return mintToken(ctx, http.DefaultClient) }
	if err := waitForVitrier(ctx, mint, vitrierReadinessTimeout, vitrierReadinessAttemptTimeout, vitrierReadinessPollInterval); err != nil {
		return err
	}
	env := prependPath(os.Environ(), binDir)
	if err := exactCheckout(ctx, request, env, stderr); err != nil {
		return err
	}
	if err := runProjectSetup(ctx, request, env, stdout, stderr); err != nil {
		return err
	}
	return configurePi(home, request.workspace, "")
}

// waitForVitrier polls mint until it returns a token or readyCtx's timeout
// elapses. deadline distinguishes ctx's own cancellation (returned as-is)
// from readyCtx's timeout (wrapped with the most recent minting error), and
// is checked at every point the loop can exit.
func waitForVitrier(ctx context.Context, mint tokenMinter, timeout, attemptTimeout, interval time.Duration) error {
	readyCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var last error
	deadline := func() error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if readyCtx.Err() != nil {
			return vitrierReadinessDeadlineError(last)
		}
		return nil
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		attemptCtx, cancelAttempt := context.WithTimeout(readyCtx, attemptTimeout)
		token, err := mint(attemptCtx)
		attemptErr := attemptCtx.Err()
		cancelAttempt()
		if err == nil && attemptErr != nil {
			err = attemptErr
		}
		if err == nil && token != "" {
			if err := deadline(); err != nil {
				return err
			}
			return nil
		}
		if err == nil {
			err = errors.New("vitrier returned an empty token")
		}
		if ctx.Err() == nil && readyCtx.Err() == nil {
			last = err
		} else if last == nil {
			last = err
		}
		if err := deadline(); err != nil {
			return err
		}
		timer := time.NewTimer(interval)
		select {
		case <-readyCtx.Done():
			timer.Stop()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return vitrierReadinessDeadlineError(last)
		case <-timer.C:
		}
	}
}

func vitrierReadinessDeadlineError(last error) error {
	if last == nil {
		return fmt.Errorf("waiting for Vitrier readiness: %w", context.DeadlineExceeded)
	}
	return fmt.Errorf("waiting for Vitrier readiness: %w (last error: %w)", context.DeadlineExceeded, last)
}

func installPrerequisites(ctx context.Context, logs io.Writer) error {
	commands := []command{
		{Name: "sudo", Args: []string{"apt-get", "update"}, Stdout: logs, Stderr: logs},
		{Name: "sudo", Args: []string{"env", "DEBIAN_FRONTEND=noninteractive", "apt-get", "install", "--yes", "ca-certificates", "curl", "git", "gh"}, Stdout: logs, Stderr: logs},
		{Name: "sudo", Args: []string{"install", "-d", "-o", "root", "-g", "root", "-m", "0555", "/etc/conseil/gh"}, Stdout: logs, Stderr: logs},
		{Name: "exeuntu", Args: []string{"update", "pi"}, Stdout: logs, Stderr: logs},
	}
	for _, command := range commands {
		if err := osCommand(ctx, command); err != nil {
			return fmt.Errorf("installing generic prerequisite with %s: %w", command.Name, err)
		}
	}
	return nil
}

func installHelpers(binDir, binary string) error {
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		return fmt.Errorf("creating helper directory: %w", err)
	}
	for _, name := range []string{"git", "gh", "vitrier-credential"} {
		path := filepath.Join(binDir, name)
		if target, err := os.Readlink(path); err == nil && target == binary {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("replacing %s helper: %w", name, err)
		}
		if err := os.Symlink(binary, path); err != nil {
			return fmt.Errorf("installing %s helper: %w", name, err)
		}
	}
	return nil
}

func prependPath(env []string, dir string) []string {
	result := make([]string, 0, len(env)+1)
	found := false
	for _, value := range env {
		if after, ok := strings.CutPrefix(value, "PATH="); ok {
			result = append(result, "PATH="+dir+":"+after)
			found = true
		} else {
			result = append(result, value)
		}
	}
	if !found {
		result = append(result, "PATH="+dir+":/usr/local/bin:/usr/bin:/bin")
	}
	return result
}

func exactCheckout(ctx context.Context, request bootstrapRequest, env []string, stderr io.Writer) error {
	runGit := func(output bool, args ...string) ([]byte, error) {
		spec, err := checkoutGitProcess(append([]string{"-C", request.workspace}, args...), env)
		if err != nil {
			return nil, err
		}
		command := command{Name: spec.Path, Args: spec.Args[1:], Env: spec.Env, Stdout: stderr, Stderr: stderr}
		if output {
			return runOutput(ctx, osCommand, command)
		}
		return nil, osCommand(ctx, command)
	}
	return exactCheckoutWithGit(request, runGit)
}

func exactCheckoutWithGit(request bootstrapRequest, runGit func(bool, ...string) ([]byte, error)) error {
	workspace := request.workspace
	if _, err := os.Stat(filepath.Join(workspace, ".git")); err == nil {
		output, runErr := runGit(true, "status", "--porcelain", "--untracked-files=all")
		if runErr != nil {
			return fmt.Errorf("inspecting existing workspace: %w", runErr)
		}
		if len(bytes.TrimSpace(output)) > 0 {
			return errors.New("refusing to overwrite dirty existing work")
		}
		head, runErr := runGit(true, "rev-parse", "HEAD")
		if runErr != nil {
			return fmt.Errorf("reading existing workspace HEAD: %w", runErr)
		}
		if got := strings.TrimSpace(string(head)); got != request.ref {
			return fmt.Errorf("refusing to replace existing commit %q with base %q", got, request.ref)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspecting workspace: %w", err)
	} else if entries, readErr := os.ReadDir(workspace); readErr == nil && len(entries) > 0 {
		return errors.New("refusing to overwrite nonempty existing work")
	} else if readErr != nil && !os.IsNotExist(readErr) {
		return fmt.Errorf("reading existing workspace: %w", readErr)
	}
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return fmt.Errorf("creating workspace: %w", err)
	}
	remote := "https://x-access-token@github.com/maintenancewindows/" + request.project + ".git"
	if _, err := runGit(false, "init"); err != nil {
		return fmt.Errorf("initializing exact checkout: %w", err)
	}
	if _, err := runGit(false, "config", "--replace-all", "remote.origin.url", remote); err != nil {
		return fmt.Errorf("setting exact checkout remote: %w", err)
	}
	if _, err := runGit(false, "config", "--replace-all", "remote.origin.pushurl", remote); err != nil {
		return fmt.Errorf("setting exact checkout push URL: %w", err)
	}
	fetchURLs, err := runGit(true, "remote", "get-url", "--all", "origin")
	if err != nil {
		return fmt.Errorf("verifying exact checkout fetch URL: %w", err)
	}
	if err := requireCanonicalRemoteURLs("fetch", string(fetchURLs), remote); err != nil {
		return fmt.Errorf("verifying exact checkout remote: %w", err)
	}
	pushURLs, err := runGit(true, "remote", "get-url", "--push", "--all", "origin")
	if err != nil {
		return fmt.Errorf("verifying exact checkout push URL: %w", err)
	}
	if err := requireCanonicalRemoteURLs("push", string(pushURLs), remote); err != nil {
		return fmt.Errorf("verifying exact checkout remote: %w", err)
	}
	if _, err := runGit(false, "fetch", "--no-tags", "--depth=1", "origin", request.ref); err != nil {
		return fmt.Errorf("fetching exact commit: %w", err)
	}
	resolved, err := runGit(true, "rev-parse", "FETCH_HEAD^{commit}")
	if err != nil {
		return fmt.Errorf("resolving fetched commit: %w", err)
	}
	if got := strings.TrimSpace(string(resolved)); got != request.ref {
		return fmt.Errorf("fetched commit %q, want %q", got, request.ref)
	}
	if _, err := runGit(false, "checkout", "-B", request.branch, request.ref); err != nil {
		return fmt.Errorf("checking out exact commit: %w", err)
	}
	head, err := runGit(true, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("verifying checked out HEAD: %w", err)
	}
	if got := strings.TrimSpace(string(head)); got != request.ref {
		return fmt.Errorf("checked out HEAD %q, want %q", got, request.ref)
	}
	status, err := runGit(true, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return fmt.Errorf("verifying clean checkout: %w", err)
	}
	if len(bytes.TrimSpace(status)) != 0 {
		return errors.New("initial checkout is not clean")
	}
	return nil
}

func runProjectSetup(ctx context.Context, request bootstrapRequest, env []string, stdout, stderr io.Writer) error {
	command := projectSetupCommand(request, env, stderr)
	if err := runProjectSetupProtocol(ctx, osCommand, command, func(_ projectEvent, canonical json.RawMessage) error {
		_, writeErr := stdout.Write(append(canonical, '\n'))
		return writeErr
	}); err != nil {
		return fmt.Errorf("running project setup: %w", err)
	}
	return nil
}

func projectSetupCommand(request bootstrapRequest, env []string, stderr io.Writer) command {
	return command{Name: "go", Args: []string{"run", ".", "setup", "--protocol", "1", "--workspace", request.workspace}, Dir: filepath.Join(request.workspace, "devbox"), Env: env, Stderr: stderr}
}

func configurePi(home, workspace, extension string) error {
	localSettings := filepath.Join(workspace, ".pi", "settings.json")
	if _, err := os.Stat(localSettings); err == nil {
		return fmt.Errorf("workspace-local Pi settings at %s would override Conseil settings", localSettings)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("checking workspace Pi settings: %w", err)
	}
	if extension == "" {
		extension = filepath.Join(home, ".pi", "agent", "extensions", "exe-dev", "index.ts")
	}
	if info, err := os.Stat(extension); err != nil || info.IsDir() {
		return fmt.Errorf("bundled exe.dev Pi extension is missing at %s", extension)
	}
	dir := filepath.Join(home, ".pi", "agent")
	files := map[string]map[string]any{
		"exe-dev-llm-integration.json": {
			"version":           1,
			"useExeIntegration": true,
		},
		"settings.json": {
			"defaultProvider":      piProvider,
			"defaultModel":         piModel,
			"defaultThinkingLevel": piThinking,
			"modelThinkingLevels": map[string]any{
				piProvider + "/" + piModel: piThinking,
			},
		},
		"models.json": {
			"providers": map[string]any{
				piProvider: map[string]any{
					"modelOverrides": map[string]any{
						piModel: map[string]any{"reasoning": true},
					},
				},
			},
		},
		"trust.json": {workspace: true},
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating Pi configuration directory: %w", err)
	}
	for name, wanted := range files {
		if err := mergeJSONFile(filepath.Join(dir, name), wanted); err != nil {
			return fmt.Errorf("writing Pi %s: %w", name, err)
		}
	}
	return nil
}

func mergeJSONFile(path string, wanted map[string]any) error {
	existing := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &existing); err != nil {
			return fmt.Errorf("decoding existing JSON: %w", err)
		}
		if existing == nil {
			return errors.New("existing JSON must be an object")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	mergeMaps(existing, wanted)
	data, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(temporary, 0o600); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

func mergeMaps(target, wanted map[string]any) {
	for key, value := range wanted {
		if child, ok := value.(map[string]any); ok {
			current, _ := target[key].(map[string]any)
			if current == nil {
				current = map[string]any{}
			}
			mergeMaps(current, child)
			target[key] = current
		} else {
			target[key] = value
		}
	}
}
