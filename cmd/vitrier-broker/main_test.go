package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNames(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"samovar-dev", "a", "vm123"} {
		if !validVMName(name) {
			t.Errorf("validVMName(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", "-vm", "vm-", "UPPER", "vm.example", string(make([]byte, 64))} {
		if validVMName(name) {
			t.Errorf("validVMName(%q) = true, want false", name)
		}
	}
	for _, name := range []string{"samovar", ".github", "repo_name-2"} {
		if !validRepositoryName(name) {
			t.Errorf("validRepositoryName(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", ".", "..", "owner/repo", "has space", string(make([]byte, 101))} {
		if validRepositoryName(name) {
			t.Errorf("validRepositoryName(%q) = true, want false", name)
		}
	}
}

func TestLoadConfigUsesSystemdCredential(t *testing.T) {
	t.Setenv("VITRIER_APP_ID", "4691351")
	t.Setenv("CREDENTIALS_DIRECTORY", "/run/credentials/vitrier-broker.service")
	t.Setenv("VITRIER_PRIVATE_KEY_FILE", "/ignored.pem")

	got, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() returned unexpected error: %v", err)
	}
	want := config{
		appID:          4691351,
		privateKeyPath: "/run/credentials/vitrier-broker.service/github-app-key",
	}
	if got != want {
		t.Errorf("loadConfig() = %+v, want %+v", got, want)
	}
}

func TestLoadConfigRequiresSystemdCredential(t *testing.T) {
	t.Setenv("VITRIER_APP_ID", "4691351")
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	t.Setenv("VITRIER_PRIVATE_KEY_FILE", "/ignored.pem")

	if _, err := loadConfig(); err == nil {
		t.Error("loadConfig() returned nil error without CREDENTIALS_DIRECTORY, want non-nil")
	}
}

func TestRunAdmin(t *testing.T) {
	requireRoot(t)
	dir := filepath.Join(t.TempDir(), "grants")
	store := testGrantStore(dir)
	var output bytes.Buffer

	if err := runAdmin([]string{"grant", "samovar-dev", "samovar"}, store, &output); err != nil {
		t.Fatalf("runAdmin(grant) returned unexpected error: %v", err)
	}
	if got, want := output.String(), "created\n"; got != want {
		t.Errorf("runAdmin(grant) output = %q, want %q", got, want)
	}
	output.Reset()
	if err := runAdmin([]string{"grant", "samovar-dev", "samovar"}, store, &output); err != nil {
		t.Fatalf("runAdmin(idempotent grant) returned unexpected error: %v", err)
	}
	if got, want := output.String(), "existing\n"; got != want {
		t.Errorf("runAdmin(idempotent grant) output = %q, want %q", got, want)
	}
	if err := runAdmin([]string{"grant", "samovar-dev", "conseil"}, store, &output); err == nil {
		t.Error("runAdmin(conflicting grant) returned nil error, want non-nil")
	}
	if err := runAdmin([]string{"check", "samovar-dev", "samovar"}, store, &output); err != nil {
		t.Fatalf("runAdmin(check) returned unexpected error: %v", err)
	}
	if err := runAdmin([]string{"check", "samovar-dev", "conseil"}, store, &output); err == nil {
		t.Error("runAdmin(check mismatch) returned nil error, want non-nil")
	}
	if err := runAdmin([]string{"revoke", "samovar-dev"}, store, &output); err != nil {
		t.Fatalf("runAdmin(revoke) returned unexpected error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "samovar-dev")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("os.Stat(grant after revoke) error = %v, want os.ErrNotExist", err)
	}
}

func TestRunAdminRejectsNonRootMutation(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires a non-root test process")
	}
	t.Parallel()

	store := testGrantStore(filepath.Join(t.TempDir(), "grants"))
	for _, args := range [][]string{{"grant", "samovar-dev", "samovar"}, {"revoke", "samovar-dev"}, {"check", "samovar-dev", "samovar"}} {
		if err := runAdmin(args, store, &bytes.Buffer{}); err == nil {
			t.Errorf("runAdmin(%q, non-root) returned nil error, want non-nil", args)
		}
	}
	if _, err := os.Stat(store.dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("os.Stat(grants directory) error = %v, want os.ErrNotExist", err)
	}
}

func TestRunAdminRejectsInvalidArguments(t *testing.T) {
	t.Parallel()

	store := testGrantStore(filepath.Join(t.TempDir(), "grants"))
	for _, args := range [][]string{nil, {"grant"}, {"revoke"}, {"check"}, {"check", "vm"}, {"unknown"}} {
		if err := runAdmin(args, store, &bytes.Buffer{}); !errors.Is(err, errAdminUsage) {
			t.Errorf("runAdmin(%q) error = %v, want errAdminUsage", args, err)
		}
	}
}

func TestVitrierIntegrationState(t *testing.T) {
	const valid = `"name":"vitrier","type":"http-proxy","target":"https://groob-tools.exe.xyz/","peer":true`
	tests := []struct {
		name      string
		json      string
		want      string
		wantError bool
	}{
		{
			name: "attached",
			json: `[{` + valid + `,"attachments":["auto:all","vm:samovar-dev"]}]`,
			want: "attached",
		},
		{
			name: "detached",
			json: `[{` + valid + `,"attachments":["vm:conseil-dev"]}]`,
			want: "detached",
		},
		{
			name: "missing_with_malformed_unrelated_items",
			json: `[null,7,{"name":"llm"},{"broken":true}]`,
			want: "missing",
		},
		{
			name:      "wrong_type",
			json:      `[{"name":"vitrier","type":"llm","target":"https://groob-tools.exe.xyz/","peer":true,"attachments":[]}]`,
			wantError: true,
		},
		{
			name:      "wrong_target",
			json:      `[{"name":"vitrier","type":"http-proxy","target":"https://other.exe.xyz/","peer":true,"attachments":[]}]`,
			wantError: true,
		},
		{
			name:      "not_peer",
			json:      `[{"name":"vitrier","type":"http-proxy","target":"https://groob-tools.exe.xyz/","peer":false,"attachments":[]}]`,
			wantError: true,
		},
		{
			name:      "invalid_attachments",
			json:      `[{` + valid + `,"attachments":"vm:samovar-dev"}]`,
			wantError: true,
		},
		{
			name:      "duplicate",
			json:      `[{` + valid + `,"attachments":[]},{` + valid + `,"attachments":[]}]`,
			wantError: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			script := filepath.Join("..", "..", "deploy", "vitrier-integration-state.py")
			command := exec.CommandContext(t.Context(), "python3", script, "vm:samovar-dev")
			command.Stdin = strings.NewReader(test.json)
			output, err := command.CombinedOutput()
			if gotError := err != nil; gotError != test.wantError {
				t.Fatalf("integration state for %s error = %v, output = %q, want error presence = %t", test.name, err, output, test.wantError)
			}
			if err != nil {
				return
			}
			if got := strings.TrimSpace(string(output)); got != test.want {
				t.Errorf("integration state for %s = %q, want %q", test.name, got, test.want)
			}
		})
	}
}

func TestTransactionRejectsInvalidInvocation(t *testing.T) {
	script := filepath.Join("..", "..", "deploy", "vitrier-broker-transaction-v1.sh")
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "invalid_id", args: []string{"prepare", "1234"}, want: "128-bit lowercase hexadecimal"},
		{name: "missing_put_argument", args: []string{"put", strings.Repeat("a", 32)}, want: "usage:"},
		{name: "unknown_operation", args: []string{"unknown", strings.Repeat("a", 32)}, want: "usage:"},
		{name: "status_argument", args: []string{"status", "unexpected"}, want: "usage:"},
	} {
		t.Run(test.name, func(t *testing.T) {
			output, err := exec.CommandContext(t.Context(), "bash", append([]string{script}, test.args...)...).CombinedOutput()
			if err == nil {
				t.Fatalf("transaction invocation succeeded, output = %q", output)
			}
			if !strings.Contains(string(output), test.want) {
				t.Errorf("transaction output = %q, want substring %q", output, test.want)
			}
		})
	}
}

