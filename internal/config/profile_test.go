package config

import (
	"strings"
	"testing"
)

const profileFixture = `
[tiers.t1]
label = "light"
model = "ollama/qwen3.5:9b-mlx"

[tiers.t2]
label = "deep"
model = "kimi/kimi-for-coding"

[profiles.review.tiers.t1]
label = "reviewer"
model = "kimi/kimi-for-coding"
effort = "high"

[profiles.docs.tiers.t1]
model = "ollama/qwen3.5:9b-mlx"
`

func TestProfilesLoadAndApply(t *testing.T) {
	c, err := LoadFile(write(t, profileFixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Profiles) != 2 {
		t.Fatalf("profiles = %d, want 2", len(c.Profiles))
	}
	if err := c.ApplyProfile("review"); err != nil {
		t.Fatal(err)
	}
	if c.ActiveProfile != "review" {
		t.Errorf("ActiveProfile = %q", c.ActiveProfile)
	}
	if len(c.Tiers) != 1 || c.Tiers[0].Label != "reviewer" {
		t.Fatalf("the active ladder is not the profile's: %+v", c.Tiers)
	}
	if c.Tiers[0].Target.Params.Reasoning == nil || c.Tiers[0].Target.Params.Reasoning.Effort != "high" {
		t.Errorf("the profile rung lost its effort: %+v", c.Tiers[0].Target)
	}
}

func TestApplyProfileNamesTheOnesConfigured(t *testing.T) {
	c, err := LoadFile(write(t, profileFixture))
	if err != nil {
		t.Fatal(err)
	}
	err = c.ApplyProfile("writing")
	if err == nil || !strings.Contains(err.Error(), "docs, review") {
		t.Errorf("an unknown profile must name what the file holds, got %v", err)
	}

	bare, err := LoadFile(write(t, "[tiers.t1]\nmodel = \"ollama/qwen3.5:9b-mlx\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	err = bare.ApplyProfile("review")
	if err == nil || !strings.Contains(err.Error(), "no profiles are configured") {
		t.Errorf("with none configured the error must say how to declare one, got %v", err)
	}
}

// TestSaveUnderAProfileKeepsTheMainLadder is the contract that makes
// -profile safe to combine with the TUI's own saves: /theme or /budget
// persisting mid-session must not overwrite the main ladder with the
// profile's rungs, and a rung bound under a profile lands in the profile.
func TestSaveUnderAProfileKeepsTheMainLadder(t *testing.T) {
	c, err := LoadFile(write(t, profileFixture))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ApplyProfile("review"); err != nil {
		t.Fatal(err)
	}
	if err := c.BindTier("t2", "checker", "ollama/qwen3.8:27b-mlx", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}

	reread, err := LoadFile(c.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reread.Tiers) != 2 || reread.Tiers[0].Label != "light" || reread.Tiers[1].Label != "deep" {
		t.Fatalf("the main ladder did not survive a save under a profile: %+v", reread.Tiers)
	}
	review := reread.Profiles["review"]
	if len(review) != 2 || review[0].Label != "reviewer" || review[1].Label != "checker" {
		t.Fatalf("the rung bound under the profile did not land in it: %+v", review)
	}
	if docs := reread.Profiles["docs"]; len(docs) != 1 {
		t.Fatalf("the inactive profile did not survive: %+v", docs)
	}
}

func TestProfileErrorsNameTheProfile(t *testing.T) {
	_, err := LoadFile(write(t, "[profiles.review.tiers.t1]\nmodel = \"nonsense\"\n"))
	if err == nil || !strings.Contains(err.Error(), "profile review tier t1") {
		t.Errorf("a broken rung inside a profile must say which profile, got %v", err)
	}

	_, err = LoadFile(write(t, "[profiles.review]\n"))
	if err == nil || !strings.Contains(err.Error(), "has no tiers") {
		t.Errorf("an empty profile must be refused at load, got %v", err)
	}
}
