package router

import (
	"strings"
	"testing"

	"github.com/cjvana/switchboard/internal/catalog"
	"github.com/cjvana/switchboard/internal/costmodel"
	"github.com/cjvana/switchboard/internal/provider"
)

func candidate(tier string, rank int, opts ...func(*Candidate)) Candidate {
	c := Candidate{
		Tier:   tier,
		Target: provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "m" + tier},
		Info: catalog.ModelInfo{
			ContextWindow: 200_000,
			Tools:         catalog.ToolsParallel,
			Vision:        true,
		},
		Rank:         rank,
		PromptTokens: 1_000,
	}
	for _, o := range opts {
		o(&c)
	}
	return c
}

func ladder() []Candidate {
	return []Candidate{candidate("t1", 0), candidate("t2", 1), candidate("t3", 2)}
}

func TestAShortRequestStaysLow(t *testing.T) {
	got, err := Heuristic{}.Route(Input{
		Prompt:     "what does main.go print?",
		Candidates: ladder(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Tier != "t1" {
		t.Errorf("tier = %q, want the bottom of the ladder", got.Tier)
	}
	if got.Source != SourceHeuristic {
		t.Errorf("source = %q", got.Source)
	}
	if got.Rationale == "" {
		t.Error("no rationale; §8.1 renders this to the user rather than logging it")
	}
}

// §8.2's own examples: breadth and a failure signature both argue upward.
func TestBreadthAndFailuresArgueUpward(t *testing.T) {
	wide, err := Heuristic{}.Route(Input{
		Prompt:     "refactor the storage layer",
		Session:    SessionFeatures{FilesInContext: 9, DiffSizeSoFar: 400},
		Candidates: ladder(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if wide.Tier == "t1" {
		t.Error("a wide refactor stayed at the bottom of the ladder")
	}
	if !strings.Contains(wide.Rationale, "files in play") {
		t.Errorf("rationale = %q; it has to name what moved the decision", wide.Rationale)
	}

	afterFailure, err := Heuristic{}.Route(Input{
		Prompt:     "fix it",
		Session:    SessionFeatures{PriorFailures: 1, TestsInvolved: true},
		Candidates: ladder(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if afterFailure.Tier == "t1" {
		t.Error("a turn straight after a test failure stayed at the bottom")
	}
}

// Intent is not difficulty. The word "refactor" on a one-line request should
// nudge, not jump to the top.
func TestABreadthWordAloneOnlyNudges(t *testing.T) {
	got, err := Heuristic{}.Route(Input{
		Prompt:     "refactor this one function name",
		Candidates: ladder(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Tier == "t3" {
		t.Errorf("a single breadth word jumped to the top of the ladder: %s", got.Rationale)
	}
}

// An infeasible target is not an expensive one. Ordering matters: a target
// excluded by policy must never be reported as one that was out-priced.
func TestFeasibilityIsCheckedBeforeEconomics(t *testing.T) {
	noVision := candidate("t1", 0, func(c *Candidate) { c.Info.Vision = false })
	noTools := candidate("t2", 1, func(c *Candidate) { c.Info.Tools = catalog.ToolsNone })
	tooSmall := candidate("t3", 2, func(c *Candidate) {
		c.Info.ContextWindow = 500
		c.PromptTokens = 90_000
	})
	fine := candidate("t4", 3)

	got, err := Heuristic{}.Route(Input{
		Prompt:       "describe this screenshot and fix the test",
		Candidates:   []Candidate{noVision, noTools, tooSmall, fine},
		Requirements: Requirements{NeedsVision: true, NeedsTools: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Tier != "t4" {
		t.Errorf("tier = %q, want the only feasible one", got.Tier)
	}

	joined := strings.Join(got.Infeasible, "\n")
	for _, want := range []string{"cannot read images", "cannot call tools", "holds 500 tokens"} {
		if !strings.Contains(joined, want) {
			t.Errorf("exclusions do not explain %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "ceiling") {
		t.Error("an infeasible target was reported as too expensive")
	}
}

// A destination that is not approved is infeasible, and no budget makes it
// feasible again.
func TestUnapprovedDestinationIsInfeasible(t *testing.T) {
	elsewhere := candidate("t1", 0)
	elsewhere.Target.Provider = "somewhere-else"

	_, err := Heuristic{}.Route(Input{
		Prompt:       "hello",
		Candidates:   []Candidate{elsewhere},
		Requirements: Requirements{ApprovedProviders: []string{"anthropic"}},
	})
	if err == nil {
		t.Fatal("an unapproved destination was routed to")
	}
	if !strings.Contains(err.Error(), "approved destination") {
		t.Errorf("err = %v", err)
	}
}

// A ceiling is checked against the upper bound. Using the expectation would
// approve a turn that is only affordable on average, which is not what a
// ceiling means.
func TestTheBudgetCeilingUsesTheUpperBound(t *testing.T) {
	pricey := candidate("t2", 1)
	pricey.Estimate = costmodel.Estimate{Expected: 100, High: 5_000}
	cheap := candidate("t1", 0)
	cheap.Estimate = costmodel.Estimate{Expected: 50, High: 200}

	got, err := Heuristic{}.Route(Input{
		Prompt:     "refactor the storage layer across the codebase",
		Session:    SessionFeatures{FilesInContext: 9},
		Candidates: []Candidate{cheap, pricey},
		Budgets:    Budgets{MaxCost: 1_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Tier != "t1" {
		t.Errorf("tier = %q; the expensive one exceeds the ceiling at its upper bound", got.Tier)
	}
	if !strings.Contains(strings.Join(got.Infeasible, " "), "ceiling") {
		t.Errorf("the exclusion did not name the ceiling: %v", got.Infeasible)
	}
}

// §8.1: a pin short-circuits selection only after the hard checks, and an
// infeasible pin is an actionable error rather than a quiet substitution.
func TestAPinIsHonouredButNotAboveThePolicyChecks(t *testing.T) {
	got, err := Heuristic{}.Route(Input{
		Prompt: "anything", Pin: "t3", Candidates: ladder(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Tier != "t3" || got.Source != SourceUserPin {
		t.Errorf("decision = %+v", got)
	}

	blind := candidate("t1", 0, func(c *Candidate) { c.Info.Vision = false })
	_, err = Heuristic{}.Route(Input{
		Prompt: "describe this image", Pin: "t1",
		Candidates:   []Candidate{blind},
		Requirements: Requirements{NeedsVision: true},
	})
	if err == nil {
		t.Fatal("an infeasible pin was served rather than refused")
	}
	if !strings.Contains(err.Error(), "pinned") {
		t.Errorf("err = %v; it has to say the pin was the problem", err)
	}
}

// Confidence is low by construction: these are rules over shallow features, and
// §8.2 composes the chain on confidence thresholds.
func TestConfidenceStaysModest(t *testing.T) {
	got, _ := Heuristic{}.Route(Input{Prompt: "hi", Candidates: ladder()})
	if got.Confidence > 0.7 {
		t.Errorf("confidence = %.2f; a rules router should not claim more", got.Confidence)
	}
}
