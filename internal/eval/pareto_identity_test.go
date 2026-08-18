package eval

import (
	"strings"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/provider"
)

func TestDeriveFrontUnknownIdentityDisablesCostComparisons(t *testing.T) {
	cat, err := catalog.LoadBundled()
	if err != nil {
		t.Fatal(err)
	}
	known := provider.RouteTargetID("anthropic/first-party/claude-opus-5")
	legacyThinking := provider.RouteTargetID("anthropic/first-party/claude-opus-5+think:high")
	front := DeriveFront([]Run{
		{Target: known, Arm: "fixed", Solved: true, Cost: 100, Duration: 10 * time.Millisecond},
		{Target: legacyThinking, Arm: "fixed", Solved: true, Cost: 1, Duration: 20 * time.Millisecond},
	}, cat, 1)

	if len(front.Points) != 2 {
		t.Fatalf("placed points = %d, want 2: %+v", len(front.Points), front.Points)
	}
	for _, point := range front.Points {
		if len(point.Dominated) != 0 {
			t.Errorf("unknown metering allowed cost domination of %q by %v", point.Target, point.Dominated)
		}
	}
	if len(front.Ladder) != 2 || front.Ladder[0] != known || front.Ladder[1] != legacyThinking {
		t.Fatalf("ladder = %v, want solve/latency order [%s %s] rather than cost order",
			front.Ladder, known, legacyThinking)
	}
	warnings := strings.Join(front.Warnings, "\n")
	for _, want := range []string{string(legacyThinking), "metering is unknown", "not comparable"} {
		if !strings.Contains(warnings, want) {
			t.Errorf("warnings = %q, missing %q", warnings, want)
		}
	}
}

func TestDeriveFrontWarnsForHistoricIdentityAcrossMeteringKinds(t *testing.T) {
	cat, err := catalog.LoadBundled()
	if err != nil {
		t.Fatal(err)
	}
	targets := []provider.RouteTargetID{
		"ollama/local/qwen-test",
		"openai/subscription/gpt-test",
		"anthropic/first-party/claude-opus-5",
		"anthropic/first-party/claude-opus-5+think:high",
	}
	runs := make([]Run, 0, len(targets))
	for index, target := range targets {
		runs = append(runs, Run{
			Target: target, Arm: "fixed", Solved: true,
			Cost: catalog.Money(index + 1), Duration: time.Duration(index+1) * time.Millisecond,
		})
	}
	front := DeriveFront(runs, cat, 1)
	if len(front.Points) != len(targets) {
		t.Fatalf("placed points = %d, want %d", len(front.Points), len(targets))
	}
	seen := map[catalog.Metering]bool{}
	for _, point := range front.Points {
		seen[point.Metering] = true
		if len(point.Dominated) != 0 {
			t.Errorf("mixed/unknown metering allowed domination of %q by %v", point.Target, point.Dominated)
		}
	}
	for _, metering := range []catalog.Metering{catalog.Local, catalog.Plan, catalog.PerToken} {
		if !seen[metering] {
			t.Errorf("missing %s point: %+v", metering, front.Points)
		}
	}
	warnings := strings.Join(front.Warnings, "\n")
	if !strings.Contains(warnings, string(targets[len(targets)-1])) || !strings.Contains(warnings, "metered differently") {
		t.Fatalf("warnings do not identify historic identity and mixed meterings: %q", warnings)
	}
}
