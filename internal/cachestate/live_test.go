package cachestate

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cj-vana/switchboard/internal/breakpoint"
	"github.com/cj-vana/switchboard/internal/catalog"
	"github.com/cj-vana/switchboard/internal/credential"
	"github.com/cj-vana/switchboard/internal/prefix"
	"github.com/cj-vana/switchboard/internal/provider"
	"github.com/cj-vana/switchboard/internal/provider/anthropic"
)

// The whole §6 chain against a real provider: zones lay out a prefix, the
// manager places markers, the adapter renders them, and the tracker records
// what came back. Each piece is tested alone; this is the one that fails if any
// two of them disagree about what a prefix is.
//
// It is also the shape of the 2a exit gate, which asks whether observed hit
// rates match what the policy expected.
func TestLiveBeliefMatchesWhatTheProviderDoes(t *testing.T) {
	if os.Getenv("SB_LIVE") == "" {
		t.Skip("set SB_LIVE=1 to run against the live API (this spends money)")
	}
	secret, err := credential.Chain(credential.Settings{}).Get(
		context.Background(), credential.Ref{Provider: anthropic.Name, Account: anthropic.Surface})
	if err != nil {
		t.Skipf("no credential: %v", err)
	}

	cat, err := catalog.LoadBundled()
	if err != nil {
		t.Fatal(err)
	}
	target := anthropic.Target("claude-haiku-4-5")
	info, _, ok := cat.Lookup(target)
	if !ok {
		t.Fatal("no catalog entry")
	}

	client := anthropic.New(anthropic.WithAPIKey(secret.Expose()))
	manager := &breakpoint.Manager{Policy: info.Cache, Target: target.ID()}
	tracker := New()

	layout := prefix.New(
		[]provider.Block{provider.Text{Text: strings.Repeat(
			"You are a precise assistant. Follow the tool schema exactly. "+
				"Do not invent file paths. Prefer the smallest correct answer. ", 420)}},
		nil, 0,
	)
	layout.AppendHistory(provider.UserText("Reply with the single word OK."))
	layout.SetTail(provider.Text{Text: "Answer now."})

	decision := manager.Plan(layout)
	if decision.Placed() == 0 {
		t.Fatalf("nothing was planned: %v", decision.Declined)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	send := func() provider.Usage {
		req := layout.Request()
		req.CachePlan = decision.Plan
		s, err := client.Stream(ctx, target, req)
		if err != nil {
			t.Fatalf("the plan was refused: %v", err)
		}
		defer s.Close()
		var last provider.Usage
		for {
			ev, err := s.Next()
			if errors.Is(err, io.EOF) {
				return last
			}
			if err != nil {
				t.Fatalf("draining: %v", err)
			}
			if ev.Type == provider.EventDone {
				last = ev.Usage
			}
		}
	}

	record := func(usage provider.Usage, when time.Time) Entry {
		return tracker.Observe(Observation{
			Target:          target.ID(),
			PrefixHash:      layout.PrefixHash(),
			Usage:           usage,
			At:              when,
			Accounting:      info.Cache.UsageAccounting,
			Eligible:        decision.Placed() > 0,
			MinimumTTL:      5 * time.Minute,
			CatalogRevision: cat.Revision,
		})
	}

	// Before anything is sent the tracker must not believe in a cache.
	if before := tracker.Expect(target.ID(), layout.PrefixHash(), time.Now()); before.HitProbability != 0 {
		t.Errorf("believed in a cache before sending anything: %+v", before)
	}

	first := record(send(), time.Now())
	if first.State != WriteObserved && first.State != ReadObserved {
		t.Fatalf("the first turn observed no cache activity: %+v", first)
	}

	// Now the tracker should expect a hit, and the provider should deliver one.
	expectation := tracker.Expect(target.ID(), layout.PrefixHash(), time.Now())
	if expectation.HitProbability < 0.5 {
		t.Errorf("after a write the tracker expected %.2f, which is not a belief in a cache it just watched being made",
			expectation.HitProbability)
	}

	second := record(send(), time.Now())
	if second.State != ReadObserved {
		t.Errorf("the tracker expected a hit at %.2f and the provider did not deliver one: %+v",
			expectation.HitProbability, second)
	}

	health := tracker.Health(target.ID())
	if health.Alarm {
		t.Errorf("a working cache raised an alarm: %s", health.Detail)
	}
	if health.EligibleRequests != 2 || health.Hits != 1 {
		t.Errorf("health = %+v, want two eligible requests and one hit", health)
	}
	t.Logf("observed hit rate %.2f over %d eligible requests", health.HitRate(), health.EligibleRequests)
}
