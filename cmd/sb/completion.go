package main

// sb completion <shell>: the completion script for zsh or bash, written to
// stdout for the user's rc file to source. The lists are maintained beside
// the things they complete — a completion that offers a flag the binary
// refuses is worse than none — and completionSubcommands/completionFlags
// are asserted against the real dispatch in completion_test.go, so the
// script and the binary cannot drift apart silently.

import (
	"fmt"
	"io"
)

// completionSubcommands is every word main dispatches on before flags.
var completionSubcommands = []string{"auth", "update", "doctor", "cost", "find", "stats", "races", "completion"}

// completionFlags is every flag the main flag set registers.
var completionFlags = []string{
	"-model", "-tier", "-host", "-mode", "-think", "-workspace", "-p",
	"-output", "-resume", "-continue", "-sessions", "-tiers", "-repl",
	"-version", "-allow-secrets",
}

func runCompletionCLI(w io.Writer, shell string) error {
	switch shell {
	case "zsh":
		fmt.Fprintf(w, `#compdef sb
# Switchboard shell completion. Install:
#   sb completion zsh > "${fpath[1]}/_sb"   # or anywhere in $fpath
_sb() {
  local -a subcmds flags
  subcmds=(%s)
  flags=(%s)
  if (( CURRENT == 2 )); then
    _describe 'subcommand' subcmds
    _values 'flag' $flags
  else
    _values 'flag' $flags
  fi
}
_sb "$@"
`, zshWords(completionSubcommands), zshWords(completionFlags))
	case "bash":
		fmt.Fprintf(w, `# Switchboard shell completion. Install:
#   sb completion bash >> ~/.bashrc   # or to a file sourced by it
_sb_complete() {
  local cur="${COMP_WORDS[COMP_CWORD]}"
  if [ "$COMP_CWORD" -eq 1 ]; then
    COMPREPLY=($(compgen -W "%s %s" -- "$cur"))
  else
    COMPREPLY=($(compgen -W "%s" -- "$cur"))
  fi
}
complete -F _sb_complete sb
`, spaceJoin(completionSubcommands), spaceJoin(completionFlags), spaceJoin(completionFlags))
	case "fish":
		fmt.Fprintf(w, `# Switchboard shell completion. Install:
#   sb completion fish > ~/.config/fish/completions/sb.fish
complete -c sb -f
complete -c sb -n __fish_use_subcommand -a "%s"
`, spaceJoin(completionSubcommands))
		for _, f := range completionFlags {
			fmt.Fprintf(w, "complete -c sb -o %s\n", f[1:])
		}
	default:
		return fmt.Errorf("sb completion takes zsh, bash, or fish, not %q", shell)
	}
	return nil
}

func zshWords(words []string) string {
	out := ""
	for i, w := range words {
		if i > 0 {
			out += " "
		}
		out += "'" + w + "'"
	}
	return out
}

func spaceJoin(words []string) string {
	out := ""
	for i, w := range words {
		if i > 0 {
			out += " "
		}
		out += w
	}
	return out
}
