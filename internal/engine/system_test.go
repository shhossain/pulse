package engine

import (
	"strings"
	"testing"

	"pulse/internal/format"
	"pulse/internal/model"
)

func TestSystemMetrics(t *testing.T) {
	e, n, _ := newEngine(t)
	var evs []map[string]any
	for i := 1; i <= 6; i++ {
		evs = append(evs, map[string]any{"step": i, "loss": 1.0 / float64(i)})
		evs = append(evs, map[string]any{"sys.cpu": 180.5, "sys.mem": 3.2, "sys.disk_free": 40.0 - float64(i)*7})
	}
	e.ApplyBatch("r_1", model.Batch{Job: "train", Meta: runMeta(), Events: append([]map[string]any{{"status": "running"}}, evs...)})
	r, _ := e.Run("r_1")
	// The job's chart leads; system charts come after it, one each.
	if r.Charts[0].Name != "loss" || r.Charts[len(r.Charts)-1].Group != "system" {
		t.Fatalf("charts = %+v", r.Charts)
	}
	if got := format.Summary(&r); strings.Contains(got, "CPU") || !strings.Contains(got, "Loss") {
		t.Fatalf("summary should leave system metrics out: %q", got)
	}
	if format.LabelFor(&r, "sys.mem") != "Memory" || format.WithUnit(&r, "sys.mem", 3.2) != "3.2 GB" {
		t.Fatal("system names and units")
	}
	// Disk drops below 2 GB at the 6th sample: the built-in rule warns once.
	var disk []string
	for _, title := range n.titles() {
		if strings.Contains(title, "disk almost full") {
			disk = append(disk, title)
		}
	}
	if len(disk) != 1 {
		t.Fatalf("disk notices = %v (all: %v)", disk, n.titles())
	}
}
