package sysstat

import (
	"context"
	"os"
	"strconv"
)

// /proc/<pid>/stat: CPU ticks are exact, unlike ps's lifetime-average %cpu.
func processTree(_ context.Context, root int) []proc {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	page := uint64(os.Getpagesize())
	var all []proc
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		if p, ok := parseStat(pid, string(b), page); ok {
			all = append(all, p)
		}
	}
	return descendants(all, root)
}
