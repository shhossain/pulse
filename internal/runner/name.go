package runner

import (
	"path/filepath"
	"regexp"
	"strings"
)

// launchers are skipped when inferring a job name from a command line.
var launchers = map[string]bool{
	"python": true, "python3": true, "python2": true, "pypy": true, "pypy3": true,
	"node": true, "deno": true, "bun": true, "ruby": true, "perl": true, "php": true,
	"bash": true, "sh": true, "zsh": true, "fish": true, "rscript": true, "julia": true,
	"uv": true, "uvx": true, "poetry": true, "pipenv": true, "conda": true, "mamba": true, "pixi": true,
	"accelerate": true, "torchrun": true, "deepspeed": true, "mpirun": true, "srun": true,
	"npx": true, "env": true, "nohup": true, "time": true, "caffeinate": true, "sudo": true,
	"run": true, "exec": true, "launch": true,
}

var wordRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,30}$`)

var scriptExt = map[string]bool{
	".py": true, ".sh": true, ".bash": true, ".zsh": true, ".js": true, ".mjs": true, ".ts": true,
	".rb": true, ".pl": true, ".jl": true, ".r": true, ".lua": true, ".php": true, ".ipynb": true,
}

// InferName derives a job name from a command line:
// "python train.py" → "train", "python -m pkg.train" → "train",
// "make deploy" → "make-deploy", "./scripts/backup.sh" → "backup".
func InferName(args []string) string {
	if len(args) == 0 {
		return "job"
	}
	// After a launcher, the first script file wins: launchers take flags
	// with separate values (torchrun --nproc_per_node 4 train.py, python -W
	// ignore train.py), and a value must never become the name. (Not for
	// other tools: "git add main.py" is git-add, not main.)
	prog := strings.ToLower(filepath.Base(args[0]))
	for _, a := range args[1:] {
		if !launchers[strings.TrimSuffix(prog, ".exe")] && !isVersionedLauncher(prog) {
			break
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		if ext := strings.ToLower(filepath.Ext(a)); scriptExt[ext] {
			return clean(strings.TrimSuffix(filepath.Base(a), filepath.Ext(a)))
		}
	}
	i := 0
scan:
	for i < len(args) {
		a := args[i]
		base := strings.ToLower(filepath.Base(a))
		switch {
		case a == "-m" && i+1 < len(args): // python -m pkg.mod
			mod := args[i+1]
			if j := strings.LastIndex(mod, "."); j >= 0 {
				mod = mod[j+1:]
			}
			return clean(mod)
		case strings.HasPrefix(a, "-"):
			i++
			continue
		case strings.Contains(a, "=") && !strings.Contains(a, "/"): // env-style VAR=x
			i++
			continue
		case launchers[strings.TrimSuffix(base, ".exe")] || isVersionedLauncher(base):
			i++
			continue
		}
		break scan
	}
	if i >= len(args) {
		return clean(filepath.Base(args[0]))
	}
	first := args[i]
	base := filepath.Base(first)
	if ext := filepath.Ext(base); ext != "" || strings.Contains(first, "/") {
		return clean(strings.TrimSuffix(base, ext))
	}
	// A plain tool: add its subcommand if it looks like one ("make deploy").
	if i+1 < len(args) && wordRe.MatchString(args[i+1]) {
		return clean(base + "-" + args[i+1])
	}
	return clean(base)
}

var versionedLauncherRe = regexp.MustCompile(`^(python|pypy|node)\d+(\.\d+)*$`)

func isVersionedLauncher(base string) bool { return versionedLauncherRe.MatchString(base) }

func clean(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "job"
	}
	return s
}
