//go:build linux

package microvm

import (
	"os"

	"golang.org/x/sys/unix"
)

func cloneFile(dst, src *os.File) error {
	return unix.IoctlFileClone(int(dst.Fd()), int(src.Fd()))
}
