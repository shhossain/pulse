// Package runner implements `pulse run`: it runs a command in a pseudo-
// terminal, mirrors its output unchanged, parses progress/metrics from it,
// and reports to the app. It must never get in the way of the command.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"

	"pulse/internal/client"
	"pulse/internal/model"
	"pulse/internal/normalize"
	"pulse/internal/parse"
	"pulse/internal/sysstat"
)

// Options configures a run.
type Options struct {
	Name     string   // job name; inferred from Args if empty
	Inferred bool     // Name was guessed by the caller (affects #2 naming)
	Args     []string // command and arguments
	Quiet    bool     // don't print Pulse's own notes to stderr
	NoSystem bool     // don't report CPU, memory, GPU and disk use
}

// systemEvery is how often a run's CPU, memory, GPU and disk use is sampled.
const systemEvery = 5 * time.Second

// Run executes the command and returns its exit code.
func Run(opts Options) int {
	if len(opts.Args) == 0 {
		fmt.Fprintln(os.Stderr, "pulse run: missing command")
		return 2
	}
	name, inferred := opts.Name, opts.Inferred
	if name == "" {
		name, inferred = InferName(opts.Args), true
	}
	runID := model.NewRunID()
	cwd, _ := os.Getwd()
	host, _ := os.Hostname()

	sender := client.NewRunSender(runID, name, model.RunMeta{
		Source: "run", Cmd: shellJoin(opts.Args), Cwd: cwd, Host: host,
		PID: os.Getpid(), Heartbeat: true, Inferred: inferred, Accepts: []string{"stop", "helper_attached"},
	})
	sender.Add(map[string]any{"status": "running"})
	sender.Start()

	cmd := exec.Command(opts.Args[0], opts.Args[1:]...)
	cmd.Env = append(os.Environ(),
		"PULSE_JOB="+name,
		"PULSE_RUN="+runID,
		"PYTHONUNBUFFERED=1",
	)
	// Hand the child exactly what we resolved (the Python helper reads it),
	// including the token when PULSE_URL was set without one.
	if c, err := client.Resolve(false); err == nil {
		cmd.Env = append(cmd.Env, "PULSE_URL="+c.BaseURL, "PULSE_TOKEN="+c.Token)
	}

	tail := &tailLines{max: 8}
	var helper atomic.Bool // the Python helper reports metrics itself
	splitter := &parse.Splitter{Emit: func(line string) {
		ev, ok := parse.Line(line)
		if ok && !helper.Load() {
			sender.Add(ev)
		}
		if bar, _ := ev[normalize.KeyCoalesce].(bool); !bar {
			// Progress-bar redraws don't belong in logs or error messages.
			tail.add(line)
			sender.AddLog(parse.StripANSI(line))
		}
	}}

	start := time.Now()
	stopper := &stopper{}
	ctx, cancel := context.WithCancel(context.Background())
	var sampling sync.WaitGroup // stopped before the final status goes out
	if !opts.NoSystem {
		stopper.onStart = func(pid int) {
			sampling.Add(1)
			go func() {
				defer sampling.Done()
				sysstat.New(pid, cwd).Watch(ctx, systemEvery, func(m map[string]any) { sender.Add(m) })
			}()
		}
	}
	go listenForCommands(ctx, runID, stopper, func() { helper.Store(true) })
	code, startErr := execute(cmd, splitter, stopper)
	cancel()
	sampling.Wait()
	splitter.Flush()

	final := map[string]any{"exit_code": code}
	switch {
	case stopper.requested():
		final["status"] = "stopped"
		final["message"] = "Stopped from Pulse"
	case startErr != nil:
		final["status"] = "failed"
		final["message"] = startErr.Error()
		fmt.Fprintf(os.Stderr, "pulse: %v\n", startErr)
	case code == 0:
		final["status"] = "done"
	default:
		final["status"] = "failed"
		final["message"] = tail.String(4)
	}
	delivered := sender.Close(final, 3*time.Second)
	if !delivered && !opts.Quiet && time.Since(start) > 2*time.Second {
		fmt.Fprintln(os.Stderr, "pulse: couldn't reach the Pulse app, so this run wasn't fully recorded.")
	}
	return code
}

