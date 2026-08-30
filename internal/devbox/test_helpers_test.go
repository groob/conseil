package devbox

import (
	"os"
	"path/filepath"
	"testing"
)

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type lifecycleHarness struct {
	commands   string
	operations string
	request    string
	removed    string
}

func installLifecycleHarness(t *testing.T) lifecycleHarness {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	harness := lifecycleHarness{
		commands:   filepath.Join(root, "commands"),
		operations: filepath.Join(root, "operations"),
		request:    filepath.Join(root, "request.json"),
		removed:    filepath.Join(root, "removed"),
	}
	for name, value := range map[string]string{
		"DEVBOX_COMMANDS":         harness.commands,
		"DEVBOX_OPERATIONS":       harness.operations,
		"DEVBOX_REQUEST":          harness.request,
		"DEVBOX_REMOVED":          harness.removed,
		"DEVBOX_VM_NAME_FILE":     filepath.Join(root, "vm-name"),
		"DEVBOX_VM_IDENTITY_FILE": filepath.Join(root, "vm-identity"),
		"DEVBOX_SSH_DEST":         "direct.example",
		"DEVBOX_READY":            `{"protocol":1,"event":"ready","environment":{"SAMOVAR_TEST_DATABASE_URL":"postgres://test"}}`,
	} {
		t.Setenv(name, value)
	}
	path := filepath.Join(bin, "ssh")
	if err := os.WriteFile(path, []byte(lifecycleSSHScript), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return harness
}

func readTestFile(t *testing.T, path string) []byte {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

const lifecycleSSHScript = `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$DEVBOX_COMMANDS"
case "$*" in
  *"inspect-guest"*)
    if [ "${DEVBOX_SCENARIO:-}" = unreachable ]; then exit 1; fi
    cat "$DEVBOX_FACTS"
    ;;
  *"exe.dev new "*)
    name=
    identity=
    previous=
    for value in "$@"; do
      if [ "$previous" = name ]; then name=$value; fi
      if [ "$previous" = identity ]; then identity=$value; fi
      case "$value" in
        --name) previous=name ;;
        --comment) previous=identity ;;
        *) previous= ;;
      esac
    done
    printf '%s' "$name" > "$DEVBOX_VM_NAME_FILE"
    printf '%s' "$identity" > "$DEVBOX_VM_IDENTITY_FILE"
    if [ "${DEVBOX_SCENARIO:-}" = reconcile ]; then exit 1; fi
    printf '{"name":"%s","ssh_dest":"%s","comment":"%s"}' "$name" "$DEVBOX_SSH_DEST" "$identity"
    ;;
  *"exe.dev ls -l --json"*)
    if [ -f "$DEVBOX_REMOVED" ]; then
      printf '{"vms":[]}'
      exit 0
    fi
    name=${DEVBOX_VM_NAME:-}
    identity=${DEVBOX_VM_IDENTITY:-}
    if [ -n "${DEVBOX_VM_NAME_FILE:-}" ] && [ -f "$DEVBOX_VM_NAME_FILE" ]; then name=$(cat "$DEVBOX_VM_NAME_FILE"); fi
    if [ -n "${DEVBOX_VM_IDENTITY_FILE:-}" ] && [ -f "$DEVBOX_VM_IDENTITY_FILE" ]; then identity=$(cat "$DEVBOX_VM_IDENTITY_FILE"); fi
    if [ "${DEVBOX_SCENARIO:-}" = identity_reuse ]; then identity=another-session; fi
    printf '{"vm_name":"%s","ssh_dest":"%s","comment":"%s"}' "$name" "$DEVBOX_SSH_DEST" "$identity"
    ;;
  *"sha256sum "*)
    printf '%s\n' "$DEVBOX_WORKER_SHA"
    ;;
  *" bootstrap "*)
    if [ "${DEVBOX_SCENARIO:-}" = bootstrap_failure ]; then exit 1; fi
    printf '%s\n' "$DEVBOX_READY"
    ;;
  *" run-task "*)
    cat > "$DEVBOX_REQUEST"
    if [ "${DEVBOX_SCENARIO:-}" = task_failure ]; then exit 1; fi
    cat "$DEVBOX_TASK_RESULT"
    ;;
  *" stream-artifact "*"events.jsonl"*)
    cat "$DEVBOX_EVENTS"
    ;;
  *" stream-artifact "*"session.jsonl"*)
    cat "$DEVBOX_SESSION"
    ;;
  *"vitrier-broker revoke "*)
    printf 'revoke\n' >> "$DEVBOX_OPERATIONS"
    if [ "${DEVBOX_REVOKE_FAIL:-}" = 1 ]; then exit 1; fi
    ;;
  *"exe.dev rm "*)
    printf 'remove\n' >> "$DEVBOX_OPERATIONS"
    if [ "${DEVBOX_SCENARIO:-}" = remove_fail_once ] && [ ! -f "$DEVBOX_REMOVE_ATTEMPT" ]; then
      : > "$DEVBOX_REMOVE_ATTEMPT"
      exit 1
    fi
    if [ "${DEVBOX_SCENARIO:-}" != keep_vm ]; then : > "$DEVBOX_REMOVED"; fi
    ;;
  *"cat > "*)
    cat > /dev/null
    ;;
esac
`
