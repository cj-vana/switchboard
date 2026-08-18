package skills

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestLoadAdditionalUsesOnlyExplicitNamespacedComponentRoots(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	writePackedSkill(t, filepath.Join(home, ".claude", "skills"), "not-activated", minimal)

	parent := t.TempDir()
	component := filepath.Join(parent, "plugin", "skills")
	writePackedSkill(t, component, "review", "---\nname: Fancy Review\ndescription: reviews changes\ndisable-model-invocation: true\narguments: [scope]\n---\nReview $scope")
	writePackedSkill(t, component, "background", "---\ndescription: background\nuser-invocable: false\n---\nContext")
	writePackedSkill(t, filepath.Join(parent, "sibling", "skills"), "not-in-component", minimal)

	list, notes := LoadAdditional([]AdditionalRoot{{
		Path: component, Namespace: "claude:review-tools", Dialect: EcosystemClaude, Scope: ScopeUser,
	}})
	if len(notes) != 0 || len(list) != 2 {
		t.Fatalf("additional load = %+v, notes %v", list, notes)
	}
	wantKeys := []string{
		"plugin:claude%3Areview-tools:Fancy%20Review",
		"plugin:claude%3Areview-tools:background",
	}
	if got := skillKeys(list); !slices.Equal(got, wantKeys) {
		t.Fatalf("plugin selectors = %v, want %v", got, wantKeys)
	}
	for _, sk := range list {
		if sk.Origin.Namespace != "claude:review-tools" || sk.Origin.Ecosystem != EcosystemClaude || sk.Origin.Scope != ScopeUser {
			t.Errorf("plugin provenance lost: %+v", sk.Origin)
		}
		if strings.Contains(sk.Key(), "not-activated") || strings.Contains(sk.Key(), "not-in-component") {
			t.Errorf("loader searched outside its explicit component: %+v", sk)
		}
	}
	manual := findSkillKey(t, list, wantKeys[0])
	if !manual.ImplicitDisabled || len(ModelVisible(list)) != 1 {
		t.Fatalf("plugin invocation policy was not preserved: %+v", list)
	}
	if got, err := RenderExplicit(manual, "api"); err != nil || got != "Review api" {
		t.Fatalf("plugin explicit render = %q, %v", got, err)
	}
}

func TestLoadAdditionalSupportsOneRootSkillAndCodexPolicy(t *testing.T) {
	root := filepath.Join(t.TempDir(), "single")
	writeSkill(t, root, "SKILL.md", "---\nname: deploy\ndescription: deploy\n---\nDeploy")
	writeSkill(t, filepath.Join(root, "agents"), "openai.yaml", "policy:\n  allow_implicit_invocation: false\n")
	// A child definition is deliberately ignored when the component itself is
	// a single root skill.
	writePackedSkill(t, root, "child", minimal)

	list, notes := LoadAdditional([]AdditionalRoot{{
		Path: root, Namespace: "codex:ops", Dialect: EcosystemCodex, Scope: ScopeManaged,
	}})
	if len(notes) != 0 || len(list) != 1 {
		t.Fatalf("root component = %+v, notes %v", list, notes)
	}
	sk := list[0]
	if sk.Key() != "plugin:codex%3Aops:deploy" || !sk.ImplicitDisabled || sk.Origin.Scope != ScopeManaged {
		t.Fatalf("root plugin skill = %+v", sk)
	}
}

func TestLoadAdditionalValidatesCallerIdentityAndSortsInput(t *testing.T) {
	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	writePackedSkill(t, first, "z", minimal)
	writePackedSkill(t, second, "a", minimal)

	list, notes := LoadAdditional([]AdditionalRoot{
		{Path: first, Namespace: "zeta", Dialect: EcosystemCodex, Scope: ScopeLocal},
		{Path: second, Namespace: "alpha", Dialect: EcosystemClaude, Scope: ScopeWorkspace},
		{Path: first, Namespace: "bad/name", Dialect: EcosystemCodex, Scope: ScopeUser},
		{Path: first, Namespace: "wrong", Dialect: EcosystemSwitchboard, Scope: ScopeUser},
	})
	if got := skillKeys(list); !slices.Equal(got, []string{"plugin:alpha:a", "plugin:zeta:z"}) {
		t.Fatalf("additional input order affected selectors: %v", got)
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "namespace") || !strings.Contains(joined, "dialect") {
		t.Fatalf("invalid caller identity needs diagnostics: %v", notes)
	}
}

func TestLoadAdditionalRetainsSameNameCollisionsAsAmbiguous(t *testing.T) {
	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	writePackedSkill(t, first, "one", "---\nname: same\ndescription: first\n---\nFirst")
	writePackedSkill(t, second, "two", "---\nname: same\ndescription: second\n---\nSecond")
	list, _ := LoadAdditional([]AdditionalRoot{
		{Path: first, Namespace: "claude:dupe", Dialect: EcosystemClaude, Scope: ScopeUser},
		{Path: second, Namespace: "claude:dupe", Dialect: EcosystemClaude, Scope: ScopeUser},
	})
	if len(list) != 2 || list[0].Key() != list[1].Key() {
		t.Fatalf("same native plugin command should remain visibly ambiguous: %+v", list)
	}
	if _, err := Resolve(list, list[0].Key()); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("explicit duplicate plugin command silently chose a component: %v", err)
	}
	tool := NewTool(list)
	in, _ := json.Marshal(skillInput{Name: list[0].Key()})
	if _, err := tool.Plan(in); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("model duplicate plugin command silently chose a component: %v", err)
	}
}

func TestLoadAdditionalAnchorsSymlinkedComponentResources(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not generally available")
	}
	real := filepath.Join(t.TempDir(), "real")
	writePackedSkill(t, real, "read", minimal)
	writeSkill(t, filepath.Join(real, "read", "references"), "note.md", "right")
	aliasParent := t.TempDir()
	alias := filepath.Join(aliasParent, "skills")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	list, notes := LoadAdditional([]AdditionalRoot{{
		Path: alias, Namespace: "claude:safe", Dialect: EcosystemClaude, Scope: ScopeUser,
	}})
	if len(notes) != 0 || len(list) != 1 {
		t.Fatalf("symlinked component = %+v, notes %v", list, notes)
	}
	tool := NewTool(list)
	in, _ := json.Marshal(skillInput{Name: list[0].Key(), File: "references/note.md"})
	plan, err := tool.Plan(in)
	if err != nil {
		t.Fatal(err)
	}

	evil := filepath.Join(t.TempDir(), "evil")
	writePackedSkill(t, evil, "read", minimal)
	writeSkill(t, filepath.Join(evil, "read", "references"), "note.md", "wrong")
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(evil, alias); err != nil {
		t.Fatal(err)
	}
	res, err := plan.Run(context.Background())
	if err != nil || res.Content != "right" {
		t.Fatalf("plugin component alias retargeted an anchored resource: %+v, %v", res, err)
	}
}
