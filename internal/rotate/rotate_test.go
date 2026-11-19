package rotate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotates(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.log")
	f, err := Open(p, 100)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		f.Write([]byte(strings.Repeat("a", 30) + "\n"))
	}
	cur, _ := os.ReadFile(p)
	old, _ := os.ReadFile(p + ".1")
	if len(cur) > 100 || len(old) == 0 || len(old) > 100 {
		t.Fatalf("cur=%d old=%d", len(cur), len(old))
	}
}

func TestKeepsGenerations(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.log")
	f, _ := Open(p, 50)
	f.Keep = 3
	for i := 0; i < 20; i++ {
		f.Write([]byte(strings.Repeat("x", 40) + "\n"))
	}
	for _, s := range []string{".1", ".2", ".3"} {
		if _, err := os.Stat(p + s); err != nil {
			t.Errorf("missing %s", s)
		}
	}
	if _, err := os.Stat(p + ".4"); err == nil {
		t.Error("kept too many")
	}
}
