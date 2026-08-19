package config

// [[permissions]]: the answers the user has already given, written down.
//
// The engine has always taken rules; only MCP allow lists ever supplied any,
// so every answer a human gave died with the session and the same command was
// asked about again on the next run. These are the same `permission.Rule`
// values the engine already matches, read from the user's own file, which is
// what makes them legitimate: a rule here has the authority `~/.switchboard/
// hooks.toml` has, the user's standing policy, and it grants the same reach a
// typed "yes" grants — unconfined where no sandbox is configured, network
// included. Nothing here is a sandbox and nothing here pretends to be.
//
// A repository-provided form is deliberately absent. A checkout must not
// pre-approve a command by being opened, which is the same reason the
// repository has no /watch declaration, and a trust-gated form is a bigger
// design than this file should invent on its own.
//
// The refusals below all say one thing: a rule is a narrower answer than a
// mode, and anything that reaches as wide as a mode should be typed as one.

import (
	"fmt"
	"strings"

	"github.com/switchboard-code/switchboard/internal/permission"
)

// permissionEntry is one rule as written. Field names are the request's own
// vocabulary so a rule reads like the prompt it replaces.
type permissionEntry struct {
	Decision   string   `toml:"decision"`
	Tool       string   `toml:"tool,omitempty"`
	Effect     string   `toml:"effect,omitempty"`
	Path       string   `toml:"path,omitempty"`
	ArgvPrefix []string `toml:"argv_prefix,omitempty"`

	// Shell is a pointer so "absent" and "explicitly false" stay different
	// facts: a rule that names shell = false is about argv calls only, and one
	// that omits it covers both.
	Shell *bool `toml:"shell,omitempty"`
}

func buildPermissionRules(entries []permissionEntry, path string) ([]permission.Rule, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	rules := make([]permission.Rule, 0, len(entries))
	for i, entry := range entries {
		rule, err := entry.rule()
		if err != nil {
			// One-based, because the reader is counting blocks in a file and
			// not indexing an array.
			return nil, fmt.Errorf("%s: permission rule %d: %w", path, i+1, err)
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

func (e permissionEntry) rule() (permission.Rule, error) {
	decision, err := parseDecision(e.Decision)
	if err != nil {
		return permission.Rule{}, err
	}
	effect, err := parseEffect(e.Effect)
	if err != nil {
		return permission.Rule{}, err
	}

	rule := permission.Rule{
		Decision:   decision,
		Tool:       e.Tool,
		Effect:     effect,
		PathGlob:   e.Path,
		ArgvPrefix: e.ArgvPrefix,
		Shell:      e.Shell,
	}
	if decision == permission.Allow {
		if err := checkAllowIsNarrowerThanAMode(rule); err != nil {
			return permission.Rule{}, err
		}
	}
	return rule, nil
}

// checkAllowIsNarrowerThanAMode refuses the shapes that are a permission mode
// wearing a rule's clothes.
//
// An allow that names nothing approves everything, which is yolo; an allow whose
// only constraint is an effect approves every write, every command, or every
// external tool, which is the same claim held one step narrower. Both are
// available as modes, typed on purpose, visible in the status bar, and revocable
// by typing another one. A line in a config file is none of those things.
//
// Reads are exempt because the modes already allow them: a read rule can only
// restate the default or, as a deny, tighten it.
func checkAllowIsNarrowerThanAMode(rule permission.Rule) error {
	named := rule.Tool != "" || rule.PathGlob != "" || len(rule.ArgvPrefix) > 0
	if named {
		return nil
	}
	switch rule.Effect {
	case "":
		return fmt.Errorf("an allow rule that names no tool, path, or argv prefix approves every request; that is what /mode yolo is for")
	case permission.EffectRead:
		return nil
	case permission.EffectExternal:
		// The sharpest one. An external tool is a process this program started
		// unconfined, acting wherever it acts, and no mode auto-allows one —
		// bypass included. A blanket grant here would be wider than every mode.
		return fmt.Errorf("an allow rule for every external tool is wider than any mode grants; name the tool it is for")
	default:
		return fmt.Errorf("an allow rule whose only constraint is effect = %q approves every one of them; name a tool, path, or argv prefix, or use the matching /mode", rule.Effect)
	}
}

func parseDecision(s string) (permission.Decision, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "allow":
		return permission.Allow, nil
	case "deny":
		return permission.Deny, nil
	case "ask":
		return permission.Ask, nil
	case "":
		return "", fmt.Errorf("decision is required; want allow, deny, or ask")
	default:
		return "", fmt.Errorf("decision %q is not allow, deny, or ask", s)
	}
}

func parseEffect(s string) (permission.Effect, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return "", nil
	case "read":
		return permission.EffectRead, nil
	case "write":
		return permission.EffectWrite, nil
	case "execute":
		return permission.EffectExecute, nil
	case "external":
		return permission.EffectExternal, nil
	default:
		return "", fmt.Errorf("effect %q is not read, write, execute, or external", s)
	}
}

// RenderPermissionRule is how a rule says what it covers, in one line, in the
// vocabulary it was written in. /permissions prints it and the dry-run names
// the rule it matched with it, so a user reading either sees the same sentence.
func RenderPermissionRule(rule permission.Rule) string {
	var parts []string
	if rule.Tool != "" {
		parts = append(parts, "tool "+rule.Tool)
	}
	if rule.Effect != "" {
		parts = append(parts, "effect "+string(rule.Effect))
	}
	if rule.PathGlob != "" {
		parts = append(parts, "path "+rule.PathGlob)
	}
	if len(rule.ArgvPrefix) > 0 {
		parts = append(parts, "argv "+strings.Join(rule.ArgvPrefix, " "))
	}
	if rule.Shell != nil {
		if *rule.Shell {
			parts = append(parts, "shell only")
		} else {
			parts = append(parts, "argv only")
		}
	}
	if len(parts) == 0 {
		return string(rule.Decision) + " everything"
	}
	return string(rule.Decision) + " " + strings.Join(parts, ", ")
}

// permissionEntryFor is rule() run backwards, so a config this program wrote
// loads back into the rules it was written from. The pair is the save
// contract; the round-trip test is what holds it.
func permissionEntryFor(rule permission.Rule) permissionEntry {
	return permissionEntry{
		Decision:   string(rule.Decision),
		Tool:       rule.Tool,
		Effect:     string(rule.Effect),
		Path:       rule.PathGlob,
		ArgvPrefix: rule.ArgvPrefix,
		Shell:      rule.Shell,
	}
}
