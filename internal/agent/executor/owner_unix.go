//go:build !windows

package executor

import (
	"io/fs"
	"os"
	"syscall"
)

// copyOwner gives f the owner of st. It needs root, which the agent has in its
// image; without it the file keeps the agent's user.
func copyOwner(f *os.File, st fs.FileInfo) {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		_ = f.Chown(int(s.Uid), int(s.Gid))
	}
}
