package main

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestGrantWritesExpectedFile(t *testing.T) {
	requireRoot(t)
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "grants")
	store := testGrantStore(dir)
	created, err := store.grant("samovar-dev", "samovar")
	if err != nil {
		t.Fatalf("grant(samovar-dev, samovar) returned unexpected error: %v", err)
	}
	if !created {
		t.Error("grant(samovar-dev, samovar) created = false, want true")
	}
	created, err = store.grant("samovar-dev", "samovar")
	if err != nil {
		t.Fatalf("idempotent grant(samovar-dev, samovar) returned unexpected error: %v", err)
	}
	if created {
		t.Error("idempotent grant(samovar-dev, samovar) created = true, want false")
	}
	info, err := os.Stat(filepath.Join(dir, "samovar-dev"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != grantFileMode {
		t.Errorf("grant mode = %04o, want %04o", got, grantFileMode)
	}
	contents, err := os.ReadFile(filepath.Join(dir, "samovar-dev"))
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "samovar\n" {
		t.Errorf("grant contents = %q, want %q", contents, "samovar\\n")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "samovar-dev" {
		t.Errorf("grant directory entries = %v", entries)
	}
}

func TestRevokeIsIdempotent(t *testing.T) {
	requireRoot(t)
	t.Parallel()

	store := testGrantStore(filepath.Join(t.TempDir(), "grants"))
	if err := store.revoke("samovar-dev"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.grant("samovar-dev", "samovar"); err != nil {
		t.Fatalf("grant(samovar-dev, samovar) returned unexpected error: %v", err)
	}
	for range 2 {
		if err := store.revoke("samovar-dev"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.lookup("samovar-dev"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("lookup after revoke = %v, want not exist", err)
	}
}

func TestLookupRejectsMalformedAndUnsafeFiles(t *testing.T) {
	requireRoot(t)
	t.Parallel()

	tests := []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{
			name: "missing_newline",
			setup: func(t *testing.T, path string) {
				writeRawGrant(t, path, []byte("samovar"), grantFileMode)
			},
		},
		{
			name: "multiple_lines",
			setup: func(t *testing.T, path string) {
				writeRawGrant(t, path, []byte("samovar\nconseil\n"), grantFileMode)
			},
		},
		{
			name: "invalid_repository",
			setup: func(t *testing.T, path string) {
				writeRawGrant(t, path, []byte("owner/repo\n"), grantFileMode)
			},
		},
		{
			name: "oversized",
			setup: func(t *testing.T, path string) {
				writeRawGrant(t, path, make([]byte, maxGrantFileSize+1), grantFileMode)
			},
		},
		{
			name: "writable",
			setup: func(t *testing.T, path string) {
				writeRawGrant(t, path, []byte("samovar\n"), 0o644)
			},
		},
		{
			name: "directory",
			setup: func(t *testing.T, path string) {
				if err := os.Mkdir(path, grantFileMode); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlink",
			setup: func(t *testing.T, path string) {
				target := filepath.Join(filepath.Dir(filepath.Dir(path)), "target")
				writeRawGrant(t, target, []byte("samovar\n"), grantFileMode)
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(t.TempDir(), "grants")
			store := testGrantStore(dir)
			if err := store.createDirectory(); err != nil {
				t.Fatal(err)
			}
			test.setup(t, filepath.Join(dir, "samovar-dev"))
			if _, err := store.lookup("samovar-dev"); err == nil {
				t.Fatal("unsafe grant was accepted")
			}
		})
	}
}

func TestLookupRejectsUnsafeGrantDirectory(t *testing.T) {
	requireRoot(t)
	t.Parallel()

	t.Run("writable", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "grants")
		store := testGrantStore(dir)
		if err := store.createDirectory(); err != nil {
			t.Fatalf("createDirectory() returned unexpected error: %v", err)
		}
		if err := os.Chmod(dir, 0o777); err != nil {
			t.Fatalf("os.Chmod(%q) returned unexpected error: %v", dir, err)
		}
		if _, err := store.lookup("samovar-dev"); err == nil {
			t.Error("lookup(samovar-dev) in writable directory returned nil error, want non-nil")
		}
	})

	t.Run("symlink", func(t *testing.T) {
		t.Parallel()
		parent := t.TempDir()
		target := filepath.Join(parent, "target")
		if err := os.Mkdir(target, grantDirMode); err != nil {
			t.Fatalf("os.Mkdir(%q) returned unexpected error: %v", target, err)
		}
		dir := filepath.Join(parent, "grants")
		if err := os.Symlink(target, dir); err != nil {
			t.Fatalf("os.Symlink(%q, %q) returned unexpected error: %v", target, dir, err)
		}
		if _, err := testGrantStore(dir).lookup("samovar-dev"); err == nil {
			t.Error("lookup(samovar-dev) through directory symlink returned nil error, want non-nil")
		}
	})

	for _, test := range []struct {
		name string
		uid  int
		gid  int
	}{
		{name: "wrong_uid", uid: 1, gid: 0},
		{name: "wrong_gid", uid: 0, gid: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(t.TempDir(), "grants")
			store := testGrantStore(dir)
			if err := store.createDirectory(); err != nil {
				t.Fatalf("createDirectory() returned unexpected error: %v", err)
			}
			if err := os.Chown(dir, test.uid, test.gid); err != nil {
				t.Fatalf("os.Chown(%q, %d, %d): %v", dir, test.uid, test.gid, err)
			}
			if _, err := store.lookup("samovar-dev"); err == nil {
				t.Errorf("lookup(samovar-dev) in %s directory returned nil error, want non-nil", test.name)
			}
		})
	}
}

func TestGrantStoreConcurrentCreationDoesNotOverwrite(t *testing.T) {
	requireRoot(t)
	t.Parallel()

	store := testGrantStore(filepath.Join(t.TempDir(), "grants"))
	start := make(chan struct{})
	results := make(chan error, 32)
	var wait sync.WaitGroup
	for worker := range 32 {
		wait.Go(func() {
			<-start
			repository := []string{"samovar", "conseil"}[worker%2]
			_, err := store.grant("samovar-dev", repository)
			results <- err
		})
	}
	close(start)
	wait.Wait()
	close(results)

	var successes int
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes == 0 {
		t.Fatal("concurrent grant creation had no successful caller")
	}
	repository, err := store.lookup("samovar-dev")
	if err != nil {
		t.Fatalf("lookup(samovar-dev) returned unexpected error: %v", err)
	}
	if repository != "samovar" && repository != "conseil" {
		t.Errorf("lookup(samovar-dev) = %q, want samovar or conseil", repository)
	}
	created, err := store.grant("samovar-dev", repository)
	if err != nil {
		t.Fatalf("idempotent grant(samovar-dev, %s) returned unexpected error: %v", repository, err)
	}
	if created {
		t.Errorf("idempotent grant(samovar-dev, %s) created = true, want false", repository)
	}
	conflict := map[string]string{"samovar": "conseil", "conseil": "samovar"}[repository]
	if _, err := store.grant("samovar-dev", conflict); err == nil {
		t.Errorf("conflicting grant(samovar-dev, %s) returned nil error, want non-nil", conflict)
	}
}

func testGrantStore(dir string) grantStore {
	return newGrantStore(dir)
}

func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires root ownership checks")
	}
}

func writeRawGrant(t *testing.T, path string, contents []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}
