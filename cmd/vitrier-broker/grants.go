package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

const (
	grantDirMode     = 0o755
	grantFileMode    = 0o444
	maxRepositoryLen = 100
	maxGrantFileSize = maxRepositoryLen + 1
)

type grantStore struct {
	dir string
}

func newGrantStore(dir string) grantStore {
	return grantStore{dir: dir}
}

func (s grantStore) lookup(vm string) (string, error) {
	if !validVMName(vm) {
		return "", fmt.Errorf("invalid VM name %q", vm)
	}

	root, directory, err := s.open()
	if err != nil {
		return "", fmt.Errorf("opening grants directory: %w", err)
	}
	defer func() {
		_ = root.Close()
		_ = directory.Close()
	}()

	file, err := root.OpenFile(vm, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", fmt.Errorf("opening grant for %q: %w", vm, err)
	}
	defer func() { _ = file.Close() }()

	after, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("inspecting open grant for %q: %w", vm, err)
	}
	if err := s.validateGrantInfo(after); err != nil {
		return "", fmt.Errorf("open grant for %q is unsafe: %w", vm, err)
	}

	contents, err := io.ReadAll(io.LimitReader(file, maxGrantFileSize+1))
	if err != nil {
		return "", fmt.Errorf("reading grant for %q: %w", vm, err)
	}
	if len(contents) > maxGrantFileSize {
		return "", fmt.Errorf("grant for %q is oversized", vm)
	}
	if len(contents) < 2 || contents[len(contents)-1] != '\n' {
		return "", fmt.Errorf("grant for %q must contain one repository and a newline", vm)
	}
	repository := string(contents[:len(contents)-1])
	if !validRepositoryName(repository) {
		return "", fmt.Errorf("grant for %q contains an invalid repository", vm)
	}
	return repository, nil
}

func (s grantStore) grant(vm, repository string) (bool, error) {
	if !validVMName(vm) {
		return false, fmt.Errorf("invalid VM name %q", vm)
	}
	if !validRepositoryName(repository) {
		return false, fmt.Errorf("invalid GitHub repository name %q", repository)
	}
	if err := s.createDirectory(); err != nil {
		return false, err
	}

	root, directory, err := s.open()
	if err != nil {
		return false, fmt.Errorf("opening grants directory: %w", err)
	}
	defer func() {
		_ = root.Close()
		_ = directory.Close()
	}()

	tempName, file, err := createGrantTemp(root)
	if err != nil {
		return false, fmt.Errorf("creating temporary grant: %w", err)
	}
	keepTemp := true
	defer func() {
		_ = file.Close()
		if keepTemp {
			_ = root.Remove(tempName)
		}
	}()

	if _, err := io.WriteString(file, repository+"\n"); err != nil {
		return false, fmt.Errorf("writing temporary grant: %w", err)
	}
	if err := file.Chown(0, 0); err != nil {
		return false, fmt.Errorf("setting temporary grant owner: %w", err)
	}
	if err := file.Chmod(grantFileMode); err != nil {
		return false, fmt.Errorf("setting temporary grant permissions: %w", err)
	}
	if err := file.Sync(); err != nil {
		return false, fmt.Errorf("syncing temporary grant: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		return false, fmt.Errorf("inspecting temporary grant: %w", err)
	}
	if err := s.validateGrantInfo(info); err != nil {
		return false, fmt.Errorf("temporary grant is unsafe: %w", err)
	}
	if err := file.Close(); err != nil {
		return false, fmt.Errorf("closing temporary grant: %w", err)
	}

	if err := root.Link(tempName, vm); errors.Is(err, fs.ErrExist) {
		if err := root.Remove(tempName); err != nil {
			return false, fmt.Errorf("removing temporary grant: %w", err)
		}
		keepTemp = false
		if err := directory.Sync(); err != nil {
			return false, fmt.Errorf("syncing grants directory: %w", err)
		}
		existing, err := s.lookup(vm)
		if err != nil {
			return false, fmt.Errorf("reading existing grant for %q: %w", vm, err)
		}
		if existing != repository {
			return false, fmt.Errorf("VM %q already grants %q, not %q", vm, existing, repository)
		}
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("installing grant for %q: %w", vm, err)
	}
	if err := root.Remove(tempName); err != nil {
		return true, fmt.Errorf("removing linked temporary grant: %w", err)
	}
	keepTemp = false
	if err := directory.Sync(); err != nil {
		return true, fmt.Errorf("syncing grants directory: %w", err)
	}
	return true, nil
}

func (s grantStore) revoke(vm string) error {
	if !validVMName(vm) {
		return fmt.Errorf("invalid VM name %q", vm)
	}

	root, directory, err := s.open()
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("opening grants directory: %w", err)
	}
	defer func() {
		_ = root.Close()
		_ = directory.Close()
	}()

	if err := root.Remove(vm); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("removing grant for %q: %w", vm, err)
	}
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("syncing grants directory: %w", err)
	}
	return nil
}

