package layout

import (
	"reflect"
	"strings"
	"testing"

	"pulse/internal/config"
	"pulse/internal/model"
)

func TestStem(t *testing.T) {
	for in, want := range map[string]string{
		"loss": "loss", "val_loss": "loss", "train_loss": "loss", "loss_val": "loss",
		"train/loss": "loss", "eval.accuracy": "accuracy", "metrics/val/acc": "acc",
		"lr": "lr", "gpu_util": "gpu_util", "train": "train",
	} {
		if got := Stem(in); got != want {
			t.Errorf("Stem(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCharts(t *testing.T) {
	m := func(name string, n int) model.MetricItem { return model.MetricItem{Name: name, N: n} }
	got := Charts([]model.MetricItem{
		m("lr", 10), m("acc", 10), m("loss", 10), m("val_loss", 6), m("seed", 1), m("val_acc", 5), m("val_f1", 9),
	})
	want := []model.Chart{
		{Name: "loss", Keys: []string{"loss", "val_loss"}},
		{Name: "lr", Keys: []string{"lr"}},
		{Name: "acc", Keys: []string{"acc", "val_acc"}},
		{Name: "val_f1", Keys: []string{"val_f1"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestChartsForConfig(t *testing.T) {
	m := func(name string, n int) model.MetricItem { return model.MetricItem{Name: name, N: n} }
	metrics := []model.MetricItem{m("loss", 10), m("val_loss", 10), m("lr", 10), m("seed", 10), m("gpu_util", 2), m("acc", 10)}
	got := ChartsFor(metrics, config.Display{
		Charts:  []config.ChartSpec{{Y: []string{"gpu_util"}}, {Y: []string{"loss", "val_loss", "missing"}}},
		Ignore:  []string{"seed"},
		Primary: "acc",
	})
	var names []string
	for _, c := range got {
		names = append(names, c.Name)
	}
	// Explicit charts (even with few points), then inferred; primary first; seed ignored.
	if strings.Join(names, ",") != "acc,gpu_util,loss,lr" {
		t.Fatalf("charts = %v", names)
	}
	order := OrderProgress([]model.ProgressItem{{Name: "epoch"}, {Name: "batch"}, {Name: "download"}}, []string{"download", "epoch"})
	if order[0].Name != "download" || order[1].Name != "epoch" || order[2].Name != "batch" {
		t.Fatalf("order = %+v", order)
	}
}

func TestOverflowChartsAreCappedAndLossLeads(t *testing.T) {
	var ms []model.MetricItem
	for _, k := range []string{"a_acc", "b_acc", "c_acc", "d_acc", "e_acc", "f_acc", "g_acc", "h_acc", "i_acc", "Val_Loss"} {
		ms = append(ms, model.MetricItem{Name: k, N: 10})
	}
	for _, c := range Charts(ms) {
		if len(c.Keys) > MaxSeries {
			t.Fatalf("%s has %d series", c.Name, len(c.Keys))
		}
	}
	if Charts(ms)[0].Name != "Val_Loss" {
		t.Fatalf("first chart = %s", Charts(ms)[0].Name)
	}
}
