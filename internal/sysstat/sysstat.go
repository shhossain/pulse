// Package sysstat samples what a job uses of the machine: CPU and memory of
// its process tree, GPU load and memory, and free disk space where it runs.
// Everything is best effort: a source that isn't there (no NVIDIA driver, a
// sandbox without /proc) is skipped, never an error. Pure Go, no cgo.
package sysstat

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Metric keys reported for a run. The UI groups the "sys." namespace as
// "System", with names and units of its own.
const (
	KeyCPU      = "sys.cpu"       // % of one core, summed over the process tree (200 = two cores)
	KeyMem      = "sys.mem"       // GB resident, summed over the process tree
	KeyGPU      = "sys.gpu"       // % busy (mean over the GPUs the job uses)
	KeyGPUMem   = "sys.gpu_mem"   // GB of GPU memory in use
	KeyDiskFree = "sys.disk_free" // GB free on the volume of the job's folder
)

// Sampler samples one process tree. It isn't safe for concurrent use.
type Sampler struct {
	root int
	dir  string
	cpu  cpuMeter
	gpu  gpuSource
}

// New samples the tree rooted at pid, and the disk holding dir.
func New(pid int, dir string) *Sampler {
	return &Sampler{root: pid, dir: dir, gpu: findGPU()}
}

// Sample returns the current values (keys absent when unknown).
func (s *Sampler) Sample(ctx context.Context) map[string]any {
	out := map[string]any{}
	procs := processTree(ctx, s.root)
	if len(procs) > 0 {
		var rss uint64
		pids := make([]int, 0, len(procs))
		for _, p := range procs {
			rss += p.rss
			pids = append(pids, p.pid)
		}
		out[KeyMem] = round(float64(rss)/1e9, 2)
		if cpu, ok := s.cpu.percent(procs, time.Now()); ok {
			out[KeyCPU] = round(cpu, 1)
		}
		if s.gpu != nil {
			if util, mem, ok := s.gpu.sample(ctx, pids); ok {
				out[KeyGPU] = round(util, 1)
				if mem >= 0 {
					out[KeyGPUMem] = round(mem/1e9, 2)
				}
			}
		}
	}
	if free, ok := diskFree(s.dir); ok {
		out[KeyDiskFree] = round(float64(free)/1e9, 1)
	}
	return out
}

// Watch samples every interval until ctx ends, handing each sample to emit.
// The first sample waits one interval, so CPU has a span to measure over.
func (s *Sampler) Watch(ctx context.Context, every time.Duration, emit func(map[string]any)) {
	t := time.NewTicker(every)
	defer t.Stop()
	s.cpu.percent(processTree(ctx, s.root), time.Now()) // baseline
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Most of the interval: nvidia-smi can take a second or more on a busy host.
			sctx, cancel := context.WithTimeout(ctx, every*9/10)
			m := s.Sample(sctx)
			cancel()
			if ctx.Err() == nil && len(m) > 0 {
				emit(m)
			}
		}
	}
}

type proc struct {
	pid, ppid int
	rss       uint64  // bytes
	cpuSec    float64 // total CPU seconds so far (-1: unknown, use pcpu)
	pcpu      float64 // the OS's own recent CPU %, where cpuSec isn't available
}

// cpuMeter turns CPU-seconds totals into a percentage between two samples.
type cpuMeter struct {
	last   map[int]float64
	lastAt time.Time
}

func (m *cpuMeter) percent(procs []proc, now time.Time) (float64, bool) {
	cur := make(map[int]float64, len(procs))
	var sum, direct float64
	usePcpu := false
	for _, p := range procs {
		if p.cpuSec < 0 {
			usePcpu = true
			direct += p.pcpu
			continue
		}
		cur[p.pid] = p.cpuSec
		if prev, ok := m.last[p.pid]; ok && p.cpuSec >= prev {
			sum += p.cpuSec - prev
		} else if !m.lastAt.IsZero() {
			sum += p.cpuSec // started since the last sample
		}
	}
	defer func() { m.last, m.lastAt = cur, now }()
	if usePcpu {
		return direct, true
	}
	if m.lastAt.IsZero() {
		return 0, false
	}
	dt := now.Sub(m.lastAt).Seconds()
	if dt <= 0 {
		return 0, false
	}
	return 100 * sum / dt, true
}

// descendants keeps root and everything under it.
func descendants(all []proc, root int) []proc {
	kids := map[int][]int{}
	byPid := map[int]proc{}
	for _, p := range all {
		kids[p.ppid] = append(kids[p.ppid], p.pid)
		byPid[p.pid] = p
	}
	var out []proc
	seen := map[int]bool{}
	stack := []int{root}
	for len(stack) > 0 {
		pid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		if p, ok := byPid[pid]; ok {
			out = append(out, p)
		}
		stack = append(stack, kids[pid]...)
	}
	return out
}

func round(v float64, places int) float64 {
	p := 1.0
	for range places {
		p *= 10
	}
	if v < 0 {
		return -float64(int64(-v*p+0.5)) / p
	}
	return float64(int64(v*p+0.5)) / p
}

// output runs a command with ctx and returns its stdout ("" on any error).
func output(ctx context.Context, name string, args ...string) string {
	s, _ := run(ctx, name, args...)
	return s
}

// run is output that also says whether the command succeeded.
func run(ctx context.Context, name string, args ...string) (string, bool) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = time.Second
	b, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return string(b), true
}

const clkTck = 100 // Linux USER_HZ; 100 on every mainstream build

// parseStat reads one /proc/<pid>/stat line (Linux).
func parseStat(pid int, s string, page uint64) (proc, bool) {
	// The command name is in parentheses and may hold spaces or ")": split
	// after the last one.
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return proc{}, false
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 22 {
		return proc{}, false
	}
	// After ")": state(0) ppid(1) … utime(11) stime(12) … rss(21) in pages.
	ppid, err1 := strconv.Atoi(f[1])
	ut, err2 := strconv.ParseFloat(f[11], 64)
	st, err3 := strconv.ParseFloat(f[12], 64)
	rss, err4 := strconv.ParseUint(f[21], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		return proc{}, false
	}
	return proc{pid: pid, ppid: ppid, rss: rss * page, cpuSec: (ut + st) / clkTck}, true
}
