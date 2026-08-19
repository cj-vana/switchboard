package main

import (
	"os"
	"testing"

	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/provider"
)

// The REPL had no compaction of either kind, so a long session there ran until
// the provider refused a request: the failure mode the design calls out, where
// the end arrives as an error rather than as a visible handoff.
func TestREPLAutoCompactsAtTheThreshold(t *testing.T) {
	m := testModel(t)
	r := &repl{
		loop:      m.app.loop,
		out:       newRenderer(os.NewFile(0, os.DevNull)),
		config:    m.app.config,
		catalog:   m.app.catalog,
		providers: newProviders("http://127.0.0.1:1", m.app.config),
		tier:      m.app.tier,
	}
	r.config.CompactAuto = true
	r.config.CompactAtPercent = 85

	// No window means no threshold to be past, so nothing fires. That is the
	// same gate the TUI has, and the reason declaring one matters.
	r.ctxWindow, r.callTokens = 0, 100000
	if r.shouldCompactNow() {
		t.Fatal("compaction was measured against a window nobody stated")
	}

	// Below the threshold nothing fires; at or past it, it does.
	r.ctxWindow, r.callTokens = 10000, 8000
	if r.shouldCompactNow() {
		t.Fatal("compaction fired at 80% of a window whose threshold is 85%")
	}
	r.callTokens = 8500
	if !r.shouldCompactNow() {
		t.Fatal("compaction did not fire at the threshold")
	}

	// Off is off, however full it gets.
	r.config.CompactAuto = false
	if r.shouldCompactNow() {
		t.Fatal("compaction fired with auto-compaction turned off")
	}
}

// The window resolves from the same three sources, in the same order, as the
// TUI's: the server, then the user, then the catalog.
func TestREPLResolvesTheContextWindowLikeTheTUI(t *testing.T) {
	m := testModel(t)
	cfg := m.app.config
	r := &repl{
		loop:      m.app.loop,
		out:       newRenderer(os.NewFile(0, os.DevNull)),
		config:    cfg,
		catalog:   m.app.catalog,
		providers: newProviders("http://127.0.0.1:1", cfg),
		tier:      m.app.tier,
	}
	target := provider.RouteTarget{Provider: "openaicompat", Surface: "generic", ModelID: "local"}
	r.loop.Target = target

	r.refreshCtxWindow()
	if r.ctxWindow != 0 {
		t.Fatalf("nothing knows this window, got %d", r.ctxWindow)
	}
	cfg.SetProviderContextWindow(config.ProviderSurfaceKey("openaicompat", "generic"), 32768)
	r.refreshCtxWindow()
	if r.ctxWindow != 32768 {
		t.Fatalf("a declared window resolved to %d", r.ctxWindow)
	}
}
