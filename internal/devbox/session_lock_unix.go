//go:build darwin || linux

package devbox

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

const sessionLockRetryInterval = 10 * time.Millisecond

func lockSessionFile(ctx context.Context, file *os.File) error {
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(sessionLockRetryInterval):
		}
	}
}

func unlockSessionFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}
