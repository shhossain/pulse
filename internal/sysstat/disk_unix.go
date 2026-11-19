//go:build darwin || linux

package sysstat

import "golang.org/x/sys/unix"

func diskFree(dir string) (uint64, bool) {
	if dir == "" {
		return 0, false
	}
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, false
	}
	return st.Bavail * blockSize(&st), true // space an unprivileged user can use
}
