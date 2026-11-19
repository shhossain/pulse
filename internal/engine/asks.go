package engine

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	"pulse/internal/alert"
	"pulse/internal/audit"
	"pulse/internal/model"
	"pulse/internal/paths"
)

const (
	maxAskOptions = 6
	maxAskText    = 500
)

func (e *Engine) loadAsks() error {
	// Open questions, and ones answered in the last hour: a job may still be
	// about to collect its answer when the app restarts.
	asks, err := e.st.RecentAsks(e.now().Add(-time.Hour))
	if err != nil {
		return err
	}
	for i := range asks {
		a := asks[i]
		e.asks[a.ID] = &a
	}
	return nil
}

// Ask opens a question on a run. The job waits for the answer with WaitAsk.
func (e *Engine) Ask(runID, question string, options []string, def string, timeout time.Duration) (model.Ask, error) {
	question = strings.TrimSpace(question)
	if question == "" || len([]rune(question)) > maxAskText {
		return model.Ask{}, fmt.Errorf("question must be 1–%d characters", maxAskText)
	}
	if len(options) > maxAskOptions {
		return model.Ask{}, fmt.Errorf("at most %d options", maxAskOptions)
	}
	trimmed := make([]string, 0, len(options))
	for _, o := range options {
		o = strings.TrimSpace(o)
		if o == "" || utf8.RuneCountInString(o) > 40 {
			return model.Ask{}, fmt.Errorf("options must be 1–40 characters")
		}
		if contains(trimmed, o) {
			return model.Ask{}, fmt.Errorf("option %q is listed twice", o)
		}
		trimmed = append(trimmed, o)
	}
	options, def = trimmed, strings.TrimSpace(def)
	if def != "" && len(options) > 0 && !contains(options, def) {
		return model.Ask{}, fmt.Errorf("default %q isn't one of the options", def)
	}
	e.mu.Lock()
	r := e.runs[runID]
	if r == nil {
		e.mu.Unlock()
		return model.Ask{}, fmt.Errorf("no such run")
	}
	a, note, err := e.askLocked(r, model.Ask{Question: question, Options: options, Default: def}, timeout)
	e.mu.Unlock()
	if err != nil {
		return model.Ask{}, err
	}
	e.emit([]model.Notification{note}, true)
	return a, nil
}

// askLocked stores a question on r and returns its notification. Caller
// holds e.mu.
func (e *Engine) askLocked(r *model.Run, a model.Ask, timeout time.Duration) (model.Ask, model.Notification, error) {
	now := e.now().UTC()
	a.ID, a.Run, a.Job, a.Created = "q_"+paths.RandomHex(6), r.ID, r.Job, now
	if timeout > 0 {
		d := now.Add(timeout)
		a.Deadline = &d
	}
	if err := e.st.SaveAsk(a); err != nil {
		return model.Ask{}, model.Notification{}, err
	}
	stored := a
	e.asks[a.ID] = &stored
	note := model.Notification{
		ID: a.ID, Level: "warn", Title: r.Job + " needs you", Subtitle: a.Question, Body: a.Context,
		Job: r.Job, RunID: r.ID, AskID: a.ID, AskOptions: a.Options,
	}
	return a, note, nil
}

// openRuleAsk reports whether an alert's question is already waiting on a run.
func (e *Engine) openRuleAsk(runID, ruleID string) bool {
	for _, a := range e.asks {
		if a.Open() && a.Run == runID && a.Rule == ruleID {
			return true
		}
	}
	return false
}

// carryOut does what an answer to an alert's question asks for.
func (e *Engine) carryOut(a model.Ask) {
	do := a.Then[a.Answer]
	if do == "" {
		return
	}
	kind, arg, _ := strings.Cut(do, ":")
	var err error
	switch kind {
	case "stop":
		_, err = e.Stop(a.Run)
	case "send":
		e.mu.Lock()
		r := e.runs[a.Run]
		if r == nil {
			err = fmt.Errorf("run is gone")
		} else if why := sendBlocked(r, arg); why != "" {
			err = fmt.Errorf("can't send %q: %s", arg, why)
		} else {
			e.enqueue(r.ID, arg)
		}
		e.mu.Unlock()
	case "action":
		_, err = e.RunAction(context.Background(), a.Run, arg, "answer")
	}
	if err != nil {
		log.Printf("answer %q to %s: %v", a.Answer, a.ID, err)
		// The answer was recorded, but what it should do didn't happen: say so.
		e.emit([]model.Notification{{
			ID: a.ID + "-failed", Level: "error", Title: fmt.Sprintf("%s: “%s” didn't go through", a.Job, a.Answer),
			Subtitle: err.Error(), Job: a.Job, RunID: a.Run,
		}}, false)
	}
}

