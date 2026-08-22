package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/config"
	route "github.com/switchboard-code/switchboard/internal/router"
)

// The policy moving the primary mid-task is the product's central bet, and a
// bet is something a user gets to decline.
func TestRoutingOffStopsMovesAndSurvivesTheSession(t *testing.T) {
	m := testModel(t)
	m.app.config.Path = filepath.Join(t.TempDir(), config.FileName)
	m.app.sticky = route.NewSticky(route.Policy{}, 0)

	moved := 0
	m.app.watcher = newWatcher(nil, m.app.sticky, 3,
		func(ctx context.Context, rank int, why string) (func() bool, func(), bool) {
			moved++
			return func() bool { return true }, nil, true
		})

	if !m.app.config.RouteAutoOn() {
		t.Fatal("routing is on unless the user says otherwise")
	}

	cmd := cmdRouting(m, "off")
	if n, ok := cmd().(noticeMsg); !ok || n.level == "error" {
		t.Fatalf("/routing off failed: %#v", cmd())
	}
	if !m.app.watcher.isPaused() {
		t.Fatal("the watcher is still allowed to move the primary")
	}

	// Evidence enough to escalate, which now changes nothing.
	for i := 0; i < 12; i++ {
		m.app.watcher.observe([]route.Signal{route.RepeatedToolCall, route.ToolErrorSpike})
		m.app.watcher.assess(context.Background())
	}
	if moved != 0 {
		t.Fatalf("routing off still moved the primary %d times", moved)
	}

	// The choice outlives the process.
	saved, err := config.LoadFile(m.app.config.Path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.RouteAutoOn() {
		t.Fatal("routing off did not persist")
	}

	// And it is reversible.
	if n, ok := cmdRouting(m, "on")().(noticeMsg); !ok || n.level == "error" {
		t.Fatalf("/routing on failed: %#v", n)
	}
	if m.app.watcher.isPaused() {
		t.Fatal("routing on left the watcher paused")
	}
	if standing := m.routingStanding(); !strings.Contains(standing, "routing is on") {
		t.Errorf("status = %q", standing)
	}
}

// A relief substitutes another rung mid-turn, which is exactly the move
// routing off reserves for the user.
func TestRoutingOffRefusesRelief(t *testing.T) {
	m := testModel(t)
	off := false
	m.app.config.RouteAuto = &off

	_, _, err := m.app.relief(context.Background(), agent.ReliefAvailability, errors.New("the target stopped answering"))
	if err == nil || !strings.Contains(err.Error(), "routing is off") {
		t.Fatalf("relief with routing off = %v, want a refusal naming the setting", err)
	}
}
