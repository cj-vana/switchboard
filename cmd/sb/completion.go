package main

// sb completion <shell> writes a self-contained zsh, bash, or fish script.
// Its grammar is intentionally bounded: static commands, actions, and enums
// complete without opening config, sessions, inventories, or providers.

import (
	"fmt"
	"io"
	"strings"
)

// completionSubcommands is every top-level command accepted before session
// assembly. Help and completion tests pin this list to commandHelp.
var completionSubcommands = []string{
	"auth", "update", "doctor", "cost", "find", "stats", "races", "blame",
	"mistakes", "ladder", "recap", "export", "plugins", "mcp", "completion", "help",
}

// completionFlags is every flag the main flag set registers. -h and --help
// are generated separately because the standard flag package supplies them.
var completionFlags = []string{
	"-model", "-tier", "-host", "-mode", "-sandbox", "-think", "-workspace", "-p",
	"-output", "-resume", "-continue", "-sessions", "-tiers", "-repl",
	"-version", "-allow-secrets", "-profile", "-workflow",
}

// completionActions covers subcommands with a second command word. Selector
// completion remains intentionally absent: discovering it would open native
// inventories and, for Codex MCP, may launch an app-server.
var completionActions = map[string][]string{
	"plugins": {"list", "inspect", "install", "enable", "disable", "trust", "untrust"},
	"mcp":     {"list", "inspect", "enable", "disable"},
}

// completionArguments are other closed positional sets that need no runtime
// discovery. Session IDs, tiers, profiles, and selectors stay out because
// completing them would require opening mutable user state or native tools.
var completionArguments = map[string][]string{
	"auth":       {"status", "login", "logout", "oauth"},
	"auth/oauth": {"login", "logout"},
	"completion": {"zsh", "bash", "fish"},
	"find":       {"all"},
	"stats":      {"all"},
	"races":      {"all"},
}

// completionFlagValues are the closed enums known without reading config.
var completionFlagValues = map[string][]string{
	"-mode":    {"plan", "default", "acceptEdits", "auto", "yolo", "bypass"},
	"-sandbox": {"off", "on", "auto"},
	"-output":  {"text", "json"},
	"-think":   {"low", "medium", "high", "max"},
}

// completionValueFlags consume the following shell word. -sandbox is omitted:
// it is bool-like (`-sandbox` means on), with explicit values written as
// -sandbox=off|on|auto.
var completionValueFlags = []string{
	"-model", "-tier", "-host", "-mode", "-think", "-workspace", "-p", "-output", "-resume", "-profile",
	"-workflow",
}

func runCompletionCLI(w io.Writer, shell string) error {
	switch shell {
	case "zsh":
		writeZshCompletion(w)
	case "bash":
		writeBashCompletion(w)
	case "fish":
		writeFishCompletion(w)
	default:
		return fmt.Errorf("sb completion takes zsh, bash, or fish, not %q", shell)
	}
	return nil
}

