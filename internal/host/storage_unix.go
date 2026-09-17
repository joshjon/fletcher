//go:build linux || darwin

package host

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func capacity(path string) (uint64, uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	size := uint64(st.Bsize) //nolint:gosec // filesystem block size is positive
	return st.Blocks * size, st.Bavail * size, nil
}

func allocated(info os.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(max(st.Blocks, 0)) * 512
	}
	return uint64(max(info.Size(), 0))
}