// execute runs cmd under a pty when possible (falls back to pipes) and
// returns the exit code. Output goes to our stdout and to sink.
func execute(cmd *exec.Cmd, sink io.Writer, stop *stopper) (int, error) {
	stdinTTY := term.IsTerminal(int(os.Stdin.Fd()))
	var attrs *syscall.SysProcAttr
	if stdinTTY {
		// Child gets the pty as stdin and controlling terminal: Ctrl-C,
		// job control and interactive prompts behave as usual.
		attrs = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	} else {
		// Keep piped/redirected stdin; the pty is only for output.
		cmd.Stdin = os.Stdin
		attrs = &syscall.SysProcAttr{Setsid: true}
	}

	var size *pty.Winsize
	sizeSrc := os.Stdin
	for _, f := range []*os.File{os.Stdout, os.Stdin, os.Stderr} {
		if s, err := pty.GetsizeFull(f); err == nil {
			size, sizeSrc = s, f
			break
		}
	}

	// Handle signals before starting the child. If this process was started
	// with SIGINT ignored (e.g. `pulse run … &` from a script), handling it
	// here means the child gets the default disposition back, so Ctrl-C and
	// Stop work in it.
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGWINCH)
	defer signal.Stop(sigs)

	ptmx, err := pty.StartWithAttrs(cmd, size, attrs)
	if err != nil {
		signal.Stop(sigs)
		if errors.Is(err, exec.ErrNotFound) || isNotFound(err) {
			return 127, err
		}
		return executePipes(cmd, sink, stop)
	}
	defer ptmx.Close()
	stop.started(cmd.Process.Pid)
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		rawOutput(ptmx) // output goes to a file/pipe: keep "\n" line endings
	}

	if stdinTTY {
		if old, err := term.MakeRaw(int(os.Stdin.Fd())); err == nil {
			defer term.Restore(int(os.Stdin.Fd()), old)
		}
		go func() { _, _ = io.Copy(ptmx, os.Stdin) }()
	}

	go func() {
		for sig := range sigs {
			if sig == syscall.SIGWINCH {
				_ = pty.InheritSize(sizeSrc, ptmx)
				continue
			}
			// Forward to the child's whole process group (dataloader workers, etc.).
			_ = syscall.Kill(-cmd.Process.Pid, sig.(syscall.Signal))
		}
	}()

	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		_, _ = io.Copy(io.MultiWriter(os.Stdout, sink), ptmx)
	}()

	waitErr := cmd.Wait()
	// Drain what the child wrote just before exiting. If a grandchild still
	// holds the terminal open, don't wait for it forever.
	select {
	case <-outDone:
	case <-time.After(500 * time.Millisecond):
	}
	return exitCode(waitErr), nil
}

func executePipes(failed *exec.Cmd, sink io.Writer, stop *stopper) (int, error) {
	// A Cmd can't be reused after a failed Start; build a fresh one.
	cmd := exec.Command(failed.Args[0], failed.Args[1:]...)
	cmd.Env = failed.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = io.MultiWriter(os.Stdout, sink)
	cmd.Stderr = io.MultiWriter(os.Stderr, sink)
	cmd.Stdin = os.Stdin
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(sigs)
	if err := cmd.Start(); err != nil {
		return 127, err
	}
	stop.started(cmd.Process.Pid)
	go func() {
		for sig := range sigs {
			_ = syscall.Kill(-cmd.Process.Pid, sig.(syscall.Signal))
		}
	}()
	return exitCode(cmd.Wait()), nil
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	return 1
}

func isNotFound(err error) bool {
	var pe *os.PathError
	return errors.As(err, &pe) && errors.Is(pe.Err, syscall.ENOENT)
}

// tailLines keeps the last few non-empty output lines (ANSI stripped).
type tailLines struct {
	mu    sync.Mutex
	max   int
	lines []string
}

func (t *tailLines) add(line string) {
	line = strings.TrimSpace(parse.StripANSI(line))
	if line == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines = append(t.lines, line)
	if len(t.lines) > t.max {
		t.lines = t.lines[len(t.lines)-t.max:]
	}
}

func (t *tailLines) String(n int) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	l := t.lines
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, "\n")
}

func shellJoin(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\"'$\\") {
			out[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		} else {
			out[i] = a
		}
	}
	return strings.Join(out, " ")
}

// stopper stops the child's process group on request from the app:
// SIGINT (like Ctrl-C), then SIGTERM after 10s, then SIGKILL after 20s.
type stopper struct {
	mu      sync.Mutex
	pid     int
	asked   bool
	exited  bool
	onStart func(pid int) // called once the child exists
}

func (s *stopper) started(pid int) {
	if s.onStart != nil {
		s.onStart(pid)
	}
	s.mu.Lock()
	s.pid = pid
	asked := s.asked
	s.mu.Unlock()
	if asked { // a stop arrived before the child existed: carry it out now
		s.signal(pid)
	}
}

func (s *stopper) requested() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.asked
}

func (s *stopper) stop() {
	s.mu.Lock()
	first := !s.asked
	s.asked = true
	pid := s.pid
	s.mu.Unlock()
	if pid == 0 || !first {
		return
	}
	s.signal(pid)
}

// signal sends SIGINT to the group, then SIGTERM and SIGKILL if it lingers.
func (s *stopper) signal(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGINT)
	go func() {
		for _, step := range []struct {
			after time.Duration
			sig   syscall.Signal
		}{{10 * time.Second, syscall.SIGTERM}, {10 * time.Second, syscall.SIGKILL}} {
			time.Sleep(step.after)
			if syscall.Kill(-pid, 0) != nil { // group is gone
				return
			}
			_ = syscall.Kill(-pid, step.sig)
		}
	}()
}

// listenForCommands long-polls the app for commands (stop) until ctx ends.
func listenForCommands(ctx context.Context, runID string, s *stopper, helperAttached func()) {
	for ctx.Err() == nil {
		c, err := client.Resolve(false)
		if err != nil {
			sleepCtx(ctx, 3*time.Second)
			continue
		}
		cmds, err := c.WaitCommands(ctx, runID, 30*time.Second, "stop", "helper_attached")
		if err != nil {
			sleepCtx(ctx, 3*time.Second)
			continue
		}
		for _, cmd := range cmds {
			switch cmd.Name {
			case "stop":
				s.stop()
			case "helper_attached":
				helperAttached()
			}
			ackCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = c.AckCommand(ackCtx, runID, cmd.ID)
			cancel()
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