func (s grantStore) createDirectory() error {
	_, beforeErr := os.Lstat(s.dir)
	created := errors.Is(beforeErr, fs.ErrNotExist)
	if beforeErr != nil && !created {
		return fmt.Errorf("inspecting grants directory: %w", beforeErr)
	}
	if err := os.MkdirAll(s.dir, grantDirMode); err != nil {
		return fmt.Errorf("creating grants directory: %w", err)
	}
	info, err := os.Lstat(s.dir)
	if err != nil {
		return fmt.Errorf("inspecting grants directory: %w", err)
	}
	if !info.IsDir() {
		return errors.New("grants path is not a directory")
	}
	if err := os.Chown(s.dir, 0, 0); err != nil {
		return fmt.Errorf("setting grants directory owner: %w", err)
	}
	if err := os.Chmod(s.dir, grantDirMode); err != nil {
		return fmt.Errorf("setting grants directory permissions: %w", err)
	}
	if created {
		parent, err := os.Open(filepath.Dir(s.dir))
		if err != nil {
			return fmt.Errorf("opening grants parent directory: %w", err)
		}
		defer func() { _ = parent.Close() }()
		if err := parent.Sync(); err != nil {
			return fmt.Errorf("syncing grants parent directory: %w", err)
		}
	}
	return nil
}

func (s grantStore) open() (*os.Root, *os.File, error) {
	info, err := os.Lstat(s.dir)
	if err != nil {
		return nil, nil, err
	}
	if !info.IsDir() {
		return nil, nil, errors.New("grants path is not a directory")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return nil, nil, errors.New("grants directory is writable by its group or others")
	}
	if err := validateRootOwner(info); err != nil {
		return nil, nil, fmt.Errorf("grants directory: %w", err)
	}

	directory, err := os.Open(s.dir)
	if err != nil {
		return nil, nil, err
	}
	openedInfo, err := directory.Stat()
	if err != nil {
		_ = directory.Close()
		return nil, nil, err
	}
	if !os.SameFile(info, openedInfo) {
		_ = directory.Close()
		return nil, nil, errors.New("grants directory changed while opening")
	}
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		_ = directory.Close()
		return nil, nil, err
	}
	rootInfo, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		_ = directory.Close()
		return nil, nil, err
	}
	if !os.SameFile(openedInfo, rootInfo) {
		_ = root.Close()
		_ = directory.Close()
		return nil, nil, errors.New("grants directory changed while opening root")
	}
	return root, directory, nil
}

func (s grantStore) validateGrantInfo(info fs.FileInfo) error {
	if !info.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	if info.Mode().Perm() != grantFileMode {
		return fmt.Errorf("mode is %04o, want %04o", info.Mode().Perm(), grantFileMode)
	}
	return validateRootOwner(info)
}

func validateRootOwner(info fs.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot determine owner")
	}
	if stat.Uid != 0 {
		return fmt.Errorf("owner UID is %d, want 0", stat.Uid)
	}
	if stat.Gid != 0 {
		return fmt.Errorf("owner GID is %d, want 0", stat.Gid)
	}
	return nil
}

func createGrantTemp(root *os.Root) (string, *os.File, error) {
	name := ".grant-" + rand.Text()
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	return name, file, err
}

func validRepositoryName(name string) bool {
	if len(name) == 0 || len(name) > maxRepositoryLen || name == "." || name == ".." {
		return false
	}
	for _, character := range name {
		if character != '-' && character != '_' && character != '.' &&
			(character < 'a' || character > 'z') &&
			(character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') {
			return false
		}
	}
	return true
}
