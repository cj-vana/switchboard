package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func ruleFile(t *testing.T, workspace, name, content string) {
	t.Helper()
	dir := filepath.Join(workspace, ".switchboard", "rules")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The point of the feature: a rule costs nothing until the session touches
// what it is about, and then it says what it has to say.
func TestARuleFiresOnlyWhenItsPathsAreTouched(t *testing.T) {
	ws := t.TempDir()
	ruleFile(t, ws, "migrations.md", "---\npaths: migrations/*\n---\nNever edit a migration that has shipped.")
	set, notes := loadRules(ws)
	if len(notes) != 0 {
		t.Fatalf("loading produced notes: %v", notes)
	}
	if len(set.rules) != 1 {
		t.Fatalf("loaded %d rules, want one", len(set.rules))
	}

	if fired := set.matched([]string{filepath.Join(ws, "internal", "thing.go")}); len(fired) != 0 {
		t.Errorf("a rule fired for a path it does not name: %+v", fired)
	}
	fired := set.matched([]string{filepath.Join(ws, "migrations", "2026", "up.sql")})
	if len(fired) != 1 {
		t.Fatalf("the rule did not fire for a path under its glob: %+v", fired)
	}
	if !strings.Contains(fired[0].body, "shipped") {
		t.Errorf("the rule's body did not travel: %+v", fired[0])
	}
}

// A session that keeps editing one directory should not be told the same
// paragraph on every round.
func TestARuleFiresOnceASession(t *testing.T) {
	ws := t.TempDir()
	ruleFile(t, ws, "a.md", "---\npaths: src/*\n---\nSay this once.")
	set, _ := loadRules(ws)

	touched := []string{filepath.Join(ws, "src", "main.go")}
	if len(set.matched(touched)) != 1 {
		t.Fatal("the rule did not fire the first time")
	}
	if fired := set.matched(touched); len(fired) != 0 {
		t.Errorf("the rule fired again: %+v", fired)
	}
}

// A checkout with thirty rules that all match on the first turn would be the
// long instructions file it replaced, arriving later.
func TestTheSessionCapBoundsHowMuchArrivesThisWay(t *testing.T) {
	ws := t.TempDir()
	for i := range maxRulesPerSession + 4 {
		ruleFile(t, ws, string(rune('a'+i))+".md", "---\npaths: src/*\n---\nRule body.")
	}
	set, _ := loadRules(ws)

	fired := set.matched([]string{filepath.Join(ws, "src", "main.go")})
	if len(fired) > maxRulesPerSession {
		t.Errorf("%d rules fired at once, past the %d cap", len(fired), maxRulesPerSession)
	}
}

// A malformed rule is named at load rather than firing as something odd later.
func TestAMalformedRuleIsRefusedAndNamed(t *testing.T) {
	ws := t.TempDir()
	ruleFile(t, ws, "nopaths.md", "---\ntitle: hello\n---\nBody.")
	ruleFile(t, ws, "nobody.md", "---\npaths: src/*\n---\n")
	ruleFile(t, ws, "nofront.md", "Just a body with no frontmatter.")

	set, notes := loadRules(ws)
	if len(set.rules) != 0 {
		t.Errorf("a malformed rule loaded: %+v", set.rules)
	}
	if len(notes) != 3 {
		t.Fatalf("notes = %v, want one per refused rule", notes)
	}
	joined := strings.Join(notes, " ")
	for _, want := range []string{"names no paths", "no body", "no frontmatter"} {
		if !strings.Contains(joined, want) {
			t.Errorf("notes do not explain %q: %v", want, notes)
		}
	}
}

// A checkout that declares none costs nothing and says nothing.
func TestNoRulesDirectoryIsSilent(t *testing.T) {
	set, notes := loadRules(t.TempDir())
	if len(set.rules) != 0 || len(notes) != 0 {
		t.Errorf("an empty checkout produced %d rules and %v", len(set.rules), notes)
	}
	if fired := set.matched([]string{"/anything"}); len(fired) != 0 {
		t.Errorf("a rule fired with none loaded: %+v", fired)
	}
}

// A glob written the way a person writes it has to cover what they meant.
func TestGlobsCoverWhatTheirAuthorMeant(t *testing.T) {
	for _, test := range []struct {
		glob, path string
		want       bool
	}{
		{"migrations/*", "migrations/up.sql", true},
		{"migrations/*", "migrations/2026/up.sql", true},
		{"migrations", "migrations/2026/up.sql", true},
		{"migrations/**", "migrations/2026/up.sql", true},
		{"*.proto", "api/thing.proto", false},
		{"api/*.proto", "api/thing.proto", true},
		{"migrations/*", "internal/thing.go", false},
	} {
		got := matchRuleGlob(test.glob, test.path)
		if got != test.want {
			t.Errorf("matchRuleGlob(%q, %q) = %t, want %t", test.glob, test.path, got, test.want)
		}
	}
}

// A path outside the workspace is not this repository's business.
func TestAPathOutsideTheWorkspaceNeverMatches(t *testing.T) {
	ws := t.TempDir()
	ruleFile(t, ws, "a.md", "---\npaths: src/*\n---\nBody.")
	set, _ := loadRules(ws)

	if fired := set.matched([]string{filepath.Join(t.TempDir(), "src", "main.go")}); len(fired) != 0 {
		t.Errorf("a rule fired for a path outside the workspace: %+v", fired)
	}
}
