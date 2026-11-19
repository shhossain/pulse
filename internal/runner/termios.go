//go:build darwin || linux

package runner

import (
	"os"

	"golang.org/x/sys/unix"
)

// rawOutput turns off output post-processing on the pty so "\n" is not
// rewritten to "\r\n" (matters when output is redirected to a file).
func rawOutput(ptmx *os.File) {
	t, err := unix.IoctlGetTermios(int(ptmx.Fd()), getTermios)
	if err != nil {
		return
	}
	t.Oflag &^= unix.OPOST
	_ = unix.IoctlSetTermios(int(ptmx.Fd()), setTermios, t)
}
