package runner

import "testing"

func TestInferName(t *testing.T) {
	cases := map[string][]string{
		"train":       {"python", "train.py", "--lr", "3e-4"},
		"finetune":    {"python3.11", "-u", "scripts/finetune.py"},
		"main":        {"uv", "run", "python", "-m", "pkg.main"},
		"train2":      {"torchrun", "--nproc_per_node=8", "train2.py"},
		"make-deploy": {"make", "deploy"},
		"backup":      {"./scripts/backup.sh"},
		"sleep":       {"sleep", "30"},
		"cargo-build": {"cargo", "build", "--release"},
		"server":      {"CUDA_VISIBLE_DEVICES=0", "node", "server.js"},
		"python":      {"python"},
		"train3":      {"torchrun", "--nproc_per_node", "4", "train3.py"},
		"ft":          {"accelerate", "launch", "--num_processes", "2", "ft.py"},
		"mpi_job":     {"mpirun", "-np", "4", "python", "mpi_job.py"},
		"warn":        {"python", "-W", "ignore", "warn.py"},
		"pinned":      {"uv", "run", "--python", "3.12", "pinned.py"},
		"git-add":     {"git", "add", "main.py"},
		"pytest":      {"pytest", "tests/test_x.py"},
	}
	for want, args := range cases {
		if got := InferName(args); got != want {
			t.Errorf("InferName(%q) = %q, want %q", args, got, want)
		}
	}
}
