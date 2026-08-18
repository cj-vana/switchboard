package eval

import (
	"reflect"
	"testing"

	"github.com/switchboard-code/switchboard/internal/provider"
)

func TestEvalTargetParsersPreservePlusModelAndParameters(t *testing.T) {
	temperature := 0.25
	target := provider.RouteTarget{
		Provider: "openai", Surface: "api", ModelID: "vendor/model+preview%2B",
		Params: provider.Params{
			MaxOutputTokens: 2_048,
			Temperature:     &temperature,
			Reasoning:       &provider.Reasoning{Enabled: true, Effort: "high+care"},
		},
	}
	paretoTarget, err := targetFromID(target.ID())
	if err != nil {
		t.Fatal(err)
	}
	for name, parsed := range map[string]provider.RouteTarget{
		"runner pricing": targetOf(nil, target.ID()),
		"pareto pricing": paretoTarget,
	} {
		if !reflect.DeepEqual(parsed, target) {
			t.Errorf("%s parser = %#v, want %#v", name, parsed, target)
		}
	}

	legacy := provider.RouteTargetID("ollama/local/vendor/model+preview")
	paretoLegacy, err := targetFromID(legacy)
	if err != nil {
		t.Fatal(err)
	}
	for name, parsed := range map[string]provider.RouteTarget{
		"legacy runner pricing": targetOf(nil, legacy),
		"legacy pareto pricing": paretoLegacy,
	} {
		if parsed.ModelID != "vendor/model+preview" {
			t.Errorf("%s model = %q, want plus-bearing legacy model", name, parsed.ModelID)
		}
	}
}
