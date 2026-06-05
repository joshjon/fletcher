//go:build linux

package doctor

import (
	"os"
	"syscall"
)

// deviceGID returns the owning group ID of a stat'd device node.
func deviceGID(info os.FileInfo) (uint32, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Gid, true
}
