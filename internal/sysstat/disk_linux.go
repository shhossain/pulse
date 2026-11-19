package sysstat

import "golang.org/x/sys/unix"

// Linux counts free blocks in fragment-size units (what df uses).
func blockSize(st *unix.Statfs_t) uint64 {
	if st.Frsize > 0 {
		return uint64(st.Frsize)
	}
	return uint64(st.Bsize)
}
