// Package rotate is a size-capped append-only log file: when it grows past
// Max bytes it's renamed to <name>.1 (one generation kept) and restarted.
package rotate

import (
	"fmt"
	"os"
	"sync"
)

// File is an io.Writer safe for concurrent use.
type File struct {
	Path string
	Max  int64
	Keep int // generations kept (default 1: <name>.1)

	mu   sync.Mutex
	f    *os.File
	size int64
}

func Open(path string, max int64) (*File, error) {
	r := &File{Path: path, Max: max}
	return r, r.open()
}

func (r *File) open() error {
	f, err := os.OpenFile(r.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, st.Size()
	return nil
}

func (r *File) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size+int64(len(p)) > r.Max && r.size > 0 {
		r.f.Close()
		keep := max(r.Keep, 1)
		for i := keep - 1; i >= 1; i-- { // .1 → .2 → … (oldest dropped)
			_ = os.Rename(fmt.Sprintf("%s.%d", r.Path, i), fmt.Sprintf("%s.%d", r.Path, i+1))
		}
		_ = os.Rename(r.Path, r.Path+".1")
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}
