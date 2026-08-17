package main

import (
	"strings"
	"testing"

	"github.com/cj-vana/switchboard/internal/config"
	"github.com/cj-vana/switchboard/internal/provider"
	"github.com/cj-vana/switchboard/internal/session"
)

func ladderFixtureTiers() []config.Tier {
	return []config.Tier{
		{ID: "t1", Label: "light", Target: provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "qwen3:4b"}},
		{ID: "t2", Label: "deep", Target: provider.RouteTarget{Provider: "kimi", Surface: "api", ModelID: "kimi-for-coding"}},
	}
}

func appendRoute(t *testing.T, sess *session.Session, rec session.Route) {
	t.Helper()
	if err := sess.AppendRoute(rec); err != nil {
		t.Fatal(err)
	}
}

// The sum answers the ladder's own question — does work that starts low
// stay low — per rung, with a move named by the rung that serves its
// destination today and the §8.4 caveats stated in the output.
func TestLadderSumsTurnsWhereTheyOpened(t *testing.T) {
	tiers := ladderFixtureTiers()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()

	sess, err := store.Create(workspace, tiers[0].Target.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	appendRoute(t, sess, session.Route{Tier: "t1", Target: tiers[0].Target.ID(), Outcome: "completed"})
	appendRoute(t, sess, session.Route{Tier: "t1", Target: tiers[0].Target.ID(), Outcome: "completed",
		Escalations: 1, EndedOn: tiers[1].Target.ID(), TurnDepth: 2})
	appendRoute(t, sess, session.Route{Tier: "t1", Target: tiers[0].Target.ID(), Outcome: "abandoned", TurnDepth: 4})
	appendRoute(t, sess, session.Route{Tier: "t2", Target: tiers[1].Target.ID(), Outcome: "completed", TurnDepth: 6})
	sess.Close()

	out := strings.Join(ladderLines(tiers, store, workspace), "\n")

	if !strings.Contains(out, "4 turns across 1 session") {
		t.Errorf("the header must count the record:\n%s", out)
	}
	if !strings.Contains(out, "t1") || !strings.Contains(out, "3 turns · stayed 1 · abandoned 1") {
		t.Errorf("t1's sum is wrong:\n%s", out)
	}
	if !strings.Contains(out, "moved to t2 (kimi/api/kimi-for-coding) ×1") {
		t.Errorf("the move must name today's rung for its destination:\n%s", out)
	}
	if !strings.Contains(out, "not a verdict") {
		t.Errorf("the §8.4 caveat is not stated:\n%s", out)
	}
	if !strings.Contains(out, "/races") || !strings.Contains(out, "/blame") || !strings.Contains(out, "/stats") {
		t.Errorf("the neighbors that hold the other halves must be named:\n%s", out)
	}
}

// A fork's copied route records are one turn's evidence, not two: the same
// mechanism every aggregate reader here uses, applied to routing.
func TestLadderCountsAForkCopiedRouteOnce(t *testing.T) {
	tiers := ladderFixtureTiers()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()

	src, err := store.Create(workspace, tiers[0].Target.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := src.AppendMessage(provider.UserText("hello")); err != nil {
		t.Fatal(err)
	}
	appendRoute(t, src, session.Route{Tier: "t1", Target: tiers[0].Target.ID(), Outcome: "completed"})
	src.Close()

	fork, err := store.Fork(src.State().ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	fork.Close()

	out := strings.Join(ladderLines(tiers, store, workspace), "\n")
	if !strings.Contains(out, "1 turn across 1 session") {
		t.Errorf("a copied route counted twice:\n%s", out)
	}
}

// A rung the record holds but today's ladder does not name still renders,
// said plainly, because history priced against a config that changed is
// still history.
func TestLadderNamesRungsTheLadderNoLongerHolds(t *testing.T) {
	tiers := ladderFixtureTiers()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()

	sess, err := store.Create(workspace, tiers[0].Target.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	appendRoute(t, sess, session.Route{Tier: "t9", Target: "openai/api/gone-model", Outcome: "completed"})
	sess.Close()

	out := strings.Join(ladderLines(tiers, store, workspace), "\n")
	if !strings.Contains(out, "t9") || !strings.Contains(out, "does not name") {
		t.Errorf("the stray rung must render and say what it is:\n%s", out)
	}
	if !strings.Contains(out, "no recorded turns opened here") {
		t.Errorf("a configured rung with no history must say so:\n%s", out)
	}
}

func TestLadderWithNoRoutesSaysSo(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out := strings.Join(ladderLines(ladderFixtureTiers(), store, t.TempDir()), "\n")
	if !strings.Contains(out, "no routed turns recorded") {
		t.Errorf("an empty record did not say so: %s", out)
	}
}
