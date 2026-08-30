package devbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

const tokenEndpoint = "https://vitrier.int.exe.xyz/token"

type processSpec struct {
	Path      string
	Args, Env []string
}
type tokenMinter func(context.Context) (string, error)

func checkoutGitProcess(args []string, env []string) (processSpec, error) {
	return gitProcess(args, env, true)
}

func gitProcess(args []string, env []string, disableHooks bool) (processSpec, error) {
	for _, arg := range args {
		if arg == "-c" || strings.HasPrefix(arg, "-c") || arg == "--config-env" || strings.HasPrefix(arg, "--config-env=") {
			return processSpec{}, fmt.Errorf("git configuration override %q is not allowed", arg)
		}
	}
	clean := make([]string, 0, len(env))
	for _, value := range env {
		key, _, _ := strings.Cut(value, "=")
		if key == "GIT_CONFIG_COUNT" || strings.HasPrefix(key, "GIT_CONFIG_KEY_") || strings.HasPrefix(key, "GIT_CONFIG_VALUE_") || strings.HasPrefix(key, "GIT_AUTHOR_") || strings.HasPrefix(key, "GIT_COMMITTER_") {
			continue
		}
		clean = append(clean, value)
	}
	gitArgs := []string{"git", "-c", "user.name=vitrier[bot]", "-c", "user.email=320185307+vitrier[bot]@users.noreply.github.com", "-c", "credential.helper=", "-c", "credential.helper=!$HOME/.local/bin/vitrier-credential"}
	if disableHooks {
		gitArgs = append(gitArgs, "-c", "core.hooksPath=/dev/null")
	}
	gitArgs = append(gitArgs, args...)
	return processSpec{Path: "/usr/bin/git", Args: gitArgs, Env: clean}, nil
}

func ghProcess(ctx context.Context, args, env []string, mint tokenMinter) (processSpec, error) {
	if len(args) > 0 && slices.Contains([]string{"alias", "auth", "config", "extension"}, args[0]) {
		return processSpec{}, fmt.Errorf("gh %s is disabled for the secret-free wrapper", args[0])
	}
	if slices.Contains(args, "--show-token") {
		return processSpec{}, errors.New("gh command would disclose the in-memory token")
	}
	token, err := mint(ctx)
	if err != nil {
		return processSpec{}, fmt.Errorf("minting gh token: %w", err)
	}
	clean := make([]string, 0, len(env)+12)
	for _, value := range env {
		key, _, _ := strings.Cut(value, "=")
		if key == "PATH" || strings.HasPrefix(key, "GH_") || strings.HasPrefix(key, "GIT_") || strings.HasPrefix(key, "GITHUB_") || strings.HasPrefix(key, "DYLD_") || slices.Contains([]string{"BROWSER", "DEBUG", "LD_LIBRARY_PATH", "LD_PRELOAD", "NODE_OPTIONS", "PAGER", "PYTHONPATH", "PYTHONSTARTUP", "RUBYOPT", "SHELL"}, key) {
			continue
		}
		clean = append(clean, value)
	}
	clean = append(clean,
		"PATH=/usr/bin:/bin",
		"GH_TOKEN="+token,
		"GH_HOST=github.com",
		"GH_CONFIG_DIR=/etc/conseil/gh",
		"GH_PAGER=/usr/bin/cat",
		"PAGER=/usr/bin/cat",
		"GH_EDITOR=/usr/bin/false",
		"GIT_EDITOR=/usr/bin/false",
		"BROWSER=/usr/bin/false",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=core.hooksPath",
		"GIT_CONFIG_VALUE_0=/dev/null",
	)
	return processSpec{Path: "/usr/bin/gh", Args: append([]string{"gh"}, args...), Env: clean}, nil
}

func execProcess(spec processSpec) error {
	if err := syscall.Exec(spec.Path, spec.Args, spec.Env); err != nil {
		return fmt.Errorf("executing %s: %w", spec.Path, err)
	}
	return nil
}

