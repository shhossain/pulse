package sysstat

import (
	"context"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// gpuSource reports GPU load (%) and memory in use (bytes, -1 if unknown).
type gpuSource interface {
	sample(ctx context.Context, pids []int) (util, mem float64, ok bool)
}

func findGPU() gpuSource {
	if p, err := exec.LookPath("nvidia-smi"); err == nil {
		return nvidia{bin: p}
	}
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		return appleGPU{}
	}
	return nil
}

// nvidia uses nvidia-smi. Memory is what the job's own processes hold when
// the driver can tell; load is the mean over the GPUs they're on (all GPUs
// when it can't tell, e.g. inside a container).
type nvidia struct{ bin string }

func (n nvidia) sample(ctx context.Context, pids []int) (float64, float64, bool) {
	// Both queries at once: each can take a second on a busy multi-GPU host.
	var apps string
	var appsOK bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		apps, appsOK = run(ctx, n.bin, "--query-compute-apps=gpu_uuid,pid,used_memory", "--format=csv,noheader,nounits")
	}()
	gpus := output(ctx, n.bin, "--query-gpu=uuid,utilization.gpu,memory.used", "--format=csv,noheader,nounits")
	<-done
	if !appsOK {
		// Without the process list we can't tell the job's share from the
		// machine's; skip this sample rather than mix the two in one series.
		return 0, 0, false
	}
	util := map[string]float64{}
	used := map[string]float64{}
	for _, line := range strings.Split(strings.TrimSpace(gpus), "\n") {
		f := splitCSV(line)
		if len(f) != 3 {
			continue
		}
		u, err1 := strconv.ParseFloat(f[1], 64)
		m, err2 := strconv.ParseFloat(f[2], 64)
		if err1 != nil || err2 != nil {
			continue // "[N/A]" on some boards
		}
		util[f[0]], used[f[0]] = u, m*(1<<20)
	}
	if len(util) == 0 {
		return 0, 0, false
	}
	mine := map[int]bool{}
	for _, p := range pids {
		mine[p] = true
	}
	onGPU := map[string]bool{}
	var mem float64
	others := false
	for _, line := range strings.Split(strings.TrimSpace(apps), "\n") {
		f := splitCSV(line)
		if len(f) != 3 {
			continue
		}
		pid, err := strconv.Atoi(f[1])
		if err != nil {
			continue
		}
		if !mine[pid] {
			others = true
			continue
		}
		onGPU[f[0]] = true
		if m, err := strconv.ParseFloat(f[2], 64); err == nil {
			mem += m * (1 << 20)
		}
	}
	var sum float64
	if len(onGPU) > 0 {
		for g := range onGPU {
			sum += util[g]
		}
		return sum / float64(len(onGPU)), mem, true
	}
	if others {
		// The driver lists processes, just none of the job's: it isn't on a
		// GPU (yet). Someone else's load isn't the job's.
		return 0, 0, false
	}
	// The driver lists no processes at all (inside a container it can't see
	// them, or the GPUs are idle): report the machine's GPUs.
	var all float64
	for g, u := range util {
		sum += u
		all += used[g]
	}
	return sum / float64(len(util)), all, true
}

func splitCSV(line string) []string {
	f := strings.Split(line, ",")
	for i := range f {
		f[i] = strings.TrimSpace(f[i])
	}
	return f
}

// appleGPU reads the Apple Silicon GPU's own statistics (whole machine:
// macOS doesn't attribute GPU time to processes without root).
type appleGPU struct{}

var (
	appleUtil = regexp.MustCompile(`"Device Utilization %"=(\d+)`)
	appleMem  = regexp.MustCompile(`"In use system memory"=(\d+)`)
)

func (appleGPU) sample(ctx context.Context, _ []int) (float64, float64, bool) {
	out := output(ctx, "ioreg", "-r", "-d", "1", "-w", "0", "-c", "IOAccelerator")
	m := appleUtil.FindStringSubmatch(out)
	if m == nil {
		return 0, 0, false
	}
	util, _ := strconv.ParseFloat(m[1], 64)
	mem := -1.0
	if mm := appleMem.FindStringSubmatch(out); mm != nil {
		mem, _ = strconv.ParseFloat(mm[1], 64)
	}
	return util, mem, true
}