// Answer answers an open question. by is ui, notification, cli or timeout.
func (e *Engine) Answer(askID, answer, by string) (model.Ask, error) {
	answer = strings.TrimSpace(answer)
	e.mu.Lock()
	a := e.asks[askID]
	if a == nil {
		e.mu.Unlock()
		return model.Ask{}, fmt.Errorf("no such question")
	}
	if !a.Open() {
		e.mu.Unlock()
		return *a, fmt.Errorf("already answered: %s", a.Answer)
	}
	if len(a.Options) > 0 && !contains(a.Options, answer) {
		e.mu.Unlock()
		return *a, fmt.Errorf("answer must be one of: %s", strings.Join(a.Options, ", "))
	}
	if answer == "" || len([]rune(answer)) > maxAskText {
		e.mu.Unlock()
		return *a, fmt.Errorf("answer must be 1–%d characters", maxAskText)
	}
	now := e.now().UTC()
	a.Answer, a.AnsweredAt, a.AnsweredBy = answer, &now, by
	if err := e.st.SaveAsk(*a); err != nil {
		log.Printf("asks: %v", err)
	}
	e.wakeAsk(a.ID)
	out := *a
	e.mu.Unlock()
	log.Printf("ask %s on %s answered %q by %s", a.ID, a.Job, answer, by)
	if out.Then[answer] != "" {
		go e.carryOut(out)
	}
	logged := answer
	if len(a.Options) == 0 { // free text may hold anything: record that it was answered, not what
		logged = fmt.Sprintf("(free text, %d characters)", len([]rune(answer)))
	}
	audit.Log("ask.answered", map[string]any{"id": a.ID, "job": a.Job, "question": a.Question, "answer": logged, "by": by})
	e.emit(nil, true)
	return out, nil
}

// GetAsk returns one question.
func (e *Engine) GetAsk(id string) (model.Ask, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if a := e.asks[id]; a != nil {
		return *a, true
	}
	return model.Ask{}, false
}

// WaitAsk returns the question once answered, or as it is after wait.
func (e *Engine) WaitAsk(ctx context.Context, id string, wait time.Duration) (model.Ask, bool) {
	deadline := time.Now().Add(wait)
	for {
		e.mu.Lock()
		a := e.asks[id]
		if a == nil {
			e.mu.Unlock()
			return model.Ask{}, false
		}
		if !a.Open() || time.Until(deadline) <= 0 {
			out := *a
			e.mu.Unlock()
			return out, true
		}
		ch := e.askWake[id]
		if ch == nil {
			ch = make(chan struct{})
			e.askWake[id] = ch
		}
		e.mu.Unlock()
		timer := time.NewTimer(time.Until(deadline))
		select {
		case <-ch:
		case <-ctx.Done():
		case <-timer.C:
		}
		timer.Stop()
		if ctx.Err() != nil {
			a, ok := e.GetAsk(id)
			return a, ok
		}
	}
}

// OpenAsks lists unanswered questions, oldest first.
func (e *Engine) OpenAsks() []model.Ask {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.openAsksLocked("")
}

func (e *Engine) openAsksLocked(runID string) []model.Ask {
	var out []model.Ask
	for _, a := range e.asks {
		if a.Open() && (runID == "" || a.Run == runID) {
			out = append(out, *a)
		}
	}
	sortAsks(out)
	return out
}

func sortAsks(a []model.Ask) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j].Created.Before(a[j-1].Created); j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

func (e *Engine) wakeAsk(id string) {
	if ch := e.askWake[id]; ch != nil {
		close(ch)
		delete(e.askWake, id)
	}
}

// expireAsks answers timed-out questions with their default (or "timeout")
// and closes questions whose run ended. Caller holds e.mu.
func (e *Engine) expireAsks(now time.Time) bool {
	changed := false
	for id, a := range e.asks {
		if !a.Open() {
			if now.Sub(*a.AnsweredAt) > time.Hour {
				delete(e.asks, id) // answered long ago: no one is waiting
			}
			continue
		}
		r := e.runs[a.Run]
		ended := r == nil || r.Status.Terminal()
		if (a.Deadline != nil && now.After(*a.Deadline)) || ended {
			ans, by := a.Default, "timeout"
			if ans == "" {
				ans = "timeout"
			}
			if ended {
				by = "run ended"
			}
			t := now.UTC()
			a.Answer, a.AnsweredAt, a.AnsweredBy = ans, &t, by
			_ = e.st.SaveAsk(*a)
			e.wakeAsk(id)
			changed = true
		}
	}
	return changed
}

// answerActionsExist checks that every "action:<name>" answer names one of
// the job's actions (have), so a typo shows up when the rule is added, not
// when someone clicks the answer.
func answerActionsExist(r alert.Rule, have map[string]bool) error {
	for _, a := range r.Answers {
		if name, ok := strings.CutPrefix(a.Do, "action:"); ok && !have[name] {
			return fmt.Errorf("answer %q runs the action %q, which this job doesn't have", a.Label, name)
		}
	}
	return nil
}
