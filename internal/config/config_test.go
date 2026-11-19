package config

import (
	"encoding/json"
	"strings"
	"testing"
)

const sample = `
name: train-gpt
run: python train.py --cfg big.yaml
link: https://wandb.ai/me/runs/abc
widgets:
  progress: [epoch, batch]
  charts:
    - [loss, val_loss]
    - [lr]
  stats: [gpu_util]
  primary: loss
  ignore: [seed]
alerts:
  - val_loss new min
  - loss is nan: { run: stop, level: error }
actions:
  gpu: { shell: ssh gpu1 nvidia-smi, output: true }
  checkpoint: { send: save_checkpoint, when: running }
  wandb: { open: "https://wandb.ai/me/runs/abc" }
`

func TestParseSingleJob(t *testing.T) {
	f, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Jobs) != 1 {
		t.Fatalf("jobs = %d", len(f.Jobs))
	}
	j := f.Jobs[0]
	if j.Name != "train-gpt" || j.Run != "python train.py --cfg big.yaml" || j.Link == "" {
		t.Fatalf("job = %+v", j)
	}
	if len(j.Widgets.Charts) != 2 || j.Widgets.Primary != "loss" || j.Widgets.Ignore[0] != "seed" {
		t.Fatalf("widgets = %+v", j.Widgets)
	}
	if len(j.Alerts) != 2 || j.Alerts[1].Expr != "loss is nan" || j.Alerts[1].Run != "stop" || j.Alerts[1].Level != "error" {
		t.Fatalf("alerts = %+v", j.Alerts)
	}
	if len(j.ActionList) != 3 || j.ActionList[0].Name != "checkpoint" || j.ActionList[1].Kind() != "shell" || !j.ActionList[1].Output {
		t.Fatalf("actions = %+v", j.ActionList)
	}
	if !HasShell(j.ActionList) {
		t.Fatal("expected shell actions")
	}
}

func TestParseJobsList(t *testing.T) {
	f, err := Parse([]byte("jobs:\n  - name: deploy\n    alerts: [no update for 10m]\n  - name: backup\n"))
	if err != nil || len(f.Jobs) != 2 || f.Jobs[1].Name != "backup" || f.Jobs[0].Alerts[0].Expr != "no update for 10m" {
		t.Fatalf("f=%+v err=%v", f, err)
	}
	if _, err := Parse([]byte("")); err != nil {
		t.Fatalf("empty file: %v", err)
	}
}

func TestParseErrors(t *testing.T) {
	for in, want := range map[string]string{
		"nmae: x":                                       "field nmae not found",
		"actions:\n  x: { shell: a, send: b }":          "exactly one of",
		"actions:\n  x: { open: 'file:///etc' }":        "http(s) URL",
		"actions:\n  'bad name': { shell: ls }":         "action name",
		"jobs:\n  - run: x\n  - name: y":                "needs a name",
		"link: javascript:alert(1)":                     "link must be",
		"alerts:\n  - { a: 1, b: 2 }":                   "optionally followed by options",
		"actions:\n  x: { shell: ls, when: sometimes }": "when must be",
	} {
		_, err := Parse([]byte(in))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Parse(%q) error = %v, want %q", in, err, want)
		}
	}
}

func TestShellHashChangesWithCommands(t *testing.T) {
	a := []Action{{Name: "gpu", Shell: "nvidia-smi"}, {Name: "ck", Send: "save"}}
	b := []Action{{Name: "gpu", Shell: "rm -rf ~"}, {Name: "ck", Send: "save"}}
	c := []Action{{Name: "gpu", Shell: "nvidia-smi"}, {Name: "ck", Send: "other"}}
	if ShellHash("/x", a) == ShellHash("/x", b) {
		t.Fatal("changing a shell command must change the hash")
	}
	if ShellHash("/x", a) != ShellHash("/x", c) {
		t.Fatal("non-shell changes don't need new trust")
	}
	if ShellHash("/x", a) == ShellHash("/y", a) {
		t.Fatal("a different folder needs new trust")
	}
}

func TestJobsListRejectsTopLevelFields(t *testing.T) {
	_, err := Parse([]byte("widgets: {primary: loss}\njobs:\n  - name: a\n"))
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("err = %v", err)
	}
}

func TestChartForms(t *testing.T) {
	f, err := Parse([]byte(`
name: t
widgets:
  charts:
    - [loss, val_loss]
    - acc
    - { y: lr, style: points }
    - { x: lr, y: [loss, val_loss], style: both, name: Loss by LR }
`))
	if err != nil {
		t.Fatal(err)
	}
	c := f.Jobs[0].Widgets.Charts
	if len(c) != 4 || len(c[0].Y) != 2 || c[1].Y[0] != "acc" || c[2].Style != "points" || c[3].X != "lr" || len(c[3].Y) != 2 || c[3].Name != "Loss by LR" {
		t.Fatalf("charts = %+v", c)
	}
	for _, bad := range []string{
		"widgets: { charts: [ { y: a, style: dots } ] }",
		"widgets: { charts: [ { x: a, y: a } ] }",
		"widgets: { charts: [ { x: a } ] }",
		"widgets: { charts: [ { y: { a: 1 } } ] }",
		"widgets: { charts: [ { y: a, colour: red } ] }",
	} {
		if _, err := Parse([]byte("name: t\n" + bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	var spec ChartSpec
	if err := json.Unmarshal([]byte(`["a","b"]`), &spec); err != nil || len(spec.Y) != 2 {
		t.Fatalf("json list: %+v %v", spec, err)
	}
}

func TestChartSpecJSONAndRuneLimits(t *testing.T) {
	var spec ChartSpec
	if err := json.Unmarshal([]byte(`{"y":"loss","x":"lr"}`), &spec); err != nil || len(spec.Y) != 1 || spec.X != "lr" {
		t.Fatalf("y as a string: %+v %v", spec, err)
	}
	if (ChartSpec{Y: []string{"a"}, Name: strings.Repeat("é", 31)}).Validate() != nil {
		t.Fatal("31 accented characters counted as bytes")
	}
	if _, err := Parse([]byte("name: t\nwidgets: { labels: { a: \"\" } }")); err == nil {
		t.Fatal("empty file label accepted")
	}
}
