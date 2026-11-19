package actions

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestShell(t *testing.T) {
	dir := t.TempDir()
	r := Shell(context.Background(), `echo "hi $PULSE_JOB from $(pwd)"; exit 3`, dir, []string{"PULSE_JOB=train"}, 10*time.Second)
	if r.ExitCode != 3 || !strings.Contains(r.Output, "hi train from") || !strings.Contains(r.Output, dir[len(dir)-8:]) {
		t.Fatalf("result = %+v", r)
	}
	r = Shell(context.Background(), "sleep 30", dir, nil, 300*time.Millisecond)
	if !r.TimedOut || r.Millis > 5000 {
		t.Fatalf("timeout result = %+v", r)
	}
	r = Shell(context.Background(), "yes | head -c 200000", dir, nil, 10*time.Second)
	if !r.Truncated || len(r.Output) != MaxOutput {
		t.Fatalf("truncation: len=%d truncated=%v", len(r.Output), r.Truncated)
	}
}

func TestShellBackgroundChildDoesNotHang(t *testing.T) {
	start := time.Now()
	r := Shell(context.Background(), "sleep 60 & echo started", t.TempDir(), nil, 30*time.Second)
	if time.Since(start) > 5*time.Second || r.TimedOut || !strings.Contains(r.Output, "started") || r.ExitCode != 0 {
		t.Fatalf("took %v: %+v", time.Since(start), r)
	}
	if r := Shell(context.Background(), "pwd", "/definitely/not/here", nil, time.Second); r.ExitCode != 127 {
		t.Fatalf("missing folder: %+v", r)
	}
}
