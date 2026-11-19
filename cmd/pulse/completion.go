package main

import (
	"fmt"
	"os"
	"strings"
)

var commands = []string{
	"run", "push", "watch", "ls", "show", "open", "stop", "rm", "alert", "mute", "action", "do",
	"apply", "trust", "ask", "inbox", "answer", "remote", "status", "doctor", "completion", "skill", "version", "help",
}

// Commands whose first argument is a job name.
var jobCommands = []string{"show", "open", "stop", "rm", "alert", "mute", "action", "do", "push"}

func cmdCompletion(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: pulse completion zsh|bash|fish")
		return 2
	}
	cmds, jobCmds := strings.Join(commands, " "), strings.Join(jobCommands, " ")
	switch args[0] {
	case "zsh":
		fmt.Printf(`#compdef pulse
# Add to ~/.zshrc:  source <(pulse completion zsh)
_pulse() {
  local -a cmds; cmds=(%s)
  if (( CURRENT == 2 )); then compadd -- $cmds; return; fi
  case ${words[2]} in
    %s) (( CURRENT == 3 )) && compadd -- ${(f)"$(pulse __jobs 2>/dev/null)"} ;;
    help) compadd -- $cmds ;;
    remote) (( CURRENT == 3 )) && compadd -- status enable disable token tokens revoke rotate ;;
    watch) (( CURRENT == 3 )) && compadd -- ls rm ;;
    completion) compadd -- zsh bash fish ;;
  esac
}
compdef _pulse pulse
`, cmds, strings.ReplaceAll(jobCmds, " ", "|"))
	case "bash":
		fmt.Printf(`# Add to ~/.bashrc:  eval "$(pulse completion bash)"
_pulse() {
  local cur=${COMP_WORDS[COMP_CWORD]} IFS=$'\n'   # job names may contain spaces
  if [ "$COMP_CWORD" -eq 1 ]; then IFS=' ' COMPREPLY=($(compgen -W "%s" -- "$cur")); return; fi
  case "${COMP_WORDS[1]}" in
    %s) [ "$COMP_CWORD" -eq 2 ] && COMPREPLY=($(compgen -W "$(pulse __jobs 2>/dev/null)" -- "$cur")) ;;
    help) IFS=' ' COMPREPLY=($(compgen -W "%s" -- "$cur")) ;;
    remote) IFS=' ' COMPREPLY=($(compgen -W "status enable disable token tokens revoke rotate" -- "$cur")) ;;
    completion) IFS=' ' COMPREPLY=($(compgen -W "zsh bash fish" -- "$cur")) ;;
  esac
}
complete -F _pulse pulse
`, cmds, strings.ReplaceAll(jobCmds, " ", "|"), cmds)
	case "fish":
		fmt.Printf("# Save as ~/.config/fish/completions/pulse.fish\n")
		fmt.Printf("complete -c pulse -f -n '__fish_use_subcommand' -a '%s'\n", cmds)
		fmt.Printf("complete -c pulse -f -n '__fish_seen_subcommand_from %s' -a '(pulse __jobs 2>/dev/null)'\n", jobCmds)
		fmt.Printf("complete -c pulse -f -n '__fish_seen_subcommand_from remote' -a 'status enable disable token tokens revoke rotate'\n")
	default:
		fmt.Fprintln(os.Stderr, "usage: pulse completion zsh|bash|fish")
		return 2
	}
	return 0
}
