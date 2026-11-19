package model

import "testing"

func TestProgressOrderOuterFirst(t *testing.T) {
	r := &Run{}
	r.SetProgress("batch", Progress{1, 20})
	r.SetProgress("download", Progress{1, 2})
	r.SetProgress("epoch", Progress{1, 10})
	r.SetProgress("batch", Progress{2, 20})
	var names []string
	for _, p := range r.Progress {
		names = append(names, p.Name)
	}
	if got := names; len(got) != 3 || got[0] != "epoch" || got[1] != "download" || got[2] != "batch" {
		t.Fatalf("order = %v", got)
	}
	if r.Progress[2].Cur != 2 {
		t.Fatalf("batch not updated: %+v", r.Progress[2])
	}
}
