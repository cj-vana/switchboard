package breakpoint

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cjvana/switchboard/internal/catalog"
	"github.com/cjvana/switchboard/internal/prefix"
	"github.com/cjvana/switchboard/internal/provider"
)

// The four surfaces this build reaches, as the catalog records them.
func policyFor(t *testing.T, target provider.RouteTarget) catalog.CachePolicy {
	t.Helper()
	c, err := catalog.LoadBundled()
	if err != nil {
		t.Fatal(err)
	}
	info, _, ok := c.Lookup(target)
	if !ok {
		t.Fatalf("%s has no catalog entry", target.ID())
	}
	return info.Cache
}

func text(n int) string { return strings.Repeat("a word here and there ", n) }

func layoutWith(system string, docs int, history int) *prefix.Layout {
	l := prefix.New(
		[]provider.Block{provider.Text{Text: system}},
		[]provider.ToolDefinition{
			{Name: "read", Description: "Read a file", Schema: json.RawMessage(`{"type":"object"}`)},
			{Name: "write", Description: "Write a file", Schema: json.RawMessage(`{"type":"object"}`)},
		},
		0,
	)
	for i := range docs {
		l.Add(prefix.Document{
			Path:    string(rune('a'+i)) + ".go",
			Hash:    "h",
			Content: text(200),
		})
	}
	for range history {
		l.AppendHistory(provider.UserText(text(50)))
	}
	l.SetTail(provider.Text{Text: "the current instruction"})
	return l
}

// Anthropic takes explicit markers and ignores a prefix under its minimum.
func TestExplicitTargetPlacesMarkersAtZoneBoundaries(t *testing.T) {
	target := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-haiku-4-5"}
	m := &Manager{Policy: policyFor(t, target), Target: target.ID()}

	decision := m.Plan(layoutWith(text(600), 3, 4))

	if decision.Mode != catalog.CacheExplicit {
		t.Fatalf("mode = %q, want explicit", decision.Mode)
	}
	if decision.Placed() == 0 {
		t.Fatal("no markers were placed on a target that takes them")
	}
	if decision.Placed() > m.Policy.MaxBreakpoints {
		t.Errorf("placed %d markers, above the %d this target allows", decision.Placed(), m.Policy.MaxBreakpoints)
	}

	// Markers must arrive in prefix order, or the deepest is not the one that
	// covers the most.
	last := -3
	for _, bp := range decision.Plan.Breakpoints {
		if bp.Position.MessageIndex < last {
			t.Errorf("markers are out of prefix order: %+v", decision.Plan.Breakpoints)
		}
		last = bp.Position.MessageIndex
	}

	// The shortest retention is the default: a longer one bills more to write
	// and only pays back on reuse nothing has measured yet.
	if got := decision.Plan.Breakpoints[0].TTL; got != 5*time.Minute {
		t.Errorf("ttl = %s, want the shortest the target offers", got)
	}
}

// A marker below the minimum is accepted by the server and does nothing. The
// only way to tell that apart from a working one is to have declined it and
// said so.
func TestPrefixBelowTheMinimumIsDeclinedOutLoud(t *testing.T) {
	target := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-haiku-4-5"}
	m := &Manager{Policy: policyFor(t, target), Target: target.ID()}

	// Far below the 4,096 token minimum this target records.
	decision := m.Plan(layoutWith("terse", 0, 1))

	if decision.Placed() != 0 {
		t.Errorf("placed %d markers on a prefix too short to cache", decision.Placed())
	}
	if len(decision.Declined) == 0 {
		t.Fatal("nothing was recorded, so a silent miss would look like a working cache")
	}
	joined := strings.Join(decision.Declined, "\n")
	if !strings.Contains(joined, "4096") {
		t.Errorf("the reason does not name the minimum:\n%s", joined)
	}
}

// The ChatGPT plan caches on its own and takes a routing key. Sending markers
// would be at best ignored.
func TestAutomaticTargetGetsAKeyAndNoMarkers(t *testing.T) {
	target := provider.RouteTarget{Provider: "openai", Surface: "subscription", ModelID: "gpt-5.4-mini"}
	m := &Manager{Policy: policyFor(t, target), Target: target.ID()}

	decision := m.Plan(layoutWith(text(600), 3, 4))

	if decision.Placed() != 0 {
		t.Errorf("placed %d markers on a target that caches automatically", decision.Placed())
	}
	if decision.RoutingKey == "" {
		t.Fatal("no routing key for the one target that accepts one")
	}
	if len(decision.Declined) == 0 {
		t.Error("the reason markers were not placed was not recorded")
	}
}

