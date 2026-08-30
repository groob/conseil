//go:build !darwin && !linux

package devbox

import (
	"errors"
	"os"
)

func validateStoreDirectory(os.FileInfo) error {
	return errors.New("devbox stores require a Darwin or Linux host")
}
