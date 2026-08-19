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
