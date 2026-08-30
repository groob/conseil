//go:build !darwin && !linux

package devbox

import (
	"context"
	"errors"
	"os"
)

func lockSessionFile(context.Context, *os.File) error {
	return errors.New("session locking is unsupported on this platform")
}

func unlockSessionFile(*os.File) error {
	return nil
}
