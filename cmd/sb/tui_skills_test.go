package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/skills"
)

func TestSkillsCommandShowsCanonicalOriginsAndInvocationState(t *testing.T) {
	m := testModel(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	m.app.workspace = filepath.Join(t.TempDir(), "repo")
	m.app.skills = []skills.Skill{
		{
			Name: "Deploy production", Description: "Deploy deliberately", Selector: "claude:user:deploy",
			ImplicitDisabled: true, ArgumentHint: "[environment]",
			Origin: skills.Origin{Ecosystem: skills.EcosystemClaude, Scope: skills.ScopeUser, LogicalPath: filepath.Join(home, ".claude", "skills", "deploy", "SKILL.md")},
		},
		{
			Name: "background", Description: "Background context", Selector: "codex:repo:.agents/skills/background",
			UserInvocationDisabled: true,
			Origin:                 skills.Origin{Ecosystem: skills.EcosystemCodex, Scope: skills.ScopeWorkspace, LogicalPath: filepath.Join(m.app.workspace, ".agents", "skills", "background", "SKILL.md")},
		},
		{
			Name: "unsafe", Description: "Needs a native host", Selector: "claude:repo:.claude/skills/unsafe",
			InvocationBlockers: []string{`unsupported control "context"`},
			Origin:             skills.Origin{Ecosystem: skills.EcosystemClaude, Scope: skills.ScopeWorkspace, LogicalPath: filepath.Join(m.app.workspace, ".claude", "skills", "unsafe", "SKILL.md")},
		},
		{
			Name: "plugin-review", Description: "Plugin review", Selector: "plugin:claude%3Areview:plugin-review",
			Origin: skills.Origin{Ecosystem: skills.EcosystemClaude, Scope: skills.ScopeUser, Namespace: "claude:review", LogicalPath: filepath.Join(home, ".cache", "review", "skills", "plugin-review", "SKILL.md")},
		},
	}

	if cmd := cmdSkills(m, ""); cmd != nil {
		t.Fatal("/skills unexpectedly became asynchronous")
	}
	got := m.tr.last().text
	for _, want := range []string{
		"claude:user:deploy  [user only]",
		"usage: /skill claude:user:deploy [environment]",
		"~/.claude/skills/deploy/SKILL.md",
		"codex:repo:.agents/skills/background  [model only]",
		".agents/skills/background/SKILL.md",
		"claude:repo:.claude/skills/unsafe  [blocked]",
		`unsupported control "context"`,
		"plugin claude:review · claude/user",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("/skills missing %q:\n%s", want, got)
		}
	}
}

func TestSkillCommandUsesExactSelectorAndDoesNotExpandMentions(t *testing.T) {
	m := testModel(t)
	m.app.config.Tiers = nil // makes the returned planning command deterministic and offline
	m.app.skills = []skills.Skill{{
		Name: "literal", Description: "literal prompt", Selector: "codex:repo:.agents/skills/literal",
		Body:   "Keep @not-a-switchboard-attachment literal.",
		Origin: skills.Origin{Ecosystem: skills.EcosystemCodex, Scope: skills.ScopeWorkspace, LogicalPath: filepath.Join(m.app.workspace, ".agents", "skills", "literal", "SKILL.md")},
	}}

	cmd := cmdSkill(m, "codex:repo:.agents/skills/literal extra")
	if cmd == nil || !m.busy || !m.turnPlanning {
		t.Fatalf("explicit skill did not enter turn planning: cmd=%v busy=%v planning=%v", cmd != nil, m.busy, m.turnPlanning)
	}
	msg, ok := cmd().(turnPlanMsg)
	if !ok {
		t.Fatalf("skill command returned %T, want turnPlanMsg", cmd())
	}
	for _, want := range []string{"explicitly invoked skill codex:repo:.agents/skills/literal", "@not-a-switchboard-attachment", "ARGUMENTS: extra"} {
		if !strings.Contains(msg.prompt, want) {
			t.Errorf("rendered prompt missing %q:\n%s", want, msg.prompt)
		}
	}
	if last := m.tr.last(); last == nil || last.kind != kindUser || last.text != "/skill codex:repo:.agents/skills/literal extra" {
		t.Fatalf("transcript did not preserve the typed command: %+v", last)
	}
	m.finishPlanning()
}

func TestSkillCommandReportsResolutionAndInvocationBlocks(t *testing.T) {
	m := testModel(t)
	m.app.skills = []skills.Skill{{
		Name: "background", Description: "background", Selector: "claude:user:background", Body: "body",
		UserInvocationDisabled: true, Origin: skills.Origin{Ecosystem: skills.EcosystemClaude},
	}}

	for _, tc := range []struct {
		args string
		want string
	}{
		{args: "background", want: "canonical selector"},
		{args: "claude:user:background", want: "user-invocable:false"},
	} {
		cmd := cmdSkill(m, tc.args)
		if cmd == nil {
			t.Fatalf("/skill %s returned no diagnostic", tc.args)
		}
		msg, ok := cmd().(noticeMsg)
		if !ok || msg.level != "error" || !strings.Contains(msg.text, tc.want) {
			t.Fatalf("/skill %s diagnostic = %#v, want %q", tc.args, msg, tc.want)
		}
	}
	if m.busy {
		t.Fatal("a rejected skill invocation started a turn")
	}
}
