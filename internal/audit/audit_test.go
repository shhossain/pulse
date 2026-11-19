package audit

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	for in, leak := range map[string]string{
		"curl -H 'Authorization: Bearer abc123def' https://x":      "abc123def",
		"deploy --token s3cr3t --env prod":                         "s3cr3t",
		"PASSWORD=hunter2 ./run.sh":                                "hunter2",
		"export PULSE_TOKEN=pls_0123456789abcdef":                  "pls_0123456789abcdef",
		"openai --key sk-ABCDEFGHIJKLMNOP":                         "sk-ABCDEFGHIJKLMNOP",
		"git push https://ghp_ABCDEFGHIJKLMNOPQRSTUVWX@github.com": "ghp_ABCDEFGHIJKLMNOPQRSTUVWX",
	} {
		if out := Redact(in); strings.Contains(out, leak) {
			t.Errorf("Redact(%q) = %q", in, out)
		}
	}
	if Redact("ssh gpu1 nvidia-smi") != "ssh gpu1 nvidia-smi" {
		t.Error("ordinary command changed")
	}
}
