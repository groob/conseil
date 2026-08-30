package devbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestGitAndGHHelpers(t *testing.T) {
	spec, err := gitProcess([]string{"status"}, []string{"PATH=/bin", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=store", "GIT_AUTHOR_NAME=attacker", "GIT_AUTHOR_EMAIL=attacker@example.com", "GIT_AUTHOR_DATE=tomorrow", "GIT_COMMITTER_NAME=attacker", "GIT_COMMITTER_EMAIL=attacker@example.com", "GIT_COMMITTER_DATE=tomorrow"}, false)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(spec.Args, "\x00")
	if strings.Contains(joined, "core.hooksPath") {
		t.Fatalf("ordinary Git command disables hooks: %q", spec.Args)
	}
	for _, want := range []string{"user.name=vitrier[bot]", "user.email=320185307+vitrier[bot]@users.noreply.github.com", "credential.helper=\x00", "credential.helper=!$HOME/.local/bin/vitrier-credential"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("git args %q lack %q", spec.Args, want)
		}
	}
	if containsString(spec.Env, "GIT_CONFIG_COUNT=1") || containsString(spec.Env, "GIT_CONFIG_VALUE_0=store") {
		t.Fatalf("git environment retained command-scope config: %q", spec.Env)
	}
	for _, value := range spec.Env {
		if strings.HasPrefix(value, "GIT_AUTHOR_") || strings.HasPrefix(value, "GIT_COMMITTER_") {
			t.Fatalf("git environment retained identity override: %q", spec.Env)
		}
	}
	for _, args := range [][]string{{"-c", "credential.helper=store", "status"}, {"--config-env=credential.helper=HELPER", "status"}} {
		if _, err := gitProcess(args, nil, false); err == nil {
			t.Fatalf("GitProcess(%q) accepted configuration override", args)
		}
	}
	gh, err := ghProcess(t.Context(), []string{"repo", "view"}, []string{"PATH=/tmp/evil", "GH_TOKEN=old", "GIT_SSH_COMMAND=/tmp/steal", "LD_PRELOAD=/tmp/steal.so"}, func(context.Context) (string, error) { return "secret", nil })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(gh.Args, " ") != "gh repo view" {
		t.Fatalf("gh args = %q", gh.Args)
	}
	if !containsString(gh.Env, "GH_TOKEN=secret") || !containsString(gh.Env, "GH_HOST=github.com") || !containsString(gh.Env, "GH_CONFIG_DIR=/etc/conseil/gh") || !containsString(gh.Env, "PATH=/usr/bin:/bin") || !containsString(gh.Env, "GIT_CONFIG_VALUE_0=/dev/null") {
		t.Fatalf("gh env = %q", gh.Env)
	}
	if containsString(gh.Env, "GIT_SSH_COMMAND=/tmp/steal") || containsString(gh.Env, "LD_PRELOAD=/tmp/steal.so") {
		t.Fatalf("gh environment retained executable injection: %q", gh.Env)
	}
	for _, args := range [][]string{{"auth", "token"}, {"auth", "status", "--show-token"}, {"auth", "status", "-t"}, {"alias", "set", "leak", "!env"}, {"extension", "exec", "leak"}} {
		if _, err := ghProcess(t.Context(), args, nil, func(context.Context) (string, error) { return "secret", nil }); err == nil {
			t.Fatalf("GHProcess(%q) accepted token disclosure", args)
		}
	}
}

func TestGitProcessForcesRealCommitIdentity(t *testing.T) {
	repository := t.TempDir()
	gitTestCommand(t, repository, "init")
	if err := os.WriteFile(filepath.Join(repository, "file"), []byte("content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTestCommand(t, repository, "add", "file")
	environment := append(os.Environ(),
		"GIT_AUTHOR_NAME=attacker",
		"GIT_AUTHOR_EMAIL=attacker@example.com",
		"GIT_COMMITTER_NAME=attacker",
		"GIT_COMMITTER_EMAIL=attacker@example.com",
	)
	spec, err := gitProcess([]string{"-C", repository, "commit", "-m", "test"}, environment, false)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(spec.Path)
	command.Args = spec.Args
	command.Env = spec.Env
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("commit: %v: %s", err, output)
	}
	identity := strings.TrimSpace(gitTestCommand(t, repository, "show", "-s", "--format=%an <%ae>%n%cn <%ce>", "HEAD"))
	want := "vitrier[bot] <320185307+vitrier[bot]@users.noreply.github.com>\nvitrier[bot] <320185307+vitrier[bot]@users.noreply.github.com>"
	if identity != want {
		t.Fatalf("identity = %q, want %q", identity, want)
	}
}

func TestCredentialHelperMintsOnlyGitHubGet(t *testing.T) {
	calls := 0
	mint := func(context.Context) (string, error) { calls++; return "token", nil }
	var output bytes.Buffer
	if err := credentialHelper(t.Context(), "get", strings.NewReader("protocol=https\nhost=example.com\n\n"), &output, mint); err != nil {
		t.Fatal(err)
	}
	if calls != 0 || output.Len() != 0 {
		t.Fatal("minted for another host")
	}
	if err := credentialHelper(t.Context(), "store", strings.NewReader("host=github.com\n"), &output, mint); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("minted for store")
	}
	if err := credentialHelper(t.Context(), "get", strings.NewReader("host=github.com\n\n"), &output, mint); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || output.String() != "username=x-access-token\npassword=token\n\n" {
		t.Fatalf("output %q calls %d", output.String(), calls)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestMintTokenUsesPOSTAndAllowsAdditiveFields(t *testing.T) {
	client := tokenTestClient(t, `{"token":"secret","expires_at":"later","future":{"value":true}}`, func(request *http.Request) {
		if request.Method != http.MethodPost {
			t.Errorf("method = %s", request.Method)
		}
	})
	token, err := mintToken(t.Context(), client)
	if err != nil {
		t.Fatal(err)
	}
	if token != "secret" {
		t.Fatalf("token = %q", token)
	}
}

func TestMintTokenRejectsMalformedTrailingData(t *testing.T) {
	for name, body := range map[string]string{
		"second_value": `{"token":"secret"} {"token":"other"}`,
		"junk":         `{"token":"secret"} trailing`,
	} {
		t.Run(name, func(t *testing.T) {
			client := tokenTestClient(t, body, nil)
			if _, err := mintToken(t.Context(), client); errString(err) != "token response has trailing data" {
				t.Errorf("mintToken() error = %q, want trailing-data error", errString(err))
			}
		})
	}
}

func tokenTestClient(t *testing.T, body string, check func(*http.Request)) *http.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if check != nil {
			check(request)
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	client := server.Client()
	transport := client.Transport
	client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		request = request.Clone(request.Context())
		request.URL.Scheme = "http"
		request.URL.Host = strings.TrimPrefix(server.URL, "http://")
		return transport.RoundTrip(request)
	})
	return client
}

func TestConfigurePiMergesAndUsesPrivateModes(t *testing.T) {
	home := t.TempDir()
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	extension := filepath.Join(t.TempDir(), "extension.js")
	if err := os.WriteFile(extension, []byte("extension"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".pi", "agent")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"unrelated":{"keep":true}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := configurePi(home, workspace, extension); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"exe-dev-llm-integration.json", "settings.json", "models.json", "trust.json"} {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %o", name, info.Mode().Perm())
		}
	}
	readObject := func(name string) map[string]any {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		var value map[string]any
		if err := json.Unmarshal(data, &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	settings := readObject("settings.json")
	if settings["defaultProvider"] != "exe-dev-openai" || settings["defaultModel"] != "gpt-5.6-sol@llm" || settings["defaultThinkingLevel"] != "high" {
		t.Fatalf("settings = %#v", settings)
	}
	if settings["unrelated"] == nil {
		t.Fatalf("unrelated key lost: %#v", settings)
	}
	integration := readObject("exe-dev-llm-integration.json")
	if integration["version"] != float64(1) || integration["useExeIntegration"] != true {
		t.Fatalf("integration = %#v", integration)
	}
	providers := readObject("models.json")["providers"].(map[string]any)
	override := providers["exe-dev-openai"].(map[string]any)["modelOverrides"].(map[string]any)["gpt-5.6-sol@llm"].(map[string]any)
	if override["reasoning"] != true {
		t.Fatalf("model override = %#v", override)
	}
}

func TestInspectGuestRepositoryPropagatesHomeError(t *testing.T) {
	t.Setenv("HOME", "")
	_, err := inspectGuestRepository(t.Context(), "$HOME/work", "counseil", "devbox/counseil-01234567890123456789012345678901", "0123456789012345678901234567890123456789")
	if err == nil {
		t.Fatal("inspectGuestRepository() error = nil, want home error")
	}
}

func TestInspectRepositoryRejectsChangedOrigin(t *testing.T) {
	base := "0123456789012345678901234567890123456789"
	branch := "devbox/counseil-01234567890123456789012345678901"
	calls := 0
	run := func(args ...string) (string, error) {
		calls++
		if strings.Join(args, " ") != "remote get-url --all origin" {
			t.Fatalf("unexpected Git call after changed origin: %q", args)
		}
		return "https://attacker.example/repository.git", nil
	}
	_, err := inspectRepositoryWithGit("/work", "counseil", branch, base, run, os.Lstat)
	want := `origin fetch URLs changed to ["https://attacker.example/repository.git"], want only "https://x-access-token@github.com/maintenancewindows/counseil.git"`
	if got := errString(err); got != want {
		t.Errorf("inspectRepositoryWithGit() error = %q, want %q", got, want)
	}
	if calls != 1 {
		t.Errorf("Git calls = %d, want 1", calls)
	}
}

func TestInspectRepositoryRejectsChangedPushURL(t *testing.T) {
	base := "0123456789012345678901234567890123456789"
	branch := "devbox/counseil-01234567890123456789012345678901"
	canonical := "https://x-access-token@github.com/maintenancewindows/counseil.git"
	run := func(args ...string) (string, error) {
		switch strings.Join(args, " ") {
		case "remote get-url --all origin":
			return canonical, nil
		case "remote get-url --push --all origin":
			return canonical + "\nhttps://attacker.example/repository.git", nil
		default:
			return "", errors.New("unexpected Git command")
		}
	}
	_, err := inspectRepositoryWithGit("/work", "counseil", branch, base, run, os.Lstat)
	want := `origin push URLs changed to ["https://x-access-token@github.com/maintenancewindows/counseil.git" "https://attacker.example/repository.git"], want only "https://x-access-token@github.com/maintenancewindows/counseil.git"`
	if got := errString(err); got != want {
		t.Errorf("inspectRepositoryWithGit() error = %q, want %q", got, want)
	}
}

func TestInspectRepositoryUsesCanonicalRemoteAndFindsHiddenWork(t *testing.T) {
	base := "0123456789012345678901234567890123456789"
	head := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hidden := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	branch := "devbox/counseil-01234567890123456789012345678901"
	canonical := "https://x-access-token@github.com/maintenancewindows/counseil.git"
	gitDir := t.TempDir()
	if err := os.Symlink("target", filepath.Join(gitDir, "MERGE_HEAD")); err != nil {
		t.Fatal(err)
	}
	var calls []string
	run := func(args ...string) (string, error) {
		call := strings.Join(args, " ")
		calls = append(calls, call)
		switch {
		case call == "remote get-url --all origin", call == "remote get-url --push --all origin":
			return canonical, nil
		case strings.HasPrefix(call, "status "):
			return "", nil
		case call == "rev-parse HEAD":
			return head, nil
		case call == "branch --show-current":
			return branch, nil
		case strings.HasPrefix(call, "for-each-ref "):
			return "refs/heads/" + branch + "\nrefs/remotes/origin/" + branch + "\nrefs/tags/unexpected", nil
		case strings.HasPrefix(call, "rev-parse --git-path "):
			return filepath.Join(gitDir, strings.TrimPrefix(call, "rev-parse --git-path ")), nil
		case call == "ls-remote --exit-code "+canonical+" refs/heads/"+branch:
			return head + "\trefs/heads/" + branch, nil
		case strings.HasPrefix(call, "rev-list --all --reflog --not "):
			return hidden, nil
		default:
			return "", errors.New("unexpected Git command: " + call)
		}
	}
	facts, err := inspectRepositoryWithGit("/work", "counseil", branch, base, run, os.Lstat)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(facts.UnexpectedRefs, "refs/tags/unexpected") || !containsString(facts.UnexpectedRefs, "MERGE_HEAD") || !containsString(facts.UnpushedCommits, hidden) {
		t.Errorf("inspectRepositoryWithGit() facts = %#v, want hidden refs and commit", facts)
	}
	for _, call := range calls {
		if strings.HasPrefix(call, "ls-remote") && strings.Contains(call, " origin ") {
			t.Fatalf("inspection queried worker-controlled origin: %q", call)
		}
	}
}

func TestInspectRepositoryFindsResetAwayCommitInRealReflog(t *testing.T) {
	repository, branch, base, hidden := makeInspectionRepository(t)
	gitTestCommand(t, repository, "reset", "--hard", base)
	if err := os.RemoveAll(filepath.Join(repository, ".git", "logs")); err != nil {
		t.Fatal(err)
	}
	fetchHead := hidden + "\tnot-for-merge\tbranch 'hidden' of canonical\n"
	if err := os.WriteFile(filepath.Join(repository, ".git", "FETCH_HEAD"), []byte(fetchHead), 0o600); err != nil {
		t.Fatal(err)
	}

	facts, err := inspectRepositoryWithGit(repository, "counseil", branch, base, inspectionGitRunner(t, repository, branch, ""), os.Lstat)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(facts.UnpushedCommits, hidden) {
		t.Fatalf("reset-away commit %s missing from facts: %#v", hidden, facts)
	}
}

func TestInspectRepositoryAcceptsFullyPushedCommitInRealRepository(t *testing.T) {
	repository, branch, base, head := makeInspectionRepository(t)
	facts, err := inspectRepositoryWithGit(repository, "counseil", branch, base, inspectionGitRunner(t, repository, branch, head), os.Lstat)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts.UnpushedCommits) != 0 || !facts.RemotePresent || facts.RemoteHead != head {
		t.Errorf("inspectRepositoryWithGit() facts = %#v, want pushed HEAD %q", facts, head)
	}
}

func makeInspectionRepository(t *testing.T) (repository, branch, base, head string) {
	t.Helper()
	repository = t.TempDir()
	gitTestCommand(t, repository, "init")
	gitTestCommand(t, repository, "config", "user.name", "test")
	gitTestCommand(t, repository, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repository, "file"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTestCommand(t, repository, "add", "file")
	gitTestCommand(t, repository, "commit", "-m", "base")
	base = strings.TrimSpace(gitTestCommand(t, repository, "rev-parse", "HEAD"))
	branch = "devbox/counseil-01234567890123456789012345678901"
	gitTestCommand(t, repository, "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(repository, "file"), []byte("work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTestCommand(t, repository, "commit", "-am", "work")
	head = strings.TrimSpace(gitTestCommand(t, repository, "rev-parse", "HEAD"))
	return repository, branch, base, head
}

func inspectionGitRunner(t *testing.T, repository, branch, remoteHead string) func(...string) (string, error) {
	t.Helper()
	canonical := "https://x-access-token@github.com/maintenancewindows/counseil.git"
	return func(args ...string) (string, error) {
		switch strings.Join(args, " ") {
		case "remote get-url --all origin", "remote get-url --push --all origin":
			return canonical, nil
		case "ls-remote --exit-code " + canonical + " refs/heads/" + branch:
			if remoteHead == "" {
				command := exec.Command("/bin/sh", "-c", "exit 2")
				return "", command.Run()
			}
			return remoteHead + "\trefs/heads/" + branch, nil
		}
		command := exec.Command("/usr/bin/git", append([]string{"-C", repository}, args...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, output)
		}
		return strings.TrimSpace(string(output)), nil
	}
}

func gitTestCommand(t *testing.T, repository string, args ...string) string {
	t.Helper()
	command := exec.Command("/usr/bin/git", append([]string{"-C", repository}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

func containsString(values []string, want string) bool {
	return slices.Contains(values, want)
}