func writeZshCompletion(w io.Writer) {
	sessionFlags := dashedFlagNames(subcommandSessionFlags)
	sessionValueFlags := commonWords(completionValueFlags, sessionFlags)
	sessionSwitchFlags := wordsExcept(sessionFlags, sessionValueFlags)
	contextValueFlags := wordsExcept(completionValueFlags, sessionValueFlags)
	fmt.Fprintf(w, `#compdef sb
# Switchboard shell completion. Install:
#   sb completion zsh > "${fpath[1]}/_sb"   # or anywhere in $fpath
_sb() {
  local -a subcmds flags help_flags value_flags plugin_actions mcp_actions
  local -a auth_actions oauth_actions completion_shells all_argument
  local -a mode_values sandbox_values output_values think_values
  local subcommand="" action="" word
  local -i subcommand_index=0 expect_value=0 invalid_root=0 blocked_command=0 end_options=0 terminal=0 i
  subcmds=(%s)
  flags=(%s)
  help_flags=('-h' '--help')
  value_flags=(%s)
  plugin_actions=(%s)
  mcp_actions=(%s)
  auth_actions=(%s)
  oauth_actions=(%s)
  completion_shells=(%s)
  all_argument=('all')
  mode_values=(%s)
  sandbox_values=(%s)
  output_values=(%s)
  think_values=(%s)

  for (( i = 2; i < CURRENT; i++ )); do
    word=$words[i]
    if (( expect_value )); then
      expect_value=0
      continue
    fi
    if (( end_options )); then
      case $word in
        %s)
          (( blocked_command )) && invalid_root=1 || { subcommand=$word; subcommand_index=$i; }
          ;;
        *) invalid_root=1 ;;
      esac
      break
    fi
    case $word in
      --) end_options=1 ;;
      -h|--help|-help|-h=*|--help=*|-help=*) terminal=1; break ;;
      %s) blocked_command=1; expect_value=1 ;;
      %s) expect_value=1 ;;
      %s) blocked_command=1 ;;
      %s) blocked_command=1 ;;
      -version|--version|-version=1|--version=1|-version=t|--version=t|-version=T|--version=T|-version=true|--version=true|-version=True|--version=True|-version=TRUE|--version=TRUE) terminal=1; break ;;
      -version=0|--version=0|-version=f|--version=f|-version=F|--version=F|-version=false|--version=false|-version=False|--version=False|-version=FALSE|--version=FALSE) ;;
      %s) ;;
      %s)
        (( blocked_command )) && invalid_root=1 || { subcommand=$word; subcommand_index=$i; }
        break ;;
      -*) invalid_root=1; break ;;
      *) invalid_root=1; break ;;
    esac
  done

  if [[ -z $subcommand ]]; then
    (( invalid_root || terminal )) && return
    if (( ! end_options )); then
      case $words[CURRENT] in
        -mode=*|--mode=*) compset -P '*='; _describe 'permission mode' mode_values; return ;;
        -sandbox=*|--sandbox=*) compset -P '*='; _describe 'sandbox mode' sandbox_values; return ;;
        -output=*|--output=*) compset -P '*='; _describe 'output format' output_values; return ;;
        -think=*|--think=*) compset -P '*='; _describe 'reasoning effort' think_values; return ;;
      esac
      if (( CURRENT > 2 )); then
        case $words[CURRENT-1] in
          -mode|--mode) _describe 'permission mode' mode_values; return ;;
          -output|--output) _describe 'output format' output_values; return ;;
          -think|--think) _describe 'reasoning effort' think_values; return ;;
        esac
      fi
    fi
    (( expect_value )) && return
    (( blocked_command )) || _describe 'subcommand' subcmds
    if (( ! end_options )); then
      _describe 'global flag' flags
      _describe 'help flag' help_flags
    fi
    return
  fi

  case $subcommand in
    plugins|mcp)
      if (( CURRENT == subcommand_index + 1 )); then
        if [[ $subcommand == plugins ]]; then
          _describe 'plugin action' plugin_actions
        else
          _describe 'MCP action' mcp_actions
        fi
        _describe 'help' help_flags
        compadd help
        return
      fi
      action=$words[subcommand_index+1]
      if [[ $action == help ]]; then
        if (( CURRENT == subcommand_index + 2 )); then
          if [[ $subcommand == plugins ]]; then
            _describe 'plugin action' plugin_actions
          else
            _describe 'MCP action' mcp_actions
          fi
          _describe 'help' help_flags
          return
        fi
        if (( CURRENT == subcommand_index + 3 )); then
          case $subcommand/$words[subcommand_index+2] in
            %s|%s) _describe 'help' help_flags ;;
          esac
        fi
        return
      fi
      case $subcommand/$action in
        %s|%s) _describe 'help' help_flags ;;
      esac
      ;;
    auth)
      if (( CURRENT == subcommand_index + 1 )); then
        _describe 'auth action' auth_actions
        _describe 'help' help_flags
        return
      fi
      action=$words[subcommand_index+1]
      if [[ $action == oauth ]] && (( CURRENT == subcommand_index + 2 )); then
        _describe 'OAuth action' oauth_actions
        return
      fi
      _describe 'help' help_flags
      ;;
    completion)
      if (( CURRENT == subcommand_index + 1 )); then
        _describe 'shell' completion_shells
        _describe 'help' help_flags
        return
      fi
      _describe 'help' help_flags
      ;;
    find|stats|races)
      if (( CURRENT == subcommand_index + 1 )); then
        _describe 'scope' all_argument
        _describe 'help' help_flags
        return
      fi
      _describe 'help' help_flags
      ;;
    help)
      if (( CURRENT == subcommand_index + 1 )); then
        _describe 'help topic' subcmds
        return
      fi
      action=$words[subcommand_index+1]
      case $action in
        plugins|mcp)
          if (( CURRENT == subcommand_index + 2 )); then
            [[ $action == plugins ]] && _describe 'plugin action' plugin_actions || _describe 'MCP action' mcp_actions
            _describe 'help' help_flags
            return
          fi
          if (( CURRENT == subcommand_index + 3 )); then
            case $action/$words[subcommand_index+2] in
              %s|%s) _describe 'help' help_flags ;;
            esac
          fi
          return
          ;;
        %s)
          (( CURRENT == subcommand_index + 2 )) && _describe 'help' help_flags
          return
          ;;
        esac
      ;;
    *)
      _describe 'help' help_flags
      ;;
  esac
}
_sb "$@"
`,
		zshWords(completionSubcommands), zshWords(completionFlags), zshWords(completionValueFlags),
		zshWords(completionActions["plugins"]), zshWords(completionActions["mcp"]),
		zshWords(completionArguments["auth"]), zshWords(completionArguments["auth/oauth"]),
		zshWords(completionArguments["completion"]),
		zshWords(completionFlagValues["-mode"]), zshWords(completionFlagValues["-sandbox"]),
		zshWords(completionFlagValues["-output"]), zshWords(completionFlagValues["-think"]),
		pipeJoin(completionSubcommands),
		doubleDashPatterns(sessionValueFlags), doubleDashPatterns(contextValueFlags),
		doubleDashPatterns(sessionSwitchFlags), doubleDashAssignmentPatterns(sessionFlags),
		doubleDashAssignmentPatterns(contextValueFlags), pipeJoin(completionSubcommands),
		commandActionPatterns("plugins", completionActions["plugins"]), commandActionPatterns("mcp", completionActions["mcp"]),
		commandActionPatterns("plugins", completionActions["plugins"]), commandActionPatterns("mcp", completionActions["mcp"]),
		commandActionPatterns("plugins", completionActions["plugins"]), commandActionPatterns("mcp", completionActions["mcp"]),
		pipeJoin(wordsExcept(completionSubcommands, []string{"plugins", "mcp"})))
}

