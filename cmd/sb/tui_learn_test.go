package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/skills"
)

func TestComposeSkillRedactsBeforeAnythingReachesDisk(t *testing.T) {
	// The guarantee is the test that greps for the token, not the comment
	// above the code: a distiller that echoed a key from the transcript must
	// not hand it to every future session and every clone.
	token := "ghp_" + strings.Repeat("a", 36)
	generated := "Use when releasing this package.\n\nRun the publish script with the token " + token + " set in the env."

	content, redacted, err := composeSkill("release-checklist", generated, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(content, token) {
		t.Fatal("the composed skill still carries the credential")
	}
	if redacted != 1 {
		t.Fatalf("redacted = %d, want 1", redacted)
	}
	if !strings.Contains(content, "[redacted: a GitHub token]") {
		t.Errorf("the redaction should say what stood there:\n%s", content)
	}
}

func TestComposeSkillRoundTripsThroughTheLoader(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // the loader also reads user skill trees
	generated := "Use when the build cache misbehaves in this repo.\n\n1. Stop the daemon.\n2. Clear ~/.cache/build.\n3. Rebuild with -x."

	content, _, err := composeSkill("cache-repair", generated, "")
	if err != nil {
		t.Fatal(err)
	}

	workspace := t.TempDir()
	dir := filepath.Join(workspace, ".agents", "skills", "cache-repair")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	list, notes := skills.Load(workspace)
	if len(notes) != 0 {
		t.Fatalf("the loader had complaints: %v", notes)
	}
	if len(list) != 1 {
		t.Fatalf("loaded %d skills, want 1", len(list))
	}
	sk := list[0]
	if sk.Name != "cache-repair" {
		t.Errorf("name = %q", sk.Name)
	}
	if sk.Description != "Use when the build cache misbehaves in this repo." {
		t.Errorf("description = %q", sk.Description)
	}
	if !strings.Contains(sk.Body, "Stop the daemon") {
		t.Errorf("body lost the instructions:\n%s", sk.Body)
	}
}

func TestComposeSkillCutsAWrappedDescriptionAtItsLine(t *testing.T) {
	generated := "Use when releasing\nthis package to npm.\n\nThe steps."
	// The parser reads the description to the end of its line, so the cut is
	// at the distiller's first newline; the wrapped tail must land in the
	// body rather than leak a newline into the frontmatter or be dropped.
	content, _, err := composeSkill("npm-release", generated, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "description: Use when releasing\n") {
		t.Errorf("composed:\n%s", content)
	}
	_, body, _ := strings.Cut(content, "---\n\n")
	if !strings.HasPrefix(body, "this package to npm.") {
		t.Errorf("the wrapped tail should open the body:\n%s", content)
	}
}

// The provenance paragraph is what makes the pack deletable later: it rides
// the body where a reader finds it, it survives the loader round trip, and
// it sits inside the credential scan's reach like everything else composed.
func TestComposeSkillCarriesProvenanceInTheBody(t *testing.T) {
	generated := "Use when releasing this package.\n\nRun the publish script."
	prov := "Provenance: distilled from session abc123 on 2026-08-17, 12 messages, written by ollama/local/qwen3:4b."

	content, _, err := composeSkill("release-checklist", generated, prov)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, prov) {
		t.Errorf("the provenance paragraph is missing:\n%s", content)
	}
	if strings.Contains(strings.SplitN(content, "---\n\n", 2)[0], "Provenance") {
		t.Errorf("provenance belongs in the body, not the frontmatter:\n%s", content)
	}

	// A key that somehow reached the provenance string redacts like one in
	// the method: the scan covers the whole file, not the distiller's half.
	token := "ghp_" + strings.Repeat("b", 36)
	leaked, redacted, err := composeSkill("x-ray", generated, "Provenance: "+token)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(leaked, token) || redacted != 1 {
		t.Fatalf("the provenance escaped the scan (redacted=%d):\n%s", redacted, leaked)
	}
}

func TestComposeSkillRefusesAnEmptyMethod(t *testing.T) {
	if _, _, err := composeSkill("nothing", "Only a description, no body.", ""); err == nil {
		t.Fatal("a skill with no instructions is not a skill")
	}
}

func TestSkillNamePattern(t *testing.T) {
	for name, ok := range map[string]bool{
		"release-checklist": true,
		"a":                 true,
		"v2-migration":      true,
		"Release":           false,
		"two words":         false,
		"trailing-":         false,
		"-leading":          false,
		"dots.bad":          false,
		"":                  false,
	} {
		if got := skillNamePattern.MatchString(name); got != ok {
			t.Errorf("skillNamePattern(%q) = %v, want %v", name, got, ok)
		}
	}
}
