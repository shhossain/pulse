// Package actions runs user-defined shell actions for the app.
package actions

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// MaxOutput caps captured output of actions (shown to people);
// MaxParsedOutput caps commands whose output Pulse parses (watchers).
const (
	MaxOutput       = 64 << 10
	MaxParsedOutput = 1 << 20
)

// Result of running a shell action.
type Result struct {
	ExitCode  int    `json:"exit_code"`
	Output    string `json:"output"`
	Stderr    string `json:"stderr,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	TimedOut  bool   `json:"timed_out,omitempty"`
	Millis    int64  `json:"ms"`
}

// Shell runs command through the user's login shell (so PATH and friends
// match their terminal, even though the app was started by launchd), in
// dir, with extra environment variables. The whole process group is killed
// on timeout.
func Shell(ctx context.Context, command, dir string, env []string, timeout time.Duration) Result {
	return run(ctx, command, dir, env, timeout, false)
}

// ShellStdout is Shell, but Output holds stdout only (stderr goes to
// Stderr), for commands whose output is parsed.
func ShellStdout(ctx context.Context, command, dir string, env []string, timeout time.Duration) Result {
	return run(ctx, command, dir, env, timeout, true)
}

func run(ctx context.Context, command, dir string, env []string, timeout time.Duration, splitStderr bool) Result {
	sh := os.Getenv("SHELL")
	if sh == "" {
		sh = "/bin/zsh"
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.Command(sh, "-l", "-c", command)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return Result{ExitCode: 127, Output: "folder doesn't exist: " + dir}
	}
	cmd.Dir = dir
	// A background child that keeps our output pipe open must not hold us
	// up: once the shell exits, stop waiting for the pipe after 2s.
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = append(os.Environ(), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, errOut := limitedBuffer{max: MaxOutput}, limitedBuffer{max: MaxOutput}
	if splitStderr {
		out.max = MaxParsedOutput
	}
	cmd.Stdout, cmd.Stderr = &out, &out
	if splitStderr {
		cmd.Stderr = &errOut
	}
	start := time.Now()
	res := Result{}
	if err := cmd.Start(); err != nil {
		res.ExitCode, res.Output = 127, err.Error()
		return res
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case err = <-done:
		case <-time.After(3 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			err = <-done
		}
		res.TimedOut = true
	}
	res.Millis = time.Since(start).Milliseconds()
	res.Output, res.Truncated, res.Stderr = out.String(), out.truncated, errOut.String()
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.Is(err, exec.ErrWaitDelay): // shell exited 0; a background child kept the pipe
	case errors.As(err, &ee):
		res.ExitCode = ee.ExitCode()
		if res.ExitCode < 0 {
			res.ExitCode = 128 + int(syscall.SIGTERM)
		}
	default:
		res.ExitCode = 1
	}
	return res
}

// limitedBuffer keeps the first MaxOutput bytes. (Not embedding
// bytes.Buffer on purpose: io.Copy would use its ReadFrom and skip the cap.)
type limitedBuffer struct {
	buf       bytes.Buffer
	truncated bool
	max       int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room < len(p) {
		if room > 0 {
			b.buf.Write(p[:room])
		}
		b.truncated = true
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *limitedBuffer) String() string { return b.buf.String() }