// Kimi caches on its own and accepts no key either.
func TestAutomaticTargetWithoutKeySupportGetsNeither(t *testing.T) {
	target := provider.RouteTarget{Provider: "kimi", Surface: "coding", ModelID: "k3-256k"}
	m := &Manager{Policy: policyFor(t, target), Target: target.ID()}

	decision := m.Plan(layoutWith(text(600), 3, 4))

	if decision.Placed() != 0 {
		t.Errorf("placed %d markers", decision.Placed())
	}
	if decision.RoutingKey != "" {
		t.Error("a routing key was set for a target that does not accept one")
	}
}

// Ollama does not cache at all, and saying so beats reporting a miss later.
func TestTargetThatDoesNotCacheGetsNothing(t *testing.T) {
	target := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "qwen3.5:9b-mlx"}
	m := &Manager{Policy: policyFor(t, target), Target: target.ID()}

	decision := m.Plan(layoutWith(text(600), 3, 4))

	if decision.Placed() != 0 || decision.RoutingKey != "" {
		t.Errorf("something was placed for a target with no cache: %+v", decision)
	}
	if len(decision.Declined) == 0 {
		t.Error("no reason was recorded")
	}
}

// The key identifies a prefix to the server without describing it, and it is
// built from the target rather than the tier: two tiers on one model, or a
// fallback, would otherwise fragment or misattribute state.
func TestRoutingKeyFollowsThePrefixAndTheTarget(t *testing.T) {
	policy := catalog.CachePolicy{DefaultMode: catalog.CacheAutomatic, RoutingKeySupport: true}
	a := &Manager{Policy: policy, Target: "openai/subscription/gpt-5.4-mini"}
	b := &Manager{Policy: policy, Target: "openai/subscription/gpt-5.5"}

	layout := layoutWith(text(600), 2, 2)
	other := layoutWith(text(600), 2, 2)
	other.AppendHistory(provider.UserText("one more turn"))

	same := a.Plan(layout).RoutingKey
	if same != a.Plan(layout).RoutingKey {
		t.Error("the same prefix on the same target produced two keys")
	}
	if same == b.Plan(layout).RoutingKey {
		t.Error("two targets shared a key, so their cache state would be attributed to one")
	}
	if same == a.Plan(other).RoutingKey {
		t.Error("a changed prefix kept its key, so a miss would be reported as a hit")
	}

	// The content must not be recoverable from the key.
	if strings.Contains(same, "a word here") {
		t.Error("the key carries prefix content")
	}
}

// A target searches back only so many block positions for a reusable prefix,
// and §6.2 asks for a history marker before a growing turn crosses that.
//
// This placement makes the condition unreachable instead: the deepest boundary
// is always kept and the deepest boundary is always the end of history, so no
// marker is ever more than the tail away from where the search begins. The
// property is worth pinning because it stops holding the moment placement
// changes, and the failure it prevents is a total miss reported as an expired
// cache.
func TestPlacementNeverCrossesTheLookbackWindow(t *testing.T) {
	policy := catalog.CachePolicy{
		DefaultMode: catalog.CacheExplicit, MaxBreakpoints: 4,
		LookbackBlocks: 20, MinTokens: 0, TTLs: []string{"5m"},
	}
	m := &Manager{Policy: policy, Target: "test/surface/model", LookbackMargin: 4}

	for _, turns := range []int{1, 5, 40, 200} {
		l := prefix.New([]provider.Block{provider.Text{Text: "s"}}, nil, 0)
		for range turns {
			l.AppendHistory(provider.UserText("turn"))
		}
		l.SetTail(provider.Text{Text: "now"})

		decision := m.Plan(l)
		if m.CrossesLookback(l, decision) {
			t.Errorf("with %d turns of history the deepest marker fell outside the lookback window", turns)
		}
	}

	// And the check itself is not vacuous: with no marker at all, a long
	// history does cross.
	bare := prefix.New([]provider.Block{provider.Text{Text: "s"}}, nil, 0)
	for range 200 {
		bare.AppendHistory(provider.UserText("turn"))
	}
	if !m.CrossesLookback(bare, Decision{Plan: &provider.CachePlan{}}) {
		t.Error("the lookback check reports nothing even with no markers placed")
	}
}

