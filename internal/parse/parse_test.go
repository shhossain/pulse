package parse

import (
	"reflect"
	"strings"
	"testing"
)

func TestLine(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want map[string]any // nil = no match
	}{
		{"empty", "   ", nil},
		{"plain text", "Loading model weights...", nil},
		{"marker", `::pulse:: {"loss": 0.5}`, map[string]any{"loss": 0.5}},
		{"json line", `{"epoch": 3, "loss": 0.25}`, map[string]any{"epoch": float64(3), "loss": 0.25}},
		{"tqdm", "train:  45%|████▌     | 450/1000 [01:02<01:15,  7.20it/s, loss=0.43]",
			map[string]any{"progress.train": []any{450.0, 1000.0}, "loss": "0.43", "_coalesce": true}},
		{"tqdm no desc", "100%|██████████| 10/10 [00:01<00:00,  9.80it/s]",
			map[string]any{"progress.main": []any{10.0, 10.0}, "_coalesce": true}},
		{"tqdm lightning", "Epoch 3:  12%|█▏        | 1.20k/10.0k [00:10<01:00, 120it/s, v_num=0, train_loss=1.23]",
			map[string]any{"progress.batch": []any{1200.0, 10000.0}, "v_num": "0", "train_loss": "1.23", "_coalesce": true}},
		{"keyword epoch", "Epoch 3/10 - loss=0.431 val_loss=0.52",
			map[string]any{"progress.epoch": []any{3.0, 10.0}, "loss": "0.431", "val_loss": "0.52"}},
		{"step of", "step 120 of 1000",
			map[string]any{"progress.step": []any{120.0, 1000.0}, "step": 120.0}},
		{"bracket", "[3/10] compiling foo.c", map[string]any{"progress.main": []any{3.0, 10.0}}},
		{"colon needs space", "listening on localhost:8080", nil},
		{"clock is not a metric", "12:30:01 started", nil},
		{"colon metric", "accuracy: 0.91, lr: 3e-4", map[string]any{"accuracy": "0.91", "lr": "3e-4"}},
		{"nan", "loss=nan", map[string]any{"loss": "nan"}},
		{"url query ignored", "GET /x?a=1&b=2", nil},
		{"ansi", "\x1b[32mloss=0.1\x1b[0m", map[string]any{"loss": "0.1"}},
		{"ratio bigger than total", "epoch 12/10", nil},
		{"json log keeps numbers only", `{"level":"info","api_key":"sk-1","latency_ms":12,"done":"3/10","msg":"hi"}`,
			map[string]any{"latency_ms": 12.0, "done": "3/10"}},
		{"json log with no numbers", `{"level":"info","msg":"started"}`, nil},
		{"hf trainer dict", `{'loss': 1.2345, 'grad_norm': 0.5, 'learning_rate': 4.9e-05, 'epoch': 0.1}`,
			map[string]any{"loss": "1.2345", "grad_norm": "0.5", "learning_rate": "4.9e-05", "epoch": "0.1"}},
		{"braced key=value", "{loss=0.43, acc=0.9}", map[string]any{"loss": "0.43", "acc": "0.9"}},
		{"json with NaN", `{"loss": NaN, "step": 10}`, map[string]any{"loss": "NaN", "step": "10"}},
		{"marker with NaN", `::pulse:: {"loss": NaN}`, map[string]any{"loss": "NaN"}},
		{"tqdm with epoch ratio", "Epoch 3/10: 45%|████▌ | 450/1000 [01:02<01:15, 7.20it/s, loss=0.43]",
			map[string]any{"progress.batch": []any{450.0, 1000.0}, "progress.epoch": []any{3.0, 10.0}, "loss": "0.43", "_coalesce": true}},
		{"tqdm epoch loop", "Epoch: 30%|███ | 3/10 [00:10<00:20, 1.2s/it]",
			map[string]any{"progress.epoch": []any{3.0, 10.0}, "_coalesce": true}},
		{"sentence period", "Final loss=0.43.", map[string]any{"loss": "0.43"}},
		{"infinity", "loss=infinity", map[string]any{"loss": "infinity"}},
		{"absurd ratio", "epoch " + strings.Repeat("9", 400) + "/" + strings.Repeat("9", 401), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Line(tt.in)
			if tt.want == nil {
				if ok {
					t.Fatalf("Line(%q) = %v, want no match", tt.in, got)
				}
				return
			}
			if !ok || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Line(%q)\n got %#v\nwant %#v", tt.in, got, tt.want)
			}
		})
	}
}

func TestSplitterCarriageReturns(t *testing.T) {
	var lines []string
	s := &Splitter{Emit: func(l string) { lines = append(lines, l) }}
	_, _ = s.Write([]byte(" 10%|█| 1/10\r 20%|██| 2/10\r"))
	_, _ = s.Write([]byte(" 30%|███| 3/10\r\ndone\r\npartial"))
	s.Flush()
	want := []string{" 10%|█| 1/10", " 20%|██| 2/10", " 30%|███| 3/10", "done", "partial"}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q want %q", lines, want)
	}
}
