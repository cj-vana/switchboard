package main

import (
	"testing"

	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/provider"
)

func TestApproverCandidatesUseExplicitSlotOrLowestTier(t *testing.T) {
	t1 := config.Tier{ID: "t1", Target: provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "small"}}
	t2 := config.Tier{ID: "t2", Target: provider.RouteTarget{Provider: "openai", Surface: "api", ModelID: "large"}}
	cfg := &config.Config{Tiers: []config.Tier{t1, t2}, Slots: map[string]string{}}
	got, explicit, err := approverCandidates(cfg, config.Tier{})
	if err != nil || explicit || len(got) != 2 || got[0].ID != "t1" {
		t.Fatalf("default candidates=%+v explicit=%v err=%v", got, explicit, err)
	}

	cfg.Slots["approver"] = "t2"
	got, explicit, err = approverCandidates(cfg, config.Tier{})
	if err != nil || !explicit || len(got) != 1 || got[0].ID != "t2" {
		t.Fatalf("slot candidates=%+v explicit=%v err=%v", got, explicit, err)
	}

	cfg.Slots["approver"] = "anthropic/cheap"
	got, explicit, err = approverCandidates(cfg, config.Tier{})
	if err != nil || !explicit || len(got) != 1 || got[0].Target.ModelID != "cheap" {
		t.Fatalf("direct slot candidates=%+v explicit=%v err=%v", got, explicit, err)
	}
}

func TestApproverCandidatesRejectBadExplicitSlot(t *testing.T) {
	cfg := &config.Config{Slots: map[string]string{"approver": "not-a-target"}}
	if _, _, err := approverCandidates(cfg, config.Tier{}); err == nil {
		t.Fatal("bad explicit approver slot accepted")
	}
}
