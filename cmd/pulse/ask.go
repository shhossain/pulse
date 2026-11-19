package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"pulse/internal/alert"
	"pulse/internal/client"
	"pulse/internal/model"
)

// cmdAsk asks a question for a job and prints the answer (for shell scripts:
// answer=$(pulse ask deploy "Deploy v2?" approve reject)). Exit 0 with the
// answer, 3 if it timed out without a default, 1 on errors.
func cmdAsk(args []string) int {
	var job, question, def string
	var options []string
	var timeout time.Duration
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--timeout":
			if i+1 >= len(args) {
				return printHelp("ask") + 2
			}
			i++
			d, err := alert.ParseDuration(args[i]) // same forms as rules: 30m, 1h30m, 1d
			if err != nil {
				return fail(fmt.Errorf("--timeout: use a duration like 30m, 2h or 1d"))
			}
			timeout = d
		case "--default":
			if i+1 >= len(args) {
				return printHelp("ask") + 2
			}
			i++
			def = args[i]
		default:
			switch {
			case job == "":
				job = a
			case question == "":
				question = a
			default:
				options = append(options, a)
			}
		}
	}
	if job == "" || question == "" {
		return printHelp("ask") + 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	c, err := client.Connect(ctx, false, 4*time.Second)
	cancel()
	if err != nil {
		return fail(err)
	}
	// Ask on the job's current run, starting one if it has none.
	pctx, pcancel := context.WithTimeout(context.Background(), 5*time.Second)
	var push model.BatchResponse
	err = c.Do(pctx, "POST", "/api/v1/push/"+url.PathEscape(job), map[string]any{"status": "running"}, &push)
	pcancel()
	if err != nil {
		return fail(err)
	}
	actx, acancel := context.WithTimeout(context.Background(), 5*time.Second)
	var a model.Ask
	err = c.Do(actx, "POST", "/api/v1/runs/"+url.PathEscape(push.Run)+"/asks",
		map[string]any{"question": question, "options": options, "default": def, "timeout_s": timeout.Seconds()}, &a)
	acancel()
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(os.Stderr, "Waiting for an answer in Pulse (%s)…\n", a.ID)
	long := c.WithTimeout(60 * time.Second)
	unreachableSince := time.Time{}
	for {
		wctx, wcancel := context.WithTimeout(context.Background(), 60*time.Second)
		err := long.Do(wctx, "GET", "/api/v1/asks/"+url.PathEscape(a.ID)+"?wait=30s", nil, &a)
		wcancel()
		var apiErr *client.APIError
		switch {
		case errors.As(err, &apiErr): // the question is gone or we lost access: don't wait forever
			return fail(fmt.Errorf("question %s: %v", a.ID, err))
		case err != nil:
			if unreachableSince.IsZero() {
				unreachableSince = time.Now()
			}
			if time.Since(unreachableSince) > 5*time.Minute {
				return fail(fmt.Errorf("Pulse has been unreachable for 5 minutes"))
			}
			time.Sleep(3 * time.Second)
			continue
		}
		unreachableSince = time.Time{}
		if a.AnsweredAt != nil {
			if (a.AnsweredBy == "timeout" || a.AnsweredBy == "run ended") && def == "" {
				fmt.Fprintln(os.Stderr, "pulse: no answer ("+a.AnsweredBy+")")
				return 3
			}
			fmt.Println(a.Answer)
			return 0
		}
	}
}

func cmdInbox(args []string) int {
	c, ctx, cancel, ok := admin()
	if !ok {
		return 1
	}
	defer cancel()
	var out struct {
		Asks []model.Ask `json:"asks"`
	}
	if err := c.Do(ctx, "GET", "/api/v1/asks", nil, &out); err != nil {
		return fail(err)
	}
	if jsonOut {
		return printJSON(out.Asks)
	}
	if len(out.Asks) == 0 {
		fmt.Println("Nothing is waiting for you.")
		return 0
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tJOB\tQUESTION\tANSWERS\tASKED")
	for _, a := range out.Asks {
		answers := strings.Join(a.Options, " / ")
		if answers == "" {
			answers = "(any text)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s ago\n", a.ID, a.Job, a.Question, answers, time.Since(a.Created).Round(time.Second))
	}
	tw.Flush()
	fmt.Println("\nAnswer with: pulse answer <id> <answer>")
	return 0
}

func cmdAnswer(args []string) int {
	if len(args) < 2 {
		return printHelp("answer") + 2
	}
	c, ctx, cancel, ok := admin()
	if !ok {
		return 1
	}
	defer cancel()
	var a model.Ask
	if err := c.Do(ctx, "POST", "/api/v1/asks/"+url.PathEscape(args[0])+"/answer",
		map[string]string{"answer": strings.Join(args[1:], " "), "by": "cli"}, &a); err != nil {
		return fail(err)
	}
	fmt.Printf("Answered %s: %s\n", a.Job, a.Answer)
	return 0
}
