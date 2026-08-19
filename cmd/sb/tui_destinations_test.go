package main

import (
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/provider"
	route "github.com/switchboard-code/switchboard/internal/router"
)

func destinationsModel(t *testing.T, tiers ...config.Tier) *tuiModel {
	t.Helper()
	m := testModel(t)
	m.app.config = &config.Config{Path: t.TempDir() + "/config.toml", Tiers: tiers}
	return m
}

func tierOn(id, providerName, model string) config.Tier {
	return config.Tier{ID: id, Target: provider.RouteTarget{
		Provider: providerName, Surface: "local", ModelID: model,
	}}
}

// The router filters this before economics and has its own exclusion sentence
// for it. Nothing outside the tests ever set it, so the check existed and no
// user could reach it.
func TestDestinationsRestrictionReachesTheRouter(t *testing.T) {
	m := destinationsModel(t, tierOn("t1", "ollama", "local:7b"), tierOn("t2", "anthropic", "claude-opus-5"))

	if cmd := cmdDestinations(m, "ollama"); cmd != nil {
		if notice, ok := cmd().(noticeMsg); ok && notice.level == "error" {
			t.Fatalf("restricting to a provider the ladder runs was refused: %s", notice.text)
		}
	}
	if got := m.app.config.Destinations; len(got) != 1 || got[0] != "ollama" {
		t.Fatalf("Destinations = %v, want the policy that was typed", got)
	}

	// The value only means something if the router honors it, so assert
	// against the router rather than against the field.
	decision, err := (route.Heuristic{}).Route(route.Input{
		Candidates: []route.Candidate{
			{Target: provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-opus-5"},
				Tier: "t2", CatalogKnown: true, CeilingCost: 1,
				Info: catalog.ModelInfo{ContextWindow: 200000, Tools: catalog.ToolsParallel, Metering: catalog.PerToken}},
		},
		Requirements: route.Requirements{NeedsTools: true, ApprovedProviders: m.app.config.Destinations},
	})
	if err == nil {
		t.Fatalf("an unapproved provider was routed to: %+v", decision)
	}
}

// A ladder with no reachable rung fails on the next turn with an exclusion for
// every entry, which reads as a broken program rather than as the policy
// working. The moment to say so is when it is typed.
func TestDestinationsRefusesAPolicyTheLadderCannotSatisfy(t *testing.T) {
	m := destinationsModel(t, tierOn("t1", "ollama", "local:7b"))

	cmd := cmdDestinations(m, "anthropic")
	if cmd == nil {
		t.Fatal("a policy excluding every rung was accepted silently")
	}
	notice, ok := cmd().(noticeMsg)
	if !ok || notice.level != "error" {
		t.Fatalf("msg = %#v, want a refusal", cmd())
	}
	if !strings.Contains(notice.text, "nowhere to go") {
		t.Errorf("refusal = %q, which does not say what would happen", notice.text)
	}
	if len(m.app.config.Destinations) != 0 {
		t.Errorf("Destinations = %v, want the refused policy not to have been stored", m.app.config.Destinations)
	}
}

func TestDestinationsAnyClearsTheRestriction(t *testing.T) {
	m := destinationsModel(t, tierOn("t1", "ollama", "local:7b"))
	m.app.config.Destinations = []string{"ollama"}

	cmdDestinations(m, "any")
	if len(m.app.config.Destinations) != 0 {
		t.Errorf("Destinations = %v, want the restriction removed", m.app.config.Destinations)
	}
}

// The standing line has to name what the ladder actually runs, because that is
// the list the user is choosing from.
func TestDestinationsStandingNamesTheLaddersProviders(t *testing.T) {
	m := destinationsModel(t, tierOn("t1", "ollama", "local:7b"), tierOn("t2", "anthropic", "claude-opus-5"))

	standing := m.destinationsStanding()
	for _, want := range []string{"ollama", "anthropic", "unrestricted"} {
		if !strings.Contains(standing, want) {
			t.Errorf("standing = %q, missing %q", standing, want)
		}
	}
}

// The policy outlives the process or it is not a policy.
func TestDestinationsSurviveASaveAndLoad(t *testing.T) {
	m := destinationsModel(t, tierOn("t1", "ollama", "local:7b"), tierOn("t2", "anthropic", "claude-opus-5"))
	if cmd := cmdDestinations(m, "ollama anthropic"); cmd != nil {
		if notice, ok := cmd().(noticeMsg); ok && notice.level == "error" {
			t.Fatalf("setting the policy failed: %s", notice.text)
		}
	}

	reloaded, err := config.LoadFile(m.app.config.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Destinations) != 2 {
		t.Fatalf("Destinations = %v, want both providers to survive the round trip", reloaded.Destinations)
	}
}

// The router filters candidates, and a candidate is what a user turn is routed
// among. Every other path to a provider resolves a rung directly, and the one
// that matters most is delegate, where the model picks the rung: a policy the
// router enforced on turns alone would be a policy a tool call walks around.
func TestDestinationPolicyCoversDirectlyResolvedRungs(t *testing.T) {
	cfg := &config.Config{
		Tiers:        []config.Tier{tierOn("t1", "ollama", "local:7b"), tierOn("t2", "anthropic", "claude-opus-5")},
		Destinations: []string{"ollama"},
	}

	if err := destinationAllowed(cfg, cfg.Tiers[0].Target); err != nil {
		t.Errorf("an approved provider was refused: %v", err)
	}
	err := destinationAllowed(cfg, cfg.Tiers[1].Target)
	if err == nil {
		t.Fatal("an unapproved provider was allowed through a direct resolution")
	}
	if !strings.Contains(err.Error(), "not an approved destination") {
		t.Errorf("refusal = %q, want the router's own words", err)
	}
	// The refusal has to name where the policy is read, or it is a dead end.
	if !strings.Contains(err.Error(), "/destinations") {
		t.Errorf("refusal = %q, which does not say where the list lives", err)
	}
}

// An unrestricted workspace must behave exactly as it did before the policy
// existed, on every one of those paths.
func TestNoPolicyAllowsEveryDirectlyResolvedRung(t *testing.T) {
	cfg := &config.Config{Tiers: []config.Tier{tierOn("t1", "anthropic", "claude-opus-5")}}
	if err := destinationAllowed(cfg, cfg.Tiers[0].Target); err != nil {
		t.Errorf("an unrestricted workspace refused a rung: %v", err)
	}
	if err := destinationAllowed(nil, cfg.Tiers[0].Target); err != nil {
		t.Errorf("a nil config refused a rung: %v", err)
	}
}

// The slot resolvers are the surface a user meets, and a slot pointing outside
// the policy has to say which slot rather than failing anonymously.
func TestASlotOutsideThePolicyNamesItself(t *testing.T) {
	m := destinationsModel(t, tierOn("t1", "ollama", "local:7b"), tierOn("t2", "anthropic", "claude-opus-5"))
	m.app.config.Slots = map[string]string{"auditor": "t2"}
	m.app.config.Destinations = []string{"ollama"}

	_, _, err := slotTier(m.app, "auditor")
	if err == nil {
		t.Fatal("a slot bound outside the policy resolved anyway")
	}
	if !strings.Contains(err.Error(), "auditor") {
		t.Errorf("error = %q, which does not name the slot", err)
	}
}