func writeBashCompletion(w io.Writer) {
	sessionFlags := dashedFlagNames(subcommandSessionFlags)
	sessionValueFlags := commonWords(completionValueFlags, sessionFlags)
	sessionSwitchFlags := wordsExcept(sessionFlags, sessionValueFlags)
	contextValueFlags := wordsExcept(completionValueFlags, sessionValueFlags)
	fmt.Fprintf(w, `# Switchboard shell completion. Install:
#   sb completion bash >> ~/.bashrc   # or to a file sourced by it
_sb_complete() {
  local cur word subcommand="" action="" expect_value="" prefix value
  local subcommand_index=0 invalid_root=0 blocked_command=0 end_options=0 terminal=0 i
  local subcommands=%s
  local global_flags=%s
  local help_flags='-h --help'
  local plugin_actions=%s
  local mcp_actions=%s
  local auth_actions=%s
  local oauth_actions=%s
  local completion_shells=%s
  local mode_values=%s
  local sandbox_values=%s
  local output_values=%s
  local think_values=%s
  cur="${COMP_WORDS[COMP_CWORD]}"

  for ((i=1; i<COMP_CWORD; i++)); do
    word="${COMP_WORDS[i]}"
    if [ -n "$expect_value" ]; then
      expect_value=""
      continue
    fi
    if [ "$end_options" -eq 1 ]; then
      case "$word" in
        %s)
          if [ "$blocked_command" -eq 1 ]; then invalid_root=1; else subcommand="$word"; subcommand_index=$i; fi ;;
        *) invalid_root=1 ;;
      esac
      break
    fi
    case "$word" in
      --) end_options=1 ;;
      -h|--help|-help|-h=*|--help=*|-help=*) terminal=1; break ;;
      %s) blocked_command=1; expect_value=1 ;;
      %s) expect_value=1 ;;
      %s) blocked_command=1 ;;
      %s) blocked_command=1 ;;
      -version|--version|-version=1|--version=1|-version=t|--version=t|-version=T|--version=T|-version=true|--version=true|-version=True|--version=True|-version=TRUE|--version=TRUE) terminal=1; break ;;
      -version=0|--version=0|-version=f|--version=f|-version=F|--version=F|-version=false|--version=false|-version=False|--version=False|-version=FALSE|--version=FALSE) ;;
      %s) ;;
      %s)
        if [ "$blocked_command" -eq 1 ]; then invalid_root=1; else subcommand="$word"; subcommand_index=$i; fi
        break ;;
      -*) invalid_root=1; break ;;
      *) invalid_root=1; break ;;
    esac
  done

  if [ -z "$subcommand" ]; then
    { [ "$invalid_root" -eq 1 ] || [ "$terminal" -eq 1 ]; } && return
    if [ "$end_options" -eq 0 ]; then
      case "$cur" in
        -mode=*|--mode=*)
          prefix="${cur%%%%=*}="; value="${cur#*=}"
          COMPREPLY=($(compgen -W "$mode_values" -- "$value"))
          for i in "${!COMPREPLY[@]}"; do COMPREPLY[$i]="${prefix}${COMPREPLY[$i]}"; done
          return ;;
        -sandbox=*|--sandbox=*)
          prefix="${cur%%%%=*}="; value="${cur#*=}"
          COMPREPLY=($(compgen -W "$sandbox_values" -- "$value"))
          for i in "${!COMPREPLY[@]}"; do COMPREPLY[$i]="${prefix}${COMPREPLY[$i]}"; done
          return ;;
        -output=*|--output=*)
          prefix="${cur%%%%=*}="; value="${cur#*=}"
          COMPREPLY=($(compgen -W "$output_values" -- "$value"))
          for i in "${!COMPREPLY[@]}"; do COMPREPLY[$i]="${prefix}${COMPREPLY[$i]}"; done
          return ;;
        -think=*|--think=*)
          prefix="${cur%%%%=*}="; value="${cur#*=}"
          COMPREPLY=($(compgen -W "$think_values" -- "$value"))
          for i in "${!COMPREPLY[@]}"; do COMPREPLY[$i]="${prefix}${COMPREPLY[$i]}"; done
          return ;;
      esac
      if [ "$COMP_CWORD" -gt 1 ]; then
        case "${COMP_WORDS[COMP_CWORD-1]}" in
          -mode|--mode) COMPREPLY=($(compgen -W "$mode_values" -- "$cur")); return ;;
          -output|--output) COMPREPLY=($(compgen -W "$output_values" -- "$cur")); return ;;
          -think|--think) COMPREPLY=($(compgen -W "$think_values" -- "$cur")); return ;;
        esac
      fi
    fi
    [ -n "$expect_value" ] && return
    if [ "$end_options" -eq 1 ]; then
      COMPREPLY=($(compgen -W "$subcommands" -- "$cur"))
    elif [ "$blocked_command" -eq 1 ]; then
      COMPREPLY=($(compgen -W "$global_flags $help_flags" -- "$cur"))
    else
      COMPREPLY=($(compgen -W "$subcommands $global_flags $help_flags" -- "$cur"))
    fi
    return
  fi

  case "$subcommand" in
    plugins|mcp)
      if [ "$COMP_CWORD" -eq $((subcommand_index+1)) ]; then
        if [ "$subcommand" = plugins ]; then action="$plugin_actions"; else action="$mcp_actions"; fi
        COMPREPLY=($(compgen -W "$action help $help_flags" -- "$cur"))
        return
      fi
      action="${COMP_WORDS[subcommand_index+1]}"
      if [ "$action" = help ]; then
        if [ "$COMP_CWORD" -eq $((subcommand_index+2)) ]; then
          if [ "$subcommand" = plugins ]; then action="$plugin_actions"; else action="$mcp_actions"; fi
          COMPREPLY=($(compgen -W "$action $help_flags" -- "$cur"))
          return
        fi
        if [ "$COMP_CWORD" -eq $((subcommand_index+3)) ]; then
          case "$subcommand/${COMP_WORDS[subcommand_index+2]}" in
            %s|%s) COMPREPLY=($(compgen -W "$help_flags" -- "$cur")) ;;
          esac
        fi
        return
      fi
      case "$subcommand/$action" in
        %s|%s) COMPREPLY=($(compgen -W "$help_flags" -- "$cur")) ;;
      esac
      ;;
    auth)
      if [ "$COMP_CWORD" -eq $((subcommand_index+1)) ]; then
        COMPREPLY=($(compgen -W "$auth_actions $help_flags" -- "$cur"))
        return
      fi
      action="${COMP_WORDS[subcommand_index+1]}"
      if [ "$action" = oauth ] && [ "$COMP_CWORD" -eq $((subcommand_index+2)) ]; then
        COMPREPLY=($(compgen -W "$oauth_actions" -- "$cur"))
        return
      fi
      COMPREPLY=($(compgen -W "$help_flags" -- "$cur"))
      ;;
    completion)
      if [ "$COMP_CWORD" -eq $((subcommand_index+1)) ]; then
        COMPREPLY=($(compgen -W "$completion_shells $help_flags" -- "$cur"))
        return
      fi
      COMPREPLY=($(compgen -W "$help_flags" -- "$cur"))
      ;;
    find|stats|races)
      if [ "$COMP_CWORD" -eq $((subcommand_index+1)) ]; then
        COMPREPLY=($(compgen -W "all $help_flags" -- "$cur"))
        return
      fi
      COMPREPLY=($(compgen -W "$help_flags" -- "$cur"))
      ;;
    help)
      if [ "$COMP_CWORD" -eq $((subcommand_index+1)) ]; then
        COMPREPLY=($(compgen -W "$subcommands" -- "$cur"))
        return
      fi
      action="${COMP_WORDS[subcommand_index+1]}"
      case "$action" in
        plugins|mcp)
          if [ "$COMP_CWORD" -eq $((subcommand_index+2)) ]; then
            if [ "$action" = plugins ]; then action="$plugin_actions"; else action="$mcp_actions"; fi
            COMPREPLY=($(compgen -W "$action $help_flags" -- "$cur"))
            return
          fi
          if [ "$COMP_CWORD" -eq $((subcommand_index+3)) ]; then
            case "${COMP_WORDS[subcommand_index+1]}/${COMP_WORDS[subcommand_index+2]}" in
              %s|%s) COMPREPLY=($(compgen -W "$help_flags" -- "$cur")) ;;
            esac
          fi
          return
          ;;
        %s)
          if [ "$COMP_CWORD" -eq $((subcommand_index+2)) ]; then
            COMPREPLY=($(compgen -W "$help_flags" -- "$cur"))
          fi
          return
          ;;
        esac
      ;;
    *)
      COMPREPLY=($(compgen -W "$help_flags" -- "$cur"))
      ;;
  esac
}
complete -F _sb_complete sb
`, shellSingleQuote(spaceJoin(completionSubcommands)),
		shellSingleQuote(spaceJoin(completionFlags)),
		shellSingleQuote(spaceJoin(completionActions["plugins"])),
		shellSingleQuote(spaceJoin(completionActions["mcp"])),
		shellSingleQuote(spaceJoin(completionArguments["auth"])),
		shellSingleQuote(spaceJoin(completionArguments["auth/oauth"])),
		shellSingleQuote(spaceJoin(completionArguments["completion"])),
		shellSingleQuote(spaceJoin(completionFlagValues["-mode"])),
		shellSingleQuote(spaceJoin(completionFlagValues["-sandbox"])),
		shellSingleQuote(spaceJoin(completionFlagValues["-output"])),
		shellSingleQuote(spaceJoin(completionFlagValues["-think"])),
		pipeJoin(completionSubcommands),
		doubleDashPatterns(sessionValueFlags), doubleDashPatterns(contextValueFlags),
		doubleDashPatterns(sessionSwitchFlags), doubleDashAssignmentPatterns(sessionFlags),
		doubleDashAssignmentPatterns(contextValueFlags), pipeJoin(completionSubcommands),
		commandActionPatterns("plugins", completionActions["plugins"]), commandActionPatterns("mcp", completionActions["mcp"]),
		commandActionPatterns("plugins", completionActions["plugins"]), commandActionPatterns("mcp", completionActions["mcp"]),
		commandActionPatterns("plugins", completionActions["plugins"]), commandActionPatterns("mcp", completionActions["mcp"]),
		pipeJoin(wordsExcept(completionSubcommands, []string{"plugins", "mcp"})))
}

