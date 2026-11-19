package parse

// Splitter turns a byte stream into lines, treating both '\r' and '\n' as
// line ends so carriage-return redraws (tqdm) are seen as separate lines.
type Splitter struct {
	Max  int               // max line length kept (excess is dropped)
	Emit func(line string) // called for every non-empty line
	buf  []byte
}

// Write implements io.Writer.
func (s *Splitter) Write(p []byte) (int, error) {
	max := s.Max
	if max <= 0 {
		max = 64 * 1024
	}
	for _, b := range p {
		if b == '\n' || b == '\r' {
			s.flush()
			continue
		}
		if len(s.buf) < max {
			s.buf = append(s.buf, b)
		}
	}
	return len(p), nil
}

// Flush emits any partial final line.
func (s *Splitter) Flush() { s.flush() }

func (s *Splitter) flush() {
	if len(s.buf) > 0 {
		s.Emit(string(s.buf))
		s.buf = s.buf[:0]
	}
}
