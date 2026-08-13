package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMissingFileIsNotAnError(t *testing.T) {
	c, err := LoadFile(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("running without a config file is the normal first-run case: %v", err)
	}
	if len(c.Tiers) != 0 {
		t.Errorf("got %d tiers from an absent file", len(c.Tiers))
	}
	if _, ok := c.Default(); ok {
		t.Error("an empty ladder has no default tier")
	}
}

func TestTiersLoadInNumericOrder(t *testing.T) {
	path := write(t, `
[tiers.t10]
label = "max"
model = "ollama/big"

[tiers.t2]
label = "standard"
model = "ollama/medium"

[tiers.t1]
label = "light"
model = "ollama/small"
`)
	c, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Lexical order would put t10 second. The ladder is ascending policy, so
	// the order has to be numeric.
	var got []string
	for _, tier := range c.Tiers {
		got = append(got, tier.ID)
	}
	if strings.Join(got, ",") != "t1,t2,t10" {
		t.Errorf("tier order = %v, want t1,t2,t10", got)
	}

	def, ok := c.Default()
	if !ok || def.ID != "t1" {
		t.Errorf("default tier = %+v, want t1: a session starts at the bottom of the ladder", def)
	}
	if def.Label != "light" {
		t.Errorf("label = %q", def.Label)
	}
}

func TestTierBindsTargetAndEffort(t *testing.T) {
	path := write(t, `
[tiers.t1]
model = "ollama/qwen3.5:9b-mlx"

[tiers.t2]
model = "ollama/qwen3.6:27b"
effort = "high"
`)
	c, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	t1, _ := c.Tier("t1")
	if t1.Target.Provider != "ollama" || t1.Target.Surface != "local" {
		t.Errorf("target = %+v, want the ollama default surface", t1.Target)
	}
	if t1.Target.ModelID != "qwen3.5:9b-mlx" {
		t.Errorf("model id = %q; a colon in the name must survive the split", t1.Target.ModelID)
	}

	t2, _ := c.Tier("t2")
	if t2.Target.Params.Reasoning == nil || t2.Target.Params.Reasoning.Effort != "high" {
		t.Errorf("effort did not bind: %+v", t2.Target.Params.Reasoning)
	}
	// Two tiers on the same model at different effort are different targets,
	// because effort changes cache identity (§3.1).
	if t1.Target.ID() == t2.Target.ID() {
		t.Error("tiers with different effort produced the same target id")
	}
}

// Model identifiers legitimately contain slashes, so the provider split has to
// take the first one only.
func TestModelNamesMayContainSlashes(t *testing.T) {
	target, err := ParseTarget("ollama/hf.co/someone/a-model", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if target.Provider != "ollama" {
		t.Errorf("provider = %q", target.Provider)
	}
	if target.ModelID != "hf.co/someone/a-model" {
		t.Errorf("model id = %q, want the whole remainder", target.ModelID)
	}
}

func TestExplicitSurface(t *testing.T) {
	target, err := ParseTarget("anthropic/claude-opus-5", "bedrock", "")
	if err != nil {
		t.Fatal(err)
	}
	if target.Surface != "bedrock" {
		t.Errorf("surface = %q, want the explicit value", target.Surface)
	}
}

// Guessing a surface would attach the wrong catalog entry, and price, cache
// behavior, and retention all differ per surface.
func TestUnknownProviderNeedsAnExplicitSurface(t *testing.T) {
	_, err := ParseTarget("acme/some-model", "", "")
	if err == nil || !strings.Contains(err.Error(), "surface") {
		t.Errorf("err = %v, want a complaint about the missing surface", err)
	}
}

func TestMalformedModelReference(t *testing.T) {
	for _, ref := range []string{"", "just-a-model", "ollama/"} {
		if _, err := ParseTarget(ref, "", ""); err == nil {
			t.Errorf("%q should not parse as a target", ref)
		}
	}
}

func TestTierNamesMustFollowTheScheme(t *testing.T) {
	for _, name := range []string{"fast", "t0", "tier1", "tx"} {
		path := write(t, "[tiers."+name+"]\nmodel = \"ollama/m\"\n")
		if _, err := LoadFile(path); err == nil {
			t.Errorf("tier name %q should be rejected", name)
		}
	}
}

// A misspelled key that is silently ignored is a setting the user believes is
// in effect and is not.
func TestUnrecognizedKeysAreRejected(t *testing.T) {
	path := write(t, `
[tiers.t1]
model = "ollama/m"
labl = "typo"
`)
	_, err := LoadFile(path)
	if err == nil {
		t.Fatal("a misspelled key must be an error, not silently dropped")
	}
	if !strings.Contains(err.Error(), "labl") {
		t.Errorf("err = %v, want it to name the unrecognized key", err)
	}
}

func TestSlots(t *testing.T) {
	path := write(t, `
[tiers.t1]
model = "ollama/m"

[slots]
title = "t1"
embed = "ollama/nomic-embed-text"
`)
	c, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Slots["title"] != "t1" {
		t.Errorf("a slot aliasing a tier did not load: %v", c.Slots)
	}
	if c.Slots["embed"] != "ollama/nomic-embed-text" {
		t.Errorf("a slot naming a model directly did not load: %v", c.Slots)
	}
}

func TestTooManyTiers(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= MaxTiers+1; i++ {
		fmt.Fprintf(&b, "[tiers.t%d]\nmodel = \"ollama/m\"\n\n", i)
	}
	path := write(t, b.String())
	if _, err := LoadFile(path); err == nil {
		t.Errorf("more than %d tiers should be rejected", MaxTiers)
	}
}

// A provider reached at another address is still that provider: a gateway, an
// Azure deployment, or a proxy does not change which credential pays or which
// catalog entry prices it.
func TestProviderBaseURLOverride(t *testing.T) {
	path := write(t, `
[tiers.t1]
model = "openai/some-model"

[providers.openai]
base_url = "https://gateway.example.com/v1"
`)
	c, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.ProviderFor("openai").BaseURL; got != "https://gateway.example.com/v1" {
		t.Errorf("base url = %q", got)
	}
	if got := c.ProviderFor("anthropic").BaseURL; got != "" {
		t.Errorf("an unconfigured provider reported %q rather than falling back to its default", got)
	}

	t1, _ := c.Tier("t1")
	if t1.Target.Provider != "openai" || t1.Target.Surface != "first-party" {
		t.Errorf("the override changed target identity: %+v", t1.Target)
	}
}

func TestUnrecognizedProviderKeyIsRejected(t *testing.T) {
	path := write(t, "[providers.openai]\nbase_urls = \"typo\"\n")
	if _, err := LoadFile(path); err == nil {
		t.Error("a misspelled provider key must be an error, not silently ignored")
	}
}