func writeFishCompletion(w io.Writer) {
	subcommands := spaceJoin(completionSubcommands)
	sessionFlags := dashedFlagNames(subcommandSessionFlags)
	sessionValueFlags := commonWords(completionValueFlags, sessionFlags)
	sessionSwitchFlags := wordsExcept(sessionFlags, sessionValueFlags)
	contextValueFlags := wordsExcept(completionValueFlags, sessionValueFlags)
	fmt.Fprintf(w, `# Switchboard shell completion. Install:
#   sb completion fish > ~/.config/fish/completions/sb.fish
function __sb_command
    set -l tokens (commandline -opc)
    if test (count $tokens) -gt 0
        set -e tokens[1]
    end
    set -l expect_value 0
    set -l blocked_command 0
    set -l end_options 0
    set -l command_name ''
    set -l command_tail
    for token in $tokens
        if test -n "$command_name"
            set -a command_tail $token
            continue
        end
        if test $expect_value -eq 1
            set expect_value 0
            continue
        end
        if test $end_options -eq 1
            switch $token
                case %s
                    test $blocked_command -eq 1; and return 3
                    set command_name $token
                case '*'
                    return 3
            end
            continue
        end
        switch $token
            case '--'
                set end_options 1
            case '-h' '--help' '-help' '-h=*' '--help=*' '-help=*'
                return 5
            case %s
                set blocked_command 1
                set expect_value 1
            case %s
                set expect_value 1
            case %s
                set blocked_command 1
            case %s
                set blocked_command 1
            case '-version' '--version' '-version=1' '--version=1' '-version=t' '--version=t' '-version=T' '--version=T' '-version=true' '--version=true' '-version=True' '--version=True' '-version=TRUE' '--version=TRUE'
                return 5
            case '-version=0' '--version=0' '-version=f' '--version=f' '-version=F' '--version=F' '-version=false' '--version=false' '-version=False' '--version=False' '-version=FALSE' '--version=FALSE'
                continue
            case %s
                continue
            case %s
                test $blocked_command -eq 1; and return 3
                set command_name $token
            case '-*'
                return 3
            case '*'
                return 3
        end
    end
    if test -n "$command_name"
        printf '%%s\n' $command_name $command_tail
        return 0
    end
    if test $expect_value -eq 1
        return 2
    end
    if test $blocked_command -eq 1
        test $end_options -eq 1; and return 5
        return 4
    end
    if test $end_options -eq 1
        return 6
    end
    return 1
end

function __sb_root_flags
    __sb_command >/dev/null
    set -l scan_status $status
    test $scan_status -eq 1; or test $scan_status -eq 4
end

function __sb_root_commands
    __sb_command >/dev/null
    set -l scan_status $status
    test $scan_status -eq 1; or test $scan_status -eq 6
end

function __sb_has_command
    __sb_command >/dev/null
    test $status -eq 0
end

function __sb_awaiting_value -a expected
    __sb_command >/dev/null
    test $status -eq 2; or return 1
    set -l tokens (commandline -opc)
    test (count $tokens) -gt 0; or return 1
    set -l previous $tokens[-1]
    test "$previous" = "-$expected"; or test "$previous" = "--$expected"
end

function __sb_needs_argument -a expected
    __sb_has_command; or return 1
    set -l tail (__sb_command)
    test (count $tail) -eq 1; and test "$tail[1]" = "$expected"
end

function __sb_needs_nested_action -a expected
    __sb_has_command; or return 1
    set -l tail (__sb_command)
    test (count $tail) -eq 2; or return 1
    test "$tail[1]" = "$expected"; and test "$tail[2]" = help; and return 0
    test "$tail[1]" = help; and test "$tail[2]" = "$expected"
end

function __sb_auth_needs_oauth_action
    __sb_has_command; or return 1
    set -l tail (__sb_command)
    test (count $tail) -eq 2; and test "$tail[1]" = auth; and test "$tail[2]" = oauth
end

function __sb_needs_help_topic
    __sb_needs_argument help
end

function __sb_accepts_help_flag
    __sb_has_command; or return 1
    set -l tail (__sb_command)
    set -l count (count $tail)
    switch $tail[1]
        case plugins
            test $count -eq 1; and return 0
            switch $tail[2]
                case %s
                    return 0
                case help
                    test $count -eq 2; and return 0
                    if test $count -eq 3
                        switch $tail[3]
                            case %s
                                return 0
                        end
                    end
            end
        case mcp
            test $count -eq 1; and return 0
            switch $tail[2]
                case %s
                    return 0
                case help
                    test $count -eq 2; and return 0
                    if test $count -eq 3
                        switch $tail[3]
                            case %s
                                return 0
                        end
                    end
            end
        case help
            test $count -eq 1; and return 0
            test $count -ge 2; or return 1
            switch $tail[2]
                case plugins
                    test $count -eq 2; and return 0
                    test $count -eq 3; or return 1
                    switch $tail[3]
                        case %s
                            return 0
                    end
                case mcp
                    test $count -eq 2; and return 0
                    test $count -eq 3; or return 1
                    switch $tail[3]
                        case %s
                            return 0
                    end
                case %s
                    test $count -eq 2; and return 0
            end
        case %s
            return 0
    end
    return 1
end

complete -c sb -f
complete -c sb -n '__sb_root_commands' -a %s
complete -c sb -n '__sb_root_flags' -s h -l help -d 'Show help without opening a session'
`, fishCaseWords(completionSubcommands),
		fishCaseWords(doubleDashFlagWords(sessionValueFlags)), fishCaseWords(doubleDashFlagWords(contextValueFlags)),
		fishCaseWords(doubleDashFlagWords(sessionSwitchFlags)), fishCaseWords(doubleDashAssignmentWords(sessionFlags)),
		fishCaseWords(doubleDashAssignmentWords(contextValueFlags)), fishCaseWords(completionSubcommands),
		fishCaseWords(completionActions["plugins"]), fishCaseWords(completionActions["plugins"]),
		fishCaseWords(completionActions["mcp"]), fishCaseWords(completionActions["mcp"]),
		fishCaseWords(completionActions["plugins"]), fishCaseWords(completionActions["mcp"]),
		fishCaseWords(wordsExcept(completionSubcommands, []string{"plugins", "mcp"})),
		fishCaseWords(wordsExcept(completionSubcommands, []string{"plugins", "mcp", "help"})),
		fishSingleQuote(subcommands))

	valueFlags := stringSet(completionValueFlags)
	for _, flag := range completionFlags {
		name := strings.TrimPrefix(flag, "-")
		condition := "__sb_root_flags"
		if values, ok := completionFlagValues[flag]; ok {
			if flag == "-sandbox" {
				fmt.Fprintf(w, "complete -c sb -n %s -o %s\n", fishSingleQuote(condition), fishSingleQuote(name))
				continue
			}
			condition += "; or __sb_awaiting_value " + name
			fmt.Fprintf(w, "complete -c sb -n %s -o %s -r -a %s\n",
				fishSingleQuote(condition), fishSingleQuote(name), fishSingleQuote(spaceJoin(values)))
			continue
		}
		line := fmt.Sprintf("complete -c sb -n %s -o %s", fishSingleQuote(condition), fishSingleQuote(name))
		if valueFlags[flag] {
			line += " -r"
		}
		fmt.Fprintln(w, line)
	}
	sandboxEquals := make([]string, 0, len(completionFlagValues["-sandbox"]))
	for _, value := range completionFlagValues["-sandbox"] {
		sandboxEquals = append(sandboxEquals, "-sandbox="+value)
	}
	fmt.Fprintf(w, "complete -c sb -n %s -a %s\n",
		fishSingleQuote(`__sb_root_flags; and string match -q -- "-sandbox=*" (commandline -ct)`),
		fishSingleQuote(spaceJoin(sandboxEquals)))

	fmt.Fprintf(w, "complete -c sb -n '__sb_accepts_help_flag' -s h -l help -d %s\n", fishSingleQuote("Show scoped command help"))
	for _, command := range []string{"plugins", "mcp"} {
		actions := spaceJoin(completionActions[command])
		fmt.Fprintf(w, "complete -c sb -n %s -a %s\n", fishSingleQuote("__sb_needs_argument "+command), fishSingleQuote(actions+" help"))
		fmt.Fprintf(w, "complete -c sb -n %s -a %s\n", fishSingleQuote("__sb_needs_nested_action "+command), fishSingleQuote(actions))
	}
	fmt.Fprintf(w, "complete -c sb -n '__sb_needs_argument auth' -a %s\n", fishSingleQuote(spaceJoin(completionArguments["auth"])))
	fmt.Fprintf(w, "complete -c sb -n '__sb_auth_needs_oauth_action' -a %s\n", fishSingleQuote(spaceJoin(completionArguments["auth/oauth"])))
	fmt.Fprintf(w, "complete -c sb -n '__sb_needs_argument completion' -a %s\n", fishSingleQuote(spaceJoin(completionArguments["completion"])))
	for _, command := range []string{"find", "stats", "races"} {
		fmt.Fprintf(w, "complete -c sb -n %s -a all\n", fishSingleQuote("__sb_needs_argument "+command))
	}
	helpTopics := spaceJoin(withoutWord(completionSubcommands, "help"))
	fmt.Fprintf(w, "complete -c sb -n '__sb_needs_help_topic' -a %s\n", fishSingleQuote(helpTopics))
}

