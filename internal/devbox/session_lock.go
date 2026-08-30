package devbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func (s *store) lockSession(ctx context.Context, id string) (func() error, error) {
	if err := validateSessionID(id); err != nil {
		return nil, err
	}
	path := filepath.Join(s.lockDir, id+".lock")
	before, err := os.Lstat(path)
	if err == nil && (!before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0) {
		return nil, errors.New("session lock path is not a regular file")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspecting session lock: %w", err)
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening session lock: %w", err)
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("inspecting open session lock: %w", err)
	}
	after, err := os.Lstat(path)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("inspecting session lock after opening: %w", err)
	}
	if !opened.Mode().IsRegular() || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, after) {
		_ = file.Close()
		return nil, errors.New("session lock changed while opening")
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("securing session lock: %w", err)
	}
	if err := lockSessionFile(ctx, file); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("locking session: %w", err)
	}
	return func() error {
		return errors.Join(unlockSessionFile(file), file.Close())
	}, nil
}