func mintToken(ctx context.Context, client *http.Client) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, nil)
	if err != nil {
		return "", fmt.Errorf("creating token request: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("requesting token: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return "", fmt.Errorf("token endpoint returned %s", response.Status)
	}
	var body struct {
		Token string `json:"token"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&body); err != nil {
		return "", fmt.Errorf("decoding token response: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return "", errors.New("token response has trailing data")
	}
	if body.Token == "" {
		return "", errors.New("token endpoint returned an empty token")
	}
	return body.Token, nil
}

func credentialHelper(ctx context.Context, operation string, input io.Reader, output io.Writer, mint tokenMinter) error {
	if operation == "store" || operation == "erase" {
		return nil
	}
	if operation != "get" {
		return fmt.Errorf("unsupported credential operation %q", operation)
	}
	values := map[string]string{}
	scanner := bufio.NewScanner(io.LimitReader(input, 64<<10))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return errors.New("malformed credential request")
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading credential request: %w", err)
	}
	host := values["host"]
	if host == "" && values["url"] != "" {
		parsed, err := url.Parse(values["url"])
		if err == nil {
			host = parsed.Hostname()
		}
	}
	if host != "github.com" {
		return nil
	}
	token, err := mint(ctx)
	if err != nil {
		return fmt.Errorf("minting GitHub credential: %w", err)
	}
	if _, err := fmt.Fprintf(output, "username=x-access-token\npassword=%s\n\n", token); err != nil {
		return fmt.Errorf("writing credential response: %w", err)
	}
	return nil
}

func inspectGuestRepository(ctx context.Context, workspace, project, branch, base string) (RepositoryFacts, error) {
	if relative, ok := strings.CutPrefix(workspace, "$HOME/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return RepositoryFacts{}, fmt.Errorf("finding home: %w", err)
		}
		workspace = filepath.Join(home, relative)
	}
	if workspace == "" || branch == "" {
		return RepositoryFacts{}, errors.New("guest inspection has empty flags")
	}
	if err := validateProject(project); err != nil {
		return RepositoryFacts{}, err
	}
	if err := validateCommit(base); err != nil {
		return RepositoryFacts{}, err
	}
	run := func(args ...string) (string, error) {
		spec, err := gitProcess(append([]string{"-C", workspace}, args...), os.Environ(), false)
		if err != nil {
			return "", err
		}
		var stderr bytes.Buffer
		stdout, err := runOutput(ctx, osCommand, command{Name: spec.Path, Args: spec.Args[1:], Env: spec.Env, Stderr: &stderr})
		if err != nil {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
		return strings.TrimSpace(string(stdout)), nil
	}
	return inspectRepositoryWithGit(workspace, project, branch, base, run, os.Lstat)
}

func inspectRepositoryWithGit(workspace, project, branch, base string, run func(...string) (string, error), lstat func(string) (os.FileInfo, error)) (RepositoryFacts, error) {
	canonical := "https://x-access-token@github.com/maintenancewindows/" + project + ".git"
	fetchURLs, err := run("remote", "get-url", "--all", "origin")
	if err != nil {
		return RepositoryFacts{}, err
	}
	if err := requireCanonicalRemoteURLs("fetch", fetchURLs, canonical); err != nil {
		return RepositoryFacts{}, err
	}
	pushURLs, err := run("remote", "get-url", "--push", "--all", "origin")
	if err != nil {
		return RepositoryFacts{}, err
	}
	if err := requireCanonicalRemoteURLs("push", pushURLs, canonical); err != nil {
		return RepositoryFacts{}, err
	}
	status, err := run("status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return RepositoryFacts{}, err
	}
	head, err := run("rev-parse", "HEAD")
	if err != nil {
		return RepositoryFacts{}, err
	}
	currentBranch, err := run("branch", "--show-current")
	if err != nil {
		return RepositoryFacts{}, err
	}
	refs, err := run("for-each-ref", "--format=%(refname)", "refs")
	if err != nil {
		return RepositoryFacts{}, err
	}
	facts := RepositoryFacts{Clean: status == "", Head: head, Branch: currentBranch}
	expectedRefs := map[string]bool{
		"refs/heads/" + branch:          true,
		"refs/remotes/origin/" + branch: true,
	}
	for ref := range strings.SplitSeq(refs, "\n") {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		if name, ok := strings.CutPrefix(ref, "refs/heads/"); ok && name != branch {
			facts.OtherBranches = append(facts.OtherBranches, name)
		}
		if !expectedRefs[ref] {
			facts.UnexpectedRefs = append(facts.UnexpectedRefs, ref)
		}
		if ref == "refs/stash" {
			facts.Stashed = true
		}
	}
	for _, pseudoRef := range []string{"ORIG_HEAD", "MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "REBASE_HEAD", "BISECT_HEAD", "AUTO_MERGE"} {
		path, pathErr := run("rev-parse", "--git-path", pseudoRef)
		if pathErr != nil {
			return RepositoryFacts{}, pathErr
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(workspace, path)
		}
		if info, statErr := lstat(path); statErr == nil && (!info.Mode().IsRegular() || info.Size() > 0) {
			facts.UnexpectedRefs = append(facts.UnexpectedRefs, pseudoRef)
		} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return RepositoryFacts{}, fmt.Errorf("inspecting %s: %w", pseudoRef, statErr)
		}
	}
	fetchHead := false
	fetchHeadPath, err := run("rev-parse", "--git-path", "FETCH_HEAD")
	if err != nil {
		return RepositoryFacts{}, err
	}
	if !filepath.IsAbs(fetchHeadPath) {
		fetchHeadPath = filepath.Join(workspace, fetchHeadPath)
	}
	if info, statErr := lstat(fetchHeadPath); statErr == nil {
		if !info.Mode().IsRegular() {
			facts.UnexpectedRefs = append(facts.UnexpectedRefs, "FETCH_HEAD")
		} else {
			fetchHead = info.Size() > 0
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return RepositoryFacts{}, fmt.Errorf("inspecting FETCH_HEAD: %w", statErr)
	}

	remote, err := run("ls-remote", "--exit-code", canonical, "refs/heads/"+branch)
	if err == nil {
		fields := strings.Fields(remote)
		if len(fields) != 2 || fields[1] != "refs/heads/"+branch {
			return RepositoryFacts{}, errors.New("malformed git ls-remote output")
		}
		facts.RemotePresent = true
		facts.RemoteHead = fields[0]
	} else {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
			return RepositoryFacts{}, err
		}
	}
	allowed := []string{base}
	if facts.RemotePresent && facts.RemoteHead == facts.Head {
		allowed = append(allowed, facts.RemoteHead)
	}
	arguments := []string{"rev-list", "--all", "--reflog"}
	if fetchHead {
		arguments = append(arguments, "FETCH_HEAD")
	}
	arguments = append(arguments, "--not")
	arguments = append(arguments, allowed...)
	unpushed, err := run(arguments...)
	if err != nil {
		return RepositoryFacts{}, err
	}
	for commit := range strings.SplitSeq(unpushed, "\n") {
		commit = strings.TrimSpace(commit)
		if commit == "" {
			continue
		}
		if err := validateCommit(commit); err != nil {
			return RepositoryFacts{}, fmt.Errorf("invalid commit from refs or reflogs: %w", err)
		}
		facts.UnpushedCommits = append(facts.UnpushedCommits, commit)
	}
	return facts, nil
}

func requireCanonicalRemoteURLs(kind, output, canonical string) error {
	urls := strings.Fields(output)
	if len(urls) != 1 || urls[0] != canonical {
		return fmt.Errorf("origin %s URLs changed to %q, want only %q", kind, urls, canonical)
	}
	return nil
}