func zshWords(words []string) string {
	quoted := make([]string, 0, len(words))
	for _, word := range words {
		quoted = append(quoted, shellSingleQuote(word))
	}
	return strings.Join(quoted, " ")
}

func shellSingleQuote(word string) string {
	return "'" + strings.ReplaceAll(word, "'", `'"'"'`) + "'"
}

func fishSingleQuote(word string) string {
	word = strings.ReplaceAll(word, `\`, `\\`)
	word = strings.ReplaceAll(word, `'`, `\'`)
	return "'" + word + "'"
}

func fishCaseWords(words []string) string {
	quoted := make([]string, 0, len(words))
	for _, word := range words {
		quoted = append(quoted, fishSingleQuote(word))
	}
	return strings.Join(quoted, " ")
}

func doubleDashFlagWords(flags []string) []string {
	words := make([]string, 0, len(flags)*2)
	for _, flag := range flags {
		words = append(words, flag, "-"+flag)
	}
	return words
}

func doubleDashAssignmentWords(flags []string) []string {
	words := make([]string, 0, len(flags)*2)
	for _, flag := range flags {
		words = append(words, flag+"=*", "-"+flag+"=*")
	}
	return words
}

func withoutWord(words []string, excluded string) []string {
	out := make([]string, 0, len(words))
	for _, word := range words {
		if word != excluded {
			out = append(out, word)
		}
	}
	return out
}

func doubleDashPatterns(flags []string) string {
	patterns := make([]string, 0, len(flags)*2)
	for _, flag := range flags {
		patterns = append(patterns, flag, "-"+flag)
	}
	return strings.Join(patterns, "|")
}

func doubleDashAssignmentPatterns(flags []string) string {
	patterns := make([]string, 0, len(flags)*2)
	for _, flag := range flags {
		patterns = append(patterns, flag+"=*", "-"+flag+"=*")
	}
	return strings.Join(patterns, "|")
}

func dashedFlagNames(names []string) []string {
	flags := make([]string, 0, len(names))
	for _, name := range names {
		flags = append(flags, "-"+name)
	}
	return flags
}

func commonWords(words, allowed []string) []string {
	set := stringSet(allowed)
	out := make([]string, 0, len(words))
	for _, word := range words {
		if set[word] {
			out = append(out, word)
		}
	}
	return out
}

func wordsExcept(words, excluded []string) []string {
	set := stringSet(excluded)
	out := make([]string, 0, len(words))
	for _, word := range words {
		if !set[word] {
			out = append(out, word)
		}
	}
	return out
}

func pipeJoin(words []string) string {
	return strings.Join(words, "|")
}

func commandActionPatterns(command string, actions []string) string {
	patterns := make([]string, 0, len(actions))
	for _, action := range actions {
		patterns = append(patterns, command+"/"+action)
	}
	return strings.Join(patterns, "|")
}

func spaceJoin(words []string) string {
	return strings.Join(words, " ")
}

func stringSet(words []string) map[string]bool {
	set := make(map[string]bool, len(words))
	for _, word := range words {
		set[word] = true
	}
	return set
}