// With one marker to spend, coverage beats stability: the deepest boundary is
// what the next turn reads back.
func TestASingleMarkerGoesToTheDeepestBoundary(t *testing.T) {
	policy := catalog.CachePolicy{
		DefaultMode: catalog.CacheExplicit, MaxBreakpoints: 1, TTLs: []string{"5m"},
	}
	m := &Manager{Policy: policy, Target: "test/surface/model"}

	layout := layoutWith(text(600), 3, 4)
	decision := m.Plan(layout)
	if decision.Placed() != 1 {
		t.Fatalf("placed %d markers, want 1", decision.Placed())
	}

	all := layout.Boundaries()
	if got, want := decision.Plan.Breakpoints[0].Position, all[len(all)-1].Position; got != want {
		t.Errorf("the single marker went to %+v, want the deepest boundary %+v", got, want)
	}
}

// Over the limit, the ends of the prefix are what survive: the first is the
// most stable and certain to be reused, the last covers the most.
func TestOverTheLimitTheEndsSurvive(t *testing.T) {
	policy := catalog.CachePolicy{
		DefaultMode: catalog.CacheExplicit, MaxBreakpoints: 2, TTLs: []string{"5m"},
	}
	m := &Manager{Policy: policy, Target: "test/surface/model"}

	decision := m.Plan(layoutWith(text(600), 3, 4))
	if decision.Placed() != 2 {
		t.Fatalf("placed %d markers, want the limit of 2", decision.Placed())
	}

	all := layoutWith(text(600), 3, 4).Boundaries()
	first, last := all[0].Position, all[len(all)-1].Position
	got := decision.Plan.Breakpoints

	if got[0].Position != first {
		t.Errorf("the shallowest boundary was dropped: %+v", got[0].Position)
	}
	if got[len(got)-1].Position != last {
		t.Errorf("the deepest boundary was dropped: %+v", got[len(got)-1].Position)
	}
	if len(decision.Declined) == 0 {
		t.Error("the dropped boundaries were not recorded")
	}
}

// Every marker has to address a position that exists, or it caches a different
// prefix than the one that was scored. The Anthropic adapter refuses one that
// does not, so this is what keeps the two agreeing.
func TestEveryMarkerAddressesARealPosition(t *testing.T) {
	target := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-haiku-4-5"}
	m := &Manager{Policy: policyFor(t, target), Target: target.ID()}

	layout := layoutWith(text(600), 3, 4)
	decision := m.Plan(layout)
	req := layout.Request()

	for _, bp := range decision.Plan.Breakpoints {
		pos := bp.Position
		switch pos.MessageIndex {
		case provider.SystemBlocks:
			if pos.BlockIndex >= len(req.System) {
				t.Errorf("system marker at block %d of %d", pos.BlockIndex, len(req.System))
			}
		case provider.ToolDefinitions:
			if pos.BlockIndex >= len(req.Tools) {
				t.Errorf("tool marker at block %d of %d", pos.BlockIndex, len(req.Tools))
			}
		default:
			if pos.MessageIndex >= len(req.Messages) {
				t.Fatalf("marker at message %d of %d", pos.MessageIndex, len(req.Messages))
			}
			if pos.BlockIndex >= len(req.Messages[pos.MessageIndex].Content) {
				t.Errorf("marker at block %d of message %d, which holds %d",
					pos.BlockIndex, pos.MessageIndex, len(req.Messages[pos.MessageIndex].Content))
			}
		}
	}
}

// The tail is rewritten every turn, so a marker on it would never be reused and
// would spend a write each time.
func TestTheTailIsNeverMarked(t *testing.T) {
	target := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-haiku-4-5"}
	m := &Manager{Policy: policyFor(t, target), Target: target.ID()}

	layout := layoutWith(text(600), 3, 4)
	tailIndex := len(layout.Request().Messages) - 1

	for _, bp := range m.Plan(layout).Plan.Breakpoints {
		if bp.Position.MessageIndex == tailIndex {
			t.Error("a marker landed on the volatile tail")
		}
	}
}
