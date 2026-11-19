package sysstat

import (
	"context"
	"strconv"
	"strings"
)

// macOS has no /proc; ps reads the same kernel data without privileges.
// Its %cpu is a short decaying average, which is what we want here.
func processTree(ctx context.Context, root int) []proc {
	out := output(ctx, "ps", "-A", "-o", "pid=,ppid=,rss=,%cpu=")
	var all []proc
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 4 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		rss, err3 := strconv.ParseUint(f[2], 10, 64)
		pcpu, err4 := strconv.ParseFloat(strings.ReplaceAll(f[3], ",", "."), 64)
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
			continue
		}
		all = append(all, proc{pid: pid, ppid: ppid, rss: rss * 1024, cpuSec: -1, pcpu: pcpu})
	}
	return descendants(all, root)
}
