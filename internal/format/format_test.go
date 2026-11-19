package format

import "testing"

func TestLabel(t *testing.T) {
	for in, want := range map[string]string{
		"val_loss": "Val loss", "gpu_util": "GPU util", "clip_pct": "Clip %", "lr": "Learning rate",
		"gb": "GB", "train/loss": "Train loss", "valLoss": "Val loss", "main": "Progress", "f1_score": "F1 score",
		"hours": "Hours", "terminate-vm": "Terminate VM", "aws-console": "AWS console",
	} {
		if got := Label(in); got != want {
			t.Errorf("Label(%q) = %q, want %q", in, got, want)
		}
	}
}
