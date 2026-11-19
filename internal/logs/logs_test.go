package logs

import (
	"fmt"
	"strings"
	"testing"
)

func TestAppendTail(t *testing.T) {
	s := New(t.TempDir())
	if l, err := s.Tail("r_none", 5); err != nil || l != nil {
		t.Fatalf("missing log: %v %v", l, err)
	}
	for i := 0; i < 1000; i++ {
		_ = s.Append("r_1", []string{fmt.Sprintf("line %d", i)})
	}
	l, err := s.Tail("r_1", 3)
	if err != nil || len(l) != 3 || l[0] != "line 997" || l[2] != "line 999" {
		t.Fatalf("tail = %v, %v", l, err)
	}
	l, _ = s.Tail("r_1", 5000)
	if len(l) != 1000 || l[0] != "line 0" {
		t.Fatalf("full tail len=%d first=%q", len(l), l[0])
	}
}

func TestTailLongLinesAndCapPerFile(t *testing.T) {
	s := New(t.TempDir())
	long := strings.Repeat("x", 300)
	var batch []string
	for i := 0; i < 2000; i++ {
		batch = append(batch, fmt.Sprintf("%04d %s", i, long))
	}
	s.Append("r_1", batch)
	l, _ := s.Tail("r_1", 500)
	if len(l) != 500 || !strings.HasPrefix(l[499], "1999 ") {
		t.Fatalf("tail len=%d last=%.10q", len(l), l[len(l)-1])
	}
	// Past the cap, nothing more is written (and the note appears once).
	big := strings.Repeat("y", 1<<20)
	for i := 0; i < 60; i++ {
		s.Append("r_2", []string{big})
	}
	s.Append("r_2", []string{"later short line"})
	tail, _ := s.Tail("r_2", 3)
	if strings.Contains(strings.Join(tail, ""), "later short line") {
		t.Fatal("wrote past the cap")
	}
}
