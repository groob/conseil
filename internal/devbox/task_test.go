package devbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunTaskUsesPlainPiStdinReadyEnvironmentAndKnownSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := t.TempDir()
	task := "fix quotes ' and shell text $(touch /tmp/nope)\nwithout changing it"
	if err := os.WriteFile(filepath.Join(home, "events.jsonl"), readTaskFixture(t, "pi-events-success.jsonl"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "session.jsonl"), readTaskFixture(t, "pi-session-success.jsonl"), 0o600); err != nil {
		t.Fatal(err)
	}
	installPiShim(t, home, `
printf '%s\n' "$*" > "$HOME/pi-args"
cat > "$HOME/pi-stdin"
printf '%s\n%s\n' "$DATABASE_URL" "$PROJECT_READY" > "$HOME/pi-environment"
mkdir -p "$PI_CODING_AGENT_SESSION_DIR/--workspace--"
cp "$HOME/session.jsonl" "$PI_CODING_AGENT_SESSION_DIR/--workspace--/task.jsonl"
cat "$HOME/events.jsonl"
`)
	request, err := json.Marshal(taskRequest{Task: task, Environment: map[string]string{"DATABASE_URL": "postgres://test", "PROJECT_READY": "yes"}})
	if err != nil {
		t.Fatal(err)
	}
	var result bytes.Buffer
	if err := runTask(t.Context(), "devbox_01234567890123456789012345678901", workspace, bytes.NewReader(request), io.Discard, &result); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(filepath.Join(home, "pi-args"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(args)), "--mode json"; got != want {
		t.Errorf("pi arguments = %q, want %q", got, want)
	}
	prompt, err := os.ReadFile(filepath.Join(home, "pi-stdin"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(prompt); got != task {
		t.Errorf("pi stdin = %q, want %q", got, task)
	}
	environment, err := os.ReadFile(filepath.Join(home, "pi-environment"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(environment), "postgres://test\nyes\n"; got != want {
		t.Errorf("pi environment = %q, want %q", got, want)
	}
	var decoded taskResult
	if err := json.NewDecoder(&result).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Evidence.AssistantMessages != 2 || decoded.Evidence.ReasoningTokens != 16 || decoded.Evidence.Output != "done" {
		t.Errorf("runTask() evidence = %#v", decoded.Evidence)
	}
	if !validSHA256(decoded.SessionSHA256) || !validSHA256(decoded.OutputSHA256) {
		t.Errorf("runTask() hashes = %#v", decoded)
	}
	root := filepath.Join(home, ".local", "share", "conseil", "pi", "devbox_01234567890123456789012345678901")
	for _, path := range []string{filepath.Join(root, taskSessionArtifact), filepath.Join(root, taskOutputArtifact)} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
			t.Errorf("%s mode = %o, want %o", path, got, want)
		}
	}
}

func TestRunTaskRejectsSymlinkedArtifactDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	marker := filepath.Join(home, "pi-ran")
	installPiShim(t, home, `touch "$HOME/pi-ran"`)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(home, ".local")); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(home, "work")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	request, err := json.Marshal(taskRequest{Task: "task"})
	if err != nil {
		t.Fatal(err)
	}
	err = runTask(t.Context(), "devbox_01234567890123456789012345678901", workspace, bytes.NewReader(request), io.Discard, io.Discard)
	if err == nil {
		t.Fatal("runTask() error = nil, want symlink rejection")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("pi marker stat error = %v, want not exist", err)
	}
}

func installPiShim(t *testing.T, home, body string) {
	t.Helper()
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(bin, "pi")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestStreamTaskArtifactUsesFixedRegularFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	session := "devbox_01234567890123456789012345678901"
	root := filepath.Join(home, ".local", "share", "conseil", "pi", session)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, taskOutputArtifact)
	if err := os.WriteFile(path, []byte("evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := streamTaskArtifact(session, taskOutputArtifact, &output); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "evidence\n"; got != want {
		t.Errorf("streamTaskArtifact() output = %q, want %q", got, want)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", path); err != nil {
		t.Fatal(err)
	}
	if err := streamTaskArtifact(session, taskOutputArtifact, io.Discard); err == nil {
		t.Fatal("streamed symlink artifact")
	}
}

