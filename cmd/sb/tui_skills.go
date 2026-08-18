package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/skills"
)

const skillUsage = "/skill <canonical-selector> [args]; /skills lists exact selectors"

// cmdSkills shows the complete inventory, not just the subset advertised to
// the model. Invocation state and provenance are part of the identity: a
// manual-only project workflow must not disappear merely because it is kept
// out of the frozen tool schema.
func cmdSkills(m *tuiModel, _ string) tea.Cmd {
	if len(m.app.skills) == 0 {
		m.addInfo("  no skills defined\n" +
			"  add an Agent Skill at .agents/skills/<name>/SKILL.md (or ~/.agents/skills/<name>/SKILL.md).\n" +
			"  Native .claude/skills packs and legacy .switchboard/skills packs are discovered too:\n\n" +
			"    ---\n" +
			"    name: migrations\n" +
			"    description: how migrations are written in this repo\n" +
			"    ---\n" +
			"    Migrations live in db/migrations, numbered, never edited after merge.\n\n" +
			"  /learn <name> writes the standard .agents layout; new files are picked up on the next Switchboard run")
		return nil
	}

	var b strings.Builder
	for _, sk := range m.app.skills {
		fmt.Fprintf(&b, "  %s  [%s]\n", sk.Key(), skillInvocationState(sk))
		description := strings.Join(strings.Fields(sk.Description), " ")
		fmt.Fprintf(&b, "    %s — %s\n", sk.Name, description)
		origin := fmt.Sprintf("%s/%s", sk.Origin.Ecosystem, sk.Origin.Scope)
		if sk.Origin.Namespace != "" {
			origin = "plugin " + sk.Origin.Namespace + " · " + origin
		}
		fmt.Fprintf(&b, "    %s · %s\n", origin, skillOriginPath(m, sk))
		if sk.ImplicitDisabled {
			b.WriteString("    model hidden: native invocation policy requires an explicit user command\n")
		}
		if sk.UserInvocationDisabled {
			b.WriteString("    user hidden: native policy sets user-invocable:false\n")
		}
		var blockers []string
		blockers = append(blockers, sk.ModelBlockers...)
		blockers = append(blockers, sk.InvocationBlockers...)
		if len(blockers) > 0 {
			fmt.Fprintf(&b, "    blocked: %s\n", strings.Join(blockers, "; "))
		}
		if sk.ArgumentHint != "" && !sk.UserInvocationDisabled && len(sk.InvocationBlockers) == 0 {
			fmt.Fprintf(&b, "    usage: /skill %s %s\n", sk.Key(), sk.ArgumentHint)
		}
	}
	b.WriteString("\n  invoke with /skill <canonical-selector> [args]; selectors are exact and never resolve by display-name precedence")
	m.addInfo(strings.TrimRight(b.String(), "\n"))
	return nil
}

func skillInvocationState(sk skills.Skill) string {
	model := !sk.ImplicitDisabled && len(sk.ModelBlockers) == 0 && len(sk.InvocationBlockers) == 0
	user := !sk.UserInvocationDisabled && len(sk.InvocationBlockers) == 0
	switch {
	case model && user:
		return "model + user"
	case model:
		return "model only"
	case user:
		return "user only"
	default:
		return "blocked"
	}
}

func skillOriginPath(m *tuiModel, sk skills.Skill) string {
	path := sk.Origin.LogicalPath
	if path == "" {
		path = sk.Origin.Path
	}
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return filepath.Join("~", rel)
		}
	}
	return m.app.displayPath(path)
}

func cmdSkill(m *tuiModel, args string) tea.Cmd {
	selector, invocationArgs := splitSkillCommand(args)
	if selector == "" {
		return noticeCmd("", skillUsage)
	}
	sk, err := skills.Resolve(m.app.skills, selector)
	if err != nil {
		return noticeCmd("error", err.Error())
	}
	body, err := skills.RenderExplicit(sk, invocationArgs)
	if err != nil {
		return noticeCmd("error", err.Error())
	}
	display := "/skill " + selector
	if invocationArgs != "" {
		display += " " + invocationArgs
	}
	prompt := "The user explicitly invoked skill " + selector + ". Follow these instructions:\n\n" + body
	m.addInfo("invoking " + selector + " from " + skillOriginPath(m, sk))
	return m.startSkillPrompt(display, prompt)
}

func splitSkillCommand(args string) (selector, rest string) {
	args = strings.TrimSpace(args)
	if args == "" {
		return "", ""
	}
	i := strings.IndexFunc(args, unicode.IsSpace)
	if i < 0 {
		return args, ""
	}
	return args[:i], strings.TrimSpace(args[i:])
}

// startSkillPrompt bypasses @mention expansion for the rendered skill body.
// Claude @ references are rejected before this point; another ecosystem's
// literal @ text must not accidentally inherit Switchboard's input shortcut.
func (m *tuiModel) startSkillPrompt(display, prompt string) tea.Cmd {
	m.addUser(display)
	prompt = m.watchContext(m.adviceContext(m.shellContext(prompt)))
	if leaks := credential.ScanPrompt(prompt); len(leaks) > 0 {
		return m.openSecretGate(leaks, prompt, func(p string) tea.Cmd {
			return m.launchTurn(p, nil)
		})
	}
	return m.launchTurn(prompt, nil)
}
