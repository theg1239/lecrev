//go:build !linux

package microvm

import (
	"fmt"
	"os"
)

func cloneFile(_ *os.File, _ *os.File) error {
	return fmt.Errorf("file cloning is only supported on linux")
}
