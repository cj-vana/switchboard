package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/provider"
)

// The words /think offers have to be the words /models will bind, or the two
// disagree about the same target. xhigh is the one that used to fall through
// the gap: priced on the current Opus and Sonnet models, bindable from
// /models, and refused here.
func TestThinkOffersTheTargetsOwnEffortLevels(t *testing.T) {
	cat, err := catalog.LoadBundled()
	if err != nil {
		t.Fatal(err)
	}
	m := testModel(t)
	m.app.catalog = cat
	m.app.tier.Target = provider.RouteTarget{
		Provider: "anthropic", Surface: "first-party", ModelID: "claude-opus-5",
	}

	levels, fromCatalog := m.thinkLevelsFor(m.app.tier.Target)
	if !fromCatalog {
		t.Fatal("the bundled catalog prices this target but reported no effort levels")
	}
	if !slices.Contains(levels, "xhigh") {
		t.Errorf("levels = %v, want xhigh among them", levels)
	}

	if cmd := cmdThink(m, ""); cmd != nil {
		t.Fatalf("opening the picker returned a command: %v", cmd)
	}
	dlg, ok := m.dlg.(*pickerDialog)
	if !ok {
		t.Fatalf("dlg = %T, want a picker", m.dlg)
	}
	offered := make([]string, 0, len(dlg.items))
	for _, item := range dlg.items {
		offered = append(offered, item.id)
	}
	if !slices.Contains(offered, "xhigh") {
		t.Errorf("the picker offered %v, which is not what the catalog prices", offered)
	}
	if !slices.Contains(offered, "default") {
		t.Errorf("the picker offered %v, with no way back to the provider's default", offered)
	}
}

// A target nobody priced still gets a usable picker, and the refusal says the
// list is this command's floor rather than the model's answer.
func TestThinkFallsBackWhenNothingPricedTheTarget(t *testing.T) {
	m := testModel(t)
	levels, fromCatalog := m.thinkLevelsFor(m.app.tier.Target)
	if fromCatalog {
		t.Fatal("an empty catalog claimed to know this target's effort levels")
	}
	if len(levels) == 0 {
		t.Fatal("the fallback list is empty, so the picker would have nothing in it")
	}

	cmd := m.applyThink("xhigh")
	if cmd == nil {
		t.Fatal("an effort this command does not take was accepted silently")
	}
	notice, ok := cmd().(noticeMsg)
	if !ok {
		t.Fatalf("msg = %T, want a notice", cmd())
	}
	if !strings.Contains(notice.text, "no effort levels are recorded") {
		t.Errorf("refusal = %q, which reads as the target's word rather than this command's", notice.text)
	}
}

// The subscription surface's catalog floor stops at high, but the endpoint's
// own list for the running model does not: Daybreak Blue takes xhigh, max,
// and ultra. What the server stated at probe time is the target's answer and
// wins over the floor.
func TestThinkOffersWhatTheServerStated(t *testing.T) {
	cat, err := catalog.LoadBundled()
	if err != nil {
		t.Fatal(err)
	}
	target := provider.RouteTarget{Provider: "openai", Surface: "subscription", ModelID: "gpt-daybreak-blue-latest"}
	// The fixture's point is that the probed list does the work: the
	// catalog's floor for this surface must not already contain the levels
	// under test, or the test passes without the probe path.
	info, _, ok := cat.Lookup(target)
	if !ok || slices.Contains(info.EffortLevels, "xhigh") {
		t.Fatalf("the catalog floor %v no longer isolates the probed answer", info.EffortLevels)
	}

	m := testModel(t)
	m.app.catalog = cat
	m.app.tier.Target = target
	stated := []string{"low", "medium", "high", "xhigh", "max", "ultra"}
	m.app.providers = &providers{efforts: map[string][]string{effortKey(target): stated}}

	levels, fromTarget := m.thinkLevelsFor(target)
	if !fromTarget {
		t.Fatal("a probed target reported no effort levels")
	}
	if !slices.Equal(levels, stated) {
		t.Errorf("levels = %v, want the server's own list %v", levels, stated)
	}

	if cmd := cmdThink(m, ""); cmd != nil {
		t.Fatalf("opening the picker returned a command: %v", cmd)
	}
	dlg, ok := m.dlg.(*pickerDialog)
	if !ok {
		t.Fatalf("dlg = %T, want a picker", m.dlg)
	}
	offered := make([]string, 0, len(dlg.items))
	for _, item := range dlg.items {
		offered = append(offered, item.id)
	}
	for _, level := range []string{"xhigh", "max", "ultra"} {
		if !slices.Contains(offered, level) {
			t.Errorf("the picker offered %v, which is not what the server stated", offered)
		}
	}
}

// Changing the effort rebinds the target under a parameterized identity, and
// the levels the server stated for the model do not move with it.
func TestThinkLevelsSurviveAnEffortChange(t *testing.T) {
	base := provider.RouteTarget{Provider: "openai", Surface: "subscription", ModelID: "gpt-daybreak-blue-latest"}
	stated := []string{"low", "medium", "high", "xhigh", "max", "ultra"}

	m := testModel(t)
	m.app.providers = &providers{efforts: map[string][]string{effortKey(base): stated}}

	moved := base
	moved.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: "high"}
	if moved.ID() == base.ID() {
		t.Fatal("an effort change did not change the target identity; the test proves nothing")
	}
	levels, fromTarget := m.thinkLevelsFor(moved)
	if !fromTarget || !slices.Equal(levels, stated) {
		t.Errorf("levels = %v (fromTarget %v), want %v under the rebound identity", levels, fromTarget, stated)
	}
}
