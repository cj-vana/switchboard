package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cj-vana/switchboard/internal/config"
	"github.com/cj-vana/switchboard/internal/provider"
)

// fakeOllama serves a tags list holding only the named models, each
// advertising tool support, which is what the probe needs to accept one.
func fakeOllama(t *testing.T, models ...string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			var entries []string
			for _, m := range models {
				entries = append(entries, `{"name":"`+m+`"}`)
			}
			w.Write([]byte(`{"models":[` + strings.Join(entries, ",") + `]}`))
		case "/api/show":
			w.Write([]byte(`{"capabilities":["tools"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func ollamaTier(id, model string, fallbacks ...string) config.Tier {
	tier := config.Tier{
		ID:     id,
		Target: provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: model},
	}
	for _, fb := range fallbacks {
		tier.Fallbacks = append(tier.Fallbacks, provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: fb})
	}
	return tier
}

func TestProbeTierFallbackServesTheFirstAnswer(t *testing.T) {
	ts := fakeOllama(t, "backup")
	p := newProviders(ts.URL, &config.Config{})

	tier, _, note, err := p.probeTierFallback(context.Background(), ollamaTier("t2", "missing", "also-missing", "backup"))
	if err != nil {
		t.Fatal(err)
	}
	if tier.Target.ModelID != "backup" {
		t.Fatalf("served %s, want the fallback that answered", tier.Target.ID())
	}
	if tier.ID != "t2" {
		t.Errorf("tier ID = %s, want the rung unchanged: fallback is availability, not routing", tier.ID)
	}
	if !strings.Contains(note, "t2 is served by its fallback") || !strings.Contains(note, "missing") {
		t.Errorf("note = %q, want the substitution and its reason named", note)
	}
}

func TestProbeTierFallbackStaysQuietWhenThePrimaryServes(t *testing.T) {
	ts := fakeOllama(t, "primary")
	p := newProviders(ts.URL, &config.Config{})

	tier, _, note, err := p.probeTierFallback(context.Background(), ollamaTier("t1", "primary", "backup"))
	if err != nil {
		t.Fatal(err)
	}
	if tier.Target.ModelID != "primary" || note != "" {
		t.Errorf("served %s with note %q; the fallback list must not be consulted when the primary answers",
			tier.Target.ID(), note)
	}
}

func TestProbeTierFallbackReportsEveryAttempt(t *testing.T) {
	ts := fakeOllama(t) // a server with nothing pulled
	p := newProviders(ts.URL, &config.Config{})

	_, _, _, err := p.probeTierFallback(context.Background(), ollamaTier("t1", "missing", "backup"))
	if err == nil {
		t.Fatal("nothing was servable, but no error came back")
	}
	for _, want := range []string{"missing", "backup", "all unavailable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q; the user needs every attempt named", err, want)
		}
	}
}
