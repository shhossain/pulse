package main

import (
	"fmt"
	"os"
	"path/filepath"

	"pulse/skills"
)

// cmdSkill prints the coding-agent skill, or installs it for Claude Code
// (user-wide, or into a project with --project).
func cmdSkill(args []string) int {
	if len(args) == 0 {
		os.Stdout.Write(skills.Pulse)
		return 0
	}
	if args[0] != "install" {
		return printHelp("skill") + 2
	}
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".claude", "skills", "pulse")
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--project":
			cwd, _ := os.Getwd()
			dir = filepath.Join(cwd, ".claude", "skills", "pulse")
		case "--dir":
			if i+1 >= len(args) {
				return printHelp("skill") + 2
			}
			i++
			dir = args[i]
		default:
			return printHelp("skill") + 2
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fail(err)
	}
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, skills.Pulse, 0o644); err != nil {
		return fail(err)
	}
	fmt.Printf("Installed the Pulse skill at %s\n", path)
	return 0
}
