//go:build darwin || linux

package devbox

import (
	"fmt"
	"os"
)

func validateStoreDirectory(info os.FileInfo) error {
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("directory mode is %04o, want no group or other write access", info.Mode().Perm())
	}
	return nil
}
