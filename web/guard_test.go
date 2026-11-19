package web

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// Job names, metric names, log lines, questions and notification text come
// from job machines, which may be compromised: the UI must only ever show
// them as text. Svelte escapes text by default; these are the ways around it.
func TestUIRendersJobTextAsText(t *testing.T) {
	raw := regexp.MustCompile(`\{@html|innerHTML|outerHTML|insertAdjacentHTML|document\.write`)
	err := filepath.WalkDir("src", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if loc := raw.FindIndex(b); loc != nil {
			t.Errorf("%s renders raw HTML (%q); show job-supplied text as text", path, b[loc[0]:loc[1]])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
