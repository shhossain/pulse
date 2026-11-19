package watch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"pulse/internal/actions"
)

// fetchGitHub reads a GitHub Actions run through the user's own `gh` CLI
// (so Pulse never handles GitHub credentials) and returns a JSON update.
func fetchGitHub(ctx context.Context, s Spec) ([]byte, error) {
	m := ghRunRe.FindStringSubmatch(s.Source)
	if m == nil {
		return nil, errors.New("not a GitHub Actions run URL")
	}
	owner, repo, id := m[1], m[2], m[3]
	// The path is quoted: '?' is a glob in zsh. owner/repo/id were checked by
	// ghRunRe, so they hold no quotes.
	gh := func(args string, out func([]byte) error) error {
		home, _ := os.UserHomeDir()
		r := actions.ShellStdout(ctx, "gh api "+args, home, nil, budget(ctx))
		if r.ExitCode == 127 || strings.Contains(r.Stderr, "command not found") {
			return errors.New("the GitHub CLI (gh) isn't installed; install it and run `gh auth login`")
		}
		if r.ExitCode != 0 {
			return fmt.Errorf("gh api: %s", strings.TrimSpace(r.Stderr))
		}
		return out([]byte(r.Output))
	}
	var run struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		HeadBranch string `json:"head_branch"`
		Event      string `json:"event"`
	}
	if err := gh(fmt.Sprintf("'repos/%s/%s/actions/runs/%s'", owner, repo, id), func(b []byte) error { return json.Unmarshal(b, &run) }); err != nil {
		return nil, err
	}
	type ghJob struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		Steps      []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"steps"`
	}
	var jobs struct{ Jobs []ghJob }
	// --paginate + --jq prints one job per line across every page (matrix
	// builds can have more than 100 jobs).
	err := gh(fmt.Sprintf("--paginate --jq '.jobs[]' 'repos/%s/%s/actions/runs/%s/jobs?per_page=100'", owner, repo, id), func(b []byte) error {
		dec := json.NewDecoder(strings.NewReader(string(b)))
		for dec.More() {
			var j ghJob
			if err := dec.Decode(&j); err != nil {
				return err
			}
			jobs.Jobs = append(jobs.Jobs, j)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	update := map[string]any{"workflow": run.Name, "branch": run.HeadBranch, "event": run.Event}
	done, total, current := 0, len(jobs.Jobs), ""
	for _, j := range jobs.Jobs {
		if j.Status == "completed" {
			done++
			continue
		}
		if j.Status == "in_progress" && current == "" {
			current = j.Name
			for _, st := range j.Steps {
				if st.Status == "in_progress" {
					current += " › " + st.Name
					break
				}
			}
		}
	}
	if total > 0 {
		update["progress"] = []any{done, total}
	}
	if current != "" {
		update["current"] = current
	}
	update["status"] = githubStatus(run.Status, run.Conclusion)
	return json.Marshal(update)
}

func githubStatus(status, conclusion string) string {
	if status != "completed" {
		return "running"
	}
	switch conclusion {
	case "success", "neutral", "skipped":
		return "done"
	case "cancelled":
		return "stopped"
	}
	return "failed" // failure, timed_out, action_required, startup_failure, stale
}
