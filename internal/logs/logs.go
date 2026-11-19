// Package logs stores each run's output in logs/<run>.log.
package logs

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// MaxBytes caps one run's log; output beyond it is dropped with a note.
const MaxBytes = 50 << 20

const capNote = "[pulse] log limit reached; further output is not saved\n"

// Store appends and reads run logs. Safe for concurrent use.
type Store struct {
	dir string
	mu  sync.Mutex
}

func New(dir string) *Store { return &Store{dir: dir} }

func (s *Store) path(run string) string { return filepath.Join(s.dir, run+".log") }

// Append adds lines to a run's log.
func (s *Store) Append(run string, lines []string) error {
	if len(lines) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path(run), os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	size := st.Size()
	if size >= MaxBytes || endsWith(f, size, capNote) { // capped already: the note was written
		return nil
	}
	var buf bytes.Buffer
	for _, l := range lines {
		l = strings.TrimRight(l, "\r\n") + "\n"
		if size+int64(buf.Len()+len(l)) > MaxBytes {
			buf.WriteString(capNote)
			break
		}
		buf.WriteString(l)
	}
	_, err = f.Write(buf.Bytes())
	return err
}

// endsWith reports whether the file's last bytes are suffix.
func endsWith(f *os.File, size int64, suffix string) bool {
	n := int64(len(suffix))
	if size < n {
		return false
	}
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, size-n); err != nil {
		return false
	}
	return string(buf) == suffix
}

// Tail returns the last n lines of a run's log (nil if there is none). It
// reads backwards in chunks until it has n lines, so long lines are fine.
func (s *Store) Tail(run string, n int) ([]string, error) {
	f, err := os.Open(s.path(run))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	var buf []byte
	for chunk := int64(64 << 10); ; chunk *= 2 {
		start := max(size-chunk, 0)
		buf = make([]byte, size-start)
		if _, err := f.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		lines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
		if start > 0 {
			lines = lines[1:] // the first line may be partial
		}
		if len(lines) >= n || start == 0 {
			if len(lines) > n {
				lines = lines[len(lines)-n:]
			}
			if len(lines) == 1 && lines[0] == "" {
				return []string{}, nil
			}
			return lines, nil
		}
	}
}

// Remove deletes a run's log.
func (s *Store) Remove(run string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = os.Remove(s.path(run))
}
