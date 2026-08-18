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
var completionSubcommands = []string{"auth", "update", "doctor", "cost", "find", "stats", "races", "blame", "mistakes", "ladder", "recap", "export", "plugins", "mcp", "completion"}

// completionFlags is every flag the main flag set registers.
var completionFlags = []string{
	"-model", "-tier", "-host", "-mode", "-sandbox", "-think", "-workspace", "-p",
	"-output", "-resume", "-continue", "-sessions", "-tiers", "-repl",
	"-version", "-allow-secrets", "-profile",
}

// completionActions covers subcommands with their own action grammar. Keep
// these lists next to the generated scripts so a second-tab completion never
// falls back to unrelated global flags.
var completionActions = map[string][]string{
	"plugins": {"list", "inspect", "install", "enable", "disable", "trust", "untrust"},
	"mcp":     {"list", "inspect", "enable", "disable"},
}

func runCompletionCLI(w io.Writer, shell string) error {
	switch shell {
	case "zsh":
		fmt.Fprintf(w, `#compdef sb
# Switchboard shell completion. Install:
#   sb completion zsh > "${fpath[1]}/_sb"   # or anywhere in $fpath
_sb() {
  local -a subcmds flags plugin_actions mcp_actions
  subcmds=(%s)
  flags=(%s)
  plugin_actions=(%s)
  mcp_actions=(%s)
  if (( CURRENT == 2 )); then
    _describe 'subcommand' subcmds
    _values 'flag' $flags
  elif (( CURRENT == 3 )); then
    case $words[2] in
      plugins) _describe 'plugin action' plugin_actions ;;
      mcp) _describe 'MCP action' mcp_actions ;;
      *) return 1 ;;
    esac
  else
    return 1
  fi
}
_sb "$@"
`, zshWords(completionSubcommands), zshWords(completionFlags), zshWords(completionActions["plugins"]), zshWords(completionActions["mcp"]))
	case "bash":
		fmt.Fprintf(w, `# Switchboard shell completion. Install:
#   sb completion bash >> ~/.bashrc   # or to a file sourced by it
_sb_complete() {
  local cur="${COMP_WORDS[COMP_CWORD]}"
  if [ "$COMP_CWORD" -eq 1 ]; then
    COMPREPLY=($(compgen -W "%s %s" -- "$cur"))
  elif [ "$COMP_CWORD" -eq 2 ] && [ "${COMP_WORDS[1]}" = "plugins" ]; then
    COMPREPLY=($(compgen -W "%s" -- "$cur"))
  elif [ "$COMP_CWORD" -eq 2 ] && [ "${COMP_WORDS[1]}" = "mcp" ]; then
    COMPREPLY=($(compgen -W "%s" -- "$cur"))
  else
    COMPREPLY=()
  fi
}
complete -F _sb_complete sb
`, spaceJoin(completionSubcommands), spaceJoin(completionFlags), spaceJoin(completionActions["plugins"]), spaceJoin(completionActions["mcp"]))
	case "fish":
		fmt.Fprintf(w, `# Switchboard shell completion. Install:
#   sb completion fish > ~/.config/fish/completions/sb.fish
complete -c sb -f
complete -c sb -n __fish_use_subcommand -a "%s"
`, spaceJoin(completionSubcommands))
		for _, f := range completionFlags {
			fmt.Fprintf(w, "complete -c sb -n 'not __fish_seen_subcommand_from %s' -o %s\n", spaceJoin(completionSubcommands), f[1:])
		}
		fmt.Fprintf(w, "complete -c sb -n '__fish_seen_subcommand_from plugins; and not __fish_seen_subcommand_from %s' -a '%s'\n",
			spaceJoin(completionActions["plugins"]), spaceJoin(completionActions["plugins"]))
		fmt.Fprintf(w, "complete -c sb -n '__fish_seen_subcommand_from mcp; and not __fish_seen_subcommand_from %s' -a '%s'\n",
			spaceJoin(completionActions["mcp"]), spaceJoin(completionActions["mcp"]))
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
