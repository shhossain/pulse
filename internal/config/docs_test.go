package config

import (
	"os"
	"regexp"
	"testing"

	"pulse/internal/alert"
)

// The YAML examples in the docs and the agent skill must parse: people copy them.
func TestDocExamplesParse(t *testing.T) {
	block := regexp.MustCompile("(?s)```yaml\n(.*?)```")
	n := 0
	for _, path := range []string{"../../docs/config.md", "../../skills/pulse/SKILL.md", "../../README.md"} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, m := range block.FindAllSubmatch(b, -1) {
			n++
			f, err := Parse(m[1])
			if err != nil {
				t.Errorf("%s example %d: %v", path, i+1, err)
				continue
			}
			for _, j := range f.Jobs {
				for _, a := range j.Alerts {
					if _, err := alert.Parse(a.Expr); err != nil {
						t.Errorf("%s example %d: alert %q: %v", path, i+1, a.Expr, err)
					}
					for _, ans := range a.Answers {
						if _, err := alert.ParseAnswer(ans); err != nil {
							t.Errorf("%s example %d: answer %q: %v", path, i+1, ans, err)
						}
					}
				}
			}
		}
	}
	if n < 3 {
		t.Fatalf("found only %d YAML examples", n)
	}
}
