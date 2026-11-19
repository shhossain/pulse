package runner

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestStopBeforeStartIsCarriedOut(t *testing.T) {
	s := &stopper{}
	s.stop() // the stop command arrives before the child exists
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s.started(cmd.Process.Pid)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		t.Fatal("the early stop was never carried out")
	}
}
