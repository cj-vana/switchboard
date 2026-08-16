package main

import (
	"strings"
	"testing"

	"github.com/cj-vana/switchboard/internal/provider"
	"github.com/cj-vana/switchboard/internal/session"
)

func TestRacesCLITalliesVerdictsAndDeduplicatesForks(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()

	record := func(race session.Race) *session.Session {
		t.Helper()
		sess, err := store.Create(workspace, "scripted/local/test", "rev")
		if err != nil {
			t.Fatal(err)
		}
		if err := sess.AppendMessage(provider.UserText("prompt")); err != nil {
			t.Fatal(err)
		}
		if err := sess.AppendRace(race); err != nil {
			t.Fatal(err)
		}
		return sess
	}

	won := record(session.Race{
		A: session.RaceArm{Tier: "t1", SessionID: "ra1"}, B: session.RaceArm{Tier: "t2", SessionID: "rb1"},
		Outcome: "b", Kept: "t2",
	})
	record(session.Race{
		A: session.RaceArm{Tier: "t1", SessionID: "ra2"}, B: session.RaceArm{Tier: "t2", SessionID: "rb2"},
		Outcome: "tie", Kept: "t1",
	}).Close()
	record(session.Race{
		A: session.RaceArm{Tier: "t2", SessionID: "ra3"}, B: session.RaceArm{Tier: "t3", SessionID: "rb3"},
		Outcome: "abandoned",
	}).Close()

	// A fork copies the winning log's records, race verdict included; the
	// tally must count the trial once, not once per log that carries it.
	fork, err := store.Fork(won.ID(), 1)
	if err != nil {
		t.Fatal(err)
	}
	fork.Close()
	won.Close()

	var b strings.Builder
	if err := runRacesCLI(&b, store, workspace); err != nil {
		t.Fatal(err)
	}
	out := b.String()

	for _, want := range []string{
		"t1 vs t2",
		"2 raced",
		"t2 picked 1",
		"both sufficed 1",
		"t2 vs t3",
		"censored 1",
		"3 races",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("races summary missing %q:\n%s", want, out)
		}
	}
}

func TestRacesCLISaysSoWhenEmpty(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := runRacesCLI(&b, store, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "no races recorded") {
		t.Errorf("an empty workspace did not say so:\n%s", b.String())
	}
}
