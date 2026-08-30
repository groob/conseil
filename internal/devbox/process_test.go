//go:build darwin || linux

package devbox

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestPrepareControlCommandInjectsBoundedSafeOptions(t *testing.T) {
	controlDir := filepath.Join(t.TempDir(), "ssh")

	originalArgs := [][]string{
		{"exe.dev", "new", "--json", "--name", "worker"},
		{"exe.dev", "integrations", "attach", "vitrier", "vm:worker"},
		{"exe.dev", "ssh", "groob-tools", "vitrier-broker", "grant", "worker", "project"},
		{"worker.exe.xyz", "true"},
		{"worker.exe.xyz", "upload"},
		{"worker.exe.xyz", "bootstrap"},
		{"worker.exe.xyz", "run-task"},
		{"worker.exe.xyz", "stream-artifact"},
	}
	options := []string{
		"-o", "ControlMaster=auto",
		"-o", "ControlPersist=5m",
		"-o", "ControlPath=" + filepath.Join(controlDir, "c-%C"),
		"-o", "ForwardAgent=no",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=15",
		"-o", "ConnectionAttempts=3",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
	}
	for index, args := range originalArgs {
		prepared, err := prepareControlCommand(command{Name: "ssh", Args: args}, controlDir, os.Getuid())
		if err != nil {
			t.Fatal(err)
		}
		want := append(slices.Clone(options), args...)
		if !slices.Equal(prepared.Args, want) {
			t.Errorf("command %d args = %q, want %q", index, prepared.Args, want)
		}
	}
	info, err := os.Stat(controlDir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("control directory mode = %04o", info.Mode().Perm())
	}
	defaultSocket := filepath.Join("/tmp", "conseil-ssh-"+strconv.Itoa(os.Getuid()), "c-"+strings.Repeat("a", 40))
	if len(defaultSocket) >= 100 {
		t.Fatalf("default control socket path is %d bytes: %s", len(defaultSocket), defaultSocket)
	}
}

func TestEnsureSSHControlDirAcceptsExistingSecureDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ssh")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := ensureSSHControlDir(path, os.Getuid()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEnsureSSHControlDirRejectsUnsafeDirectory(t *testing.T) {
	tests := map[string]func(*testing.T) (string, int){
		"symlink": func(t *testing.T) (string, int) {
			target := filepath.Join(t.TempDir(), "target")
			if err := os.Mkdir(target, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "ssh")
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
			return path, os.Getuid()
		},
		"wrong_mode": func(t *testing.T) (string, int) {
			path := filepath.Join(t.TempDir(), "ssh")
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o755); err != nil {
				t.Fatal(err)
			}
			return path, os.Getuid()
		},
		"wrong_owner": func(t *testing.T) (string, int) {
			path := filepath.Join(t.TempDir(), "ssh")
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			return path, os.Getuid() + 1
		},
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			path, uid := setup(t)
			if err := ensureSSHControlDir(path, uid); err == nil {
				t.Fatal("unsafe control directory was accepted")
			}
		})
	}
}