func TestDeploymentRejectsInvalidInputs(t *testing.T) {
	script := filepath.Join("..", "..", "deploy", "vitrier-broker.sh")
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing_arguments", want: "usage:"},
		{name: "missing_repository", args: []string{"conseil-dev"}, want: "usage:"},
		{name: "invalid_vm", args: []string{"INVALID", "conseil"}, want: "invalid name"},
		{name: "empty_repository", args: []string{"conseil-dev", ""}, want: "invalid GitHub repository name"},
		{name: "repository_owner", args: []string{"conseil-dev", "maintenancewindows/conseil"}, want: "invalid GitHub repository name"},
		{name: "dot_repository", args: []string{"conseil-dev", "."}, want: "invalid GitHub repository name"},
		{name: "oversized_repository", args: []string{"conseil-dev", strings.Repeat("a", 101)}, want: "invalid GitHub repository name"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command := exec.CommandContext(t.Context(), "bash", append([]string{script}, test.args...)...)
			command.Env = append(os.Environ(), "VITRIER_PRIVATE_KEY_FILE="+filepath.Join(t.TempDir(), "missing.pem"))
			output, err := command.CombinedOutput()
			var exitError *exec.ExitError
			if !errors.As(err, &exitError) || exitError.ExitCode() != 2 {
				t.Fatalf("deployment input %s error = %v, output = %q, want exit code 2", test.name, err, output)
			}
			if !strings.Contains(string(output), test.want) {
				t.Errorf("deployment input %s output = %q, want substring %q", test.name, output, test.want)
			}
		})
	}

	command := exec.CommandContext(t.Context(), "bash", script, "conseil-dev", "conseil")
	command.Env = append(os.Environ(), "VITRIER_PRIVATE_KEY_FILE="+filepath.Join(t.TempDir(), "missing.pem"))
	output, err := command.CombinedOutput()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 1 {
		t.Fatalf("deployment with missing key error = %v, output = %q, want exit code 1", err, output)
	}
	if !strings.Contains(string(output), "cannot read Vitrier private key") {
		t.Errorf("deployment with missing key output = %q, want missing-key message", output)
	}
}
