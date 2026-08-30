//go:build darwin || linux

package devbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

const sshControlPersist = "5m"

func runControlCommand(ctx context.Context, command command) error {
	uid := os.Getuid()
	prepared, err := prepareControlCommand(command, filepath.Join("/tmp", "conseil-ssh-"+strconv.Itoa(uid)), uid)
	if err != nil {
		return err
	}
	return osCommand(ctx, prepared)
}

func prepareControlCommand(command command, controlDir string, uid int) (command, error) {
	if command.Name != "ssh" {
		return command, nil
	}
	if err := ensureSSHControlDir(controlDir, uid); err != nil {
		return command, fmt.Errorf("preparing SSH control directory: %w", err)
	}
	options := []string{
		"-o", "ControlMaster=auto",
		"-o", "ControlPersist=" + sshControlPersist,
		"-o", "ControlPath=" + filepath.Join(controlDir, "c-%C"),
		"-o", "ForwardAgent=no",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=15",
		"-o", "ConnectionAttempts=3",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
	}
	command.Args = append(options, command.Args...)
	return command, nil
}

func ensureSSHControlDir(path string, uid int) error {
	if err := os.Mkdir(path, 0o700); err != nil && !os.IsExist(err) {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspecting %s: %w", path, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is not a directory", path)
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("%s has mode %04o, want 0700", path, info.Mode().Perm())
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s has unsupported ownership metadata", path)
	}
	if stat.Uid != uint32(uid) {
		return fmt.Errorf("%s is owned by UID %d, want %d", path, stat.Uid, uid)
	}
	return nil
}
