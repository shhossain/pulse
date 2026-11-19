package sysstat

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestSampleOwnProcess(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no process source")
	}
	// A child that burns CPU, so the tree has a descendant and some load.
	cmd := exec.Command("sh", "-c", "while :; do :; done")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	s := New(os.Getpid(), t.TempDir())
	s.gpu = nil // the host's GPU isn't under test
	s.Sample(context.Background())
	time.Sleep(1500 * time.Millisecond)
	m := s.Sample(context.Background())
	if mem, _ := m[KeyMem].(float64); mem <= 0 {
		t.Errorf("mem = %v", m[KeyMem])
	}
	if cpu, _ := m[KeyCPU].(float64); cpu < 20 {
		t.Errorf("cpu = %v, want the busy child counted", m[KeyCPU])
	}
	if free, _ := m[KeyDiskFree].(float64); free <= 0 {
		t.Errorf("disk free = %v", m[KeyDiskFree])
	}
}

func TestCPUMeterAndTree(t *testing.T) {
	var m cpuMeter
	t0 := time.Unix(100, 0)
	if _, ok := m.percent([]proc{{pid: 1, cpuSec: 10}}, t0); ok {
		t.Fatal("first sample has no span")
	}
	// pid 1 used 1s in 2s; pid 2 started since and used 1s: (1+1)/2 = 100%.
	got, ok := m.percent([]proc{{pid: 1, cpuSec: 11}, {pid: 2, cpuSec: 1}}, t0.Add(2*time.Second))
	if !ok || got != 100 {
		t.Fatalf("cpu = %v %v", got, ok)
	}
	tree := descendants([]proc{{pid: 1, ppid: 0}, {pid: 2, ppid: 1}, {pid: 3, ppid: 2}, {pid: 4, ppid: 9}}, 1)
	if len(tree) != 3 {
		t.Fatalf("tree = %+v", tree)
	}
}

func TestNvidia(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "nvidia-smi")
	script := `#!/bin/sh
case "$1" in
--query-gpu*) printf 'GPU-a, 90, 1024\nGPU-b, 10, 2048\nGPU-c, [N/A], [N/A]\n' ;;
--query-compute-apps*) printf 'GPU-a, 42, 512\nGPU-b, 7, 100\n' ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	n := nvidia{bin: bin}
	util, mem, ok := n.sample(context.Background(), []int{42})
	if !ok || util != 90 || mem != 512*(1<<20) {
		t.Fatalf("own process: %v %v %v", util, mem, ok)
	}
	// Others are on the GPUs but the job isn't: nothing (not their load).
	if _, _, ok := n.sample(context.Background(), []int{99}); ok {
		t.Fatal("reported other processes' GPU use as the job's")
	}
	// The driver lists no processes (containers): the whole machine.
	quiet := filepath.Join(dir, "quiet-smi")
	os.WriteFile(quiet, []byte("#!/bin/sh\ncase \"$1\" in --query-gpu*) printf 'GPU-a, 90, 1024\\nGPU-b, 10, 2048\\n' ;; esac\n"), 0o755)
	util, mem, ok = nvidia{bin: quiet}.sample(context.Background(), []int{99})
	if !ok || util != 50 || mem != 3072*(1<<20) {
		t.Fatalf("machine: %v %v %v", util, mem, ok)
	}
	// The process query failing: skip the sample, don't switch to machine-wide.
	broken := filepath.Join(dir, "broken-smi")
	os.WriteFile(broken, []byte("#!/bin/sh\ncase \"$1\" in --query-gpu*) printf 'GPU-a, 90, 1024\\n' ;; *) exit 9 ;; esac\n"), 0o755)
	if _, _, ok := (nvidia{bin: broken}).sample(context.Background(), []int{42}); ok {
		t.Fatal("mixed machine-wide numbers into the job's series")
	}
}

func TestParseStat(t *testing.T) {
	// A real line, with a command name holding a space and a parenthesis.
	line := "4242 (python (worker) 1) S 4200 4242 4242 0 -1 4194560 1234 0 0 0 250 50 0 0 20 0 9 0 12345 987654321 2048 18446744073709551615 1 1 0 0 0 0 0 16781312 16386 0 0 0 17 3 0 0 0 0 0"
	p, ok := parseStat(4242, line, 4096)
	if !ok || p.ppid != 4200 || p.cpuSec != 3 || p.rss != 2048*4096 {
		t.Fatalf("parsed %+v %v", p, ok)
	}
	if _, ok := parseStat(1, "1 (init) S 0", 4096); ok {
		t.Fatal("short line accepted")
	}
}
