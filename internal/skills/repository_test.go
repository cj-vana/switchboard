package skills

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestRepositorySkillsAreValidAndModelVisible(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".agents", "skills"))
	if err != nil {
		t.Fatal(err)
	}
	loaded, notes := LoadAdditional([]AdditionalRoot{{
		Path: root, Namespace: "codex:switchboard", Dialect: EcosystemCodex, Scope: ScopeWorkspace,
	}})
	if len(notes) != 0 {
		t.Fatalf("repository skill diagnostics: %v", notes)
	}
	want := []string{"switchboard-extensions", "switchboard-router", "switchboard-shipcheck"}
	if got := skillNames(loaded); !slices.Equal(got, want) {
		t.Fatalf("repository skill names = %v, want %v", got, want)
	}
	if visible := ModelVisible(loaded); len(visible) != len(want) {
		t.Fatalf("only %d/%d repository skills are model-visible: %#v", len(visible), len(want), loaded)
	}
}