func TestVerifyPiOutputRequiresSettledLifecycleAndCanonicalizes(t *testing.T) {
	for name, contents := range map[string]string{
		"malformed":          `{"type":`,
		"missing_settled":    "{\"type\":\"agent_end\"}\n",
		"settled_before_end": "{\"type\":\"agent_settled\"}\n",
		"after_settled":      "{\"type\":\"agent_end\"}\n{\"type\":\"agent_settled\"}\n{\"type\":\"message_end\"}\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "events.jsonl")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := verifyPiOutput(path); err == nil {
				t.Fatal("invalid Pi output was accepted")
			}
		})
	}

	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(path, readTaskFixture(t, "pi-events-success.jsonl"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyPiOutput(path); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(contents)), "\n")
	if got, want := lines[len(lines)-1], `{"type":"agent_settled"}`; got != want {
		t.Errorf("final event = %s, want %s", got, want)
	}
}

func TestDecodeTaskRequestAllowsEmptyEnvironmentAndRejectsReservedNames(t *testing.T) {
	request, err := decodeTaskRequest(strings.NewReader(`{"task":"test","environment":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Environment) != 0 {
		t.Errorf("decodeTaskRequest() environment = %#v, want empty", request.Environment)
	}
	for _, name := range []string{"PATH", "PI_MODEL", "GIT_CONFIG_COUNT", "https_proxy", "NODE_OPTIONS"} {
		input := fmt.Sprintf(`{"task":"test","environment":{%q:"value"}}`, name)
		_, err := decodeTaskRequest(strings.NewReader(input))
		want := fmt.Sprintf("setup ready environment: contains reserved variable %q", name)
		if got := errString(err); got != want {
			t.Errorf("decodeTaskRequest() error = %q, want %q", got, want)
		}
	}
}

func TestDecodeTaskRequestRejectsOversizeInput(t *testing.T) {
	request := strings.Repeat(" ", maxTaskRequest+1)
	if _, err := decodeTaskRequest(strings.NewReader(request)); errString(err) != "task request exceeds its size limit" {
		t.Errorf("decodeTaskRequest() error = %q, want size-limit error", errString(err))
	}
}

func TestVerifyPiSessionRejectsWrongModelThinkingOrTerminalOutcome(t *testing.T) {
	tests := map[string]string{
		"wrong_model":      strings.Replace(validPiSession(), `"model":"gpt-5.6-sol@llm"`, `"model":"other"`, 1),
		"thinking_off":     strings.Replace(validPiSession(), `"thinkingLevel":"high"`, `"thinkingLevel":"off"`, 1),
		"no_reasoning":     strings.ReplaceAll(strings.ReplaceAll(validPiSession(), `"reasoning":7`, `"reasoning":0`), `"reasoning":9`, `"reasoning":0`),
		"terminal_error":   strings.Replace(validPiSession(), `"stopReason":"stop"`, `"stopReason":"error"`, 1),
		"terminal_aborted": strings.Replace(validPiSession(), `"stopReason":"stop"`, `"stopReason":"aborted"`, 1),
		"terminal_length":  strings.Replace(validPiSession(), `"stopReason":"stop"`, `"stopReason":"length"`, 1),
	}
	for name, contents := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.jsonl")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := verifyPiSession(path); err == nil {
				t.Fatal("invalid Pi session was accepted")
			}
		})
	}
}

func validPiSession() string {
	return strings.Join([]string{
		`{"type":"session","version":3,"id":"session","cwd":"/work"}`,
		`{"type":"model_change","provider":"exe-dev-openai","modelId":"gpt-5.6-sol@llm"}`,
		`{"type":"thinking_level_change","thinkingLevel":"high"}`,
		`{"type":"message","message":{"role":"user","content":"task"}}`,
		`{"type":"message","message":{"role":"assistant","provider":"exe-dev-openai","model":"gpt-5.6-sol@llm","content":[{"type":"toolCall","id":"call","name":"read","arguments":{}}],"stopReason":"toolUse","usage":{"reasoning":7}}}`,
		`{"type":"message","message":{"role":"assistant","provider":"exe-dev-openai","model":"gpt-5.6-sol@llm","content":[{"type":"text","text":"done"}],"stopReason":"stop","usage":{"reasoning":9}}}`,
	}, "\n") + "\n"
}

func readTaskFixture(t *testing.T, name string) []byte {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return contents
}
