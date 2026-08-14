package breakpoint

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cj-vana/switchboard/internal/catalog"
	"github.com/cj-vana/switchboard/internal/credential"
	"github.com/cj-vana/switchboard/internal/prefix"
	"github.com/cj-vana/switchboard/internal/provider"
	"github.com/cj-vana/switchboard/internal/provider/anthropic"
)

// The manager and the adapter have to agree about what a position means. The
// adapter refuses a marker it cannot place exactly, and the server accepts one
// that lands somewhere unintended without comment, so only a real request
// settles it.
func TestLivePlanIsAcceptedAndCaches(t *testing.T) {
	if os.Getenv("SB_LIVE") == "" {
		t.Skip("set SB_LIVE=1 to run against the live API (this spends money)")
	}
	secret, err := credential.Chain(credential.Settings{}).Get(
		context.Background(), credential.Ref{Provider: anthropic.Name, Account: anthropic.Surface})
	if err != nil {
		t.Skipf("no credential: %v", err)
	}
	client := anthropic.New(anthropic.WithAPIKey(secret.Expose()))

	target := anthropic.Target("claude-haiku-4-5")
	cat, err := catalog.LoadBundled()
	if err != nil {
		t.Fatal(err)
	}
	info, _, ok := cat.Lookup(target)
	if !ok {
		t.Fatal("no catalog entry")
	}
	m := &Manager{Policy: info.Cache, Target: target.ID()}

	// Clear of the 4,096 token minimum the catalog records for this target.
	layout := prefix.New(
		[]provider.Block{provider.Text{Text: strings.Repeat(
			"You are a precise assistant. Follow the tool schema exactly. "+
				"Do not invent file paths. Prefer the smallest correct answer. ", 420)}},
		nil, 0,
	)
	layout.AppendHistory(provider.UserText("Reply with the single word OK."))
	layout.SetTail(provider.Text{Text: "Answer now."})

	decision := m.Plan(layout)
	if decision.Placed() == 0 {
		t.Fatalf("no markers were planned: %v", decision.Declined)
	}

	req := layout.Request()
	req.CachePlan = decision.Plan

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	usage := func() provider.Usage {
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

	first := usage()
	if first.CacheWriteTokens == 0 && first.CacheReadTokens == 0 {
		t.Fatalf("the markers were accepted and cached nothing: %+v\n"+
			"a marker below the minimum is accepted and silently ineffective, "+
			"which is the failure this manager exists to avoid", first)
	}

	second := usage()
	if second.CacheReadTokens == 0 {
		t.Errorf("a second identical request read nothing back: %+v", second)
	}

	// The lookback property, checked against a real placement rather than only
	// a constructed one.
	if m.CrossesLookback(layout, decision) {
		t.Error("the deepest marker fell outside the lookback window")
	}
}
