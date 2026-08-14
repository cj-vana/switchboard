// Package breakpoint owns cache-marker placement.
//
// §6.2 asks for a single component that does this, and the reason is that the
// four surfaces this build reaches want four different things. Anthropic takes
// explicit markers, at most four, and ignores any prefix under 4,096 tokens.
// The ChatGPT plan caches on its own and takes a routing key instead. Kimi
// caches on its own and takes nothing. Ollama does not cache at all and reports
// nothing either way.
//
// Spread that across the call sites and every one of them grows a different
// wrong assumption. Here it is one decision with the policy as data.
//
// The failure this exists to prevent is silent. A marker below a target's
// minimum is accepted and does nothing, with no error in either direction, so
// the only way to know is to have declined it deliberately and said so.
package breakpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"github.com/cj-vana/switchboard/internal/catalog"
	"github.com/cj-vana/switchboard/internal/prefix"
	"github.com/cj-vana/switchboard/internal/provider"
)

// Manager places markers for one route target.
type Manager struct {
	Policy catalog.CachePolicy
	Target provider.RouteTargetID

	// LookbackMargin is how many block positions of headroom to keep inside the
	// target's lookback window before adding a history marker.
	//
	// §6.2 is explicit that this is a policy parameter and not a universal
	// constant, because the right value depends on how fast a turn grows and
	// the cost of being one block late is a total miss.
	LookbackMargin int
}

// DefaultLookbackMargin leaves room for one tool-use round, which is the unit a
// turn actually grows by: an assistant block carrying the call, and a result.
const DefaultLookbackMargin = 4

// Decision is what the manager concluded, including what it refused to do.
//
// Declined is not a debug aid. A marker that silently does nothing is
// indistinguishable from one that worked, so the reasons are the only evidence
// that a miss was expected rather than a bug (§6.6).
type Decision struct {
	Plan       *provider.CachePlan
	RoutingKey string
	Mode       catalog.CacheMode
	Declined   []string
}

// Placed reports how many markers were emitted.
func (d Decision) Placed() int {
	if d.Plan == nil {
		return 0
	}
	return len(d.Plan.Breakpoints)
}

func (m *Manager) margin() int {
	if m.LookbackMargin > 0 {
		return m.LookbackMargin
	}
	return DefaultLookbackMargin
}

// Plan decides where markers go for the layout as it currently stands.
func (m *Manager) Plan(layout *prefix.Layout) Decision {
	mode := m.mode()
	decision := Decision{Mode: mode}

	switch mode {
	case catalog.CacheNone:
		decision.Declined = append(decision.Declined,
			fmt.Sprintf("%s does not cache, so nothing was placed", m.Target))
		return decision

	case catalog.CacheAutomatic, catalog.CacheImplicit:
		// The server decides what to keep. Sending markers would be at best
		// ignored, and the useful thing a caller can supply is an affinity key.
		decision.Declined = append(decision.Declined,
			fmt.Sprintf("%s caches automatically, so markers are not the mechanism", m.Target))
		decision.RoutingKey = m.routingKey(layout)
		return decision
	}

	candidates := m.candidates(layout, &decision)
	if len(candidates) == 0 {
		decision.RoutingKey = m.routingKey(layout)
		return decision
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].Position.MessageIndex < candidates[j].Position.MessageIndex
	})

	plan := &provider.CachePlan{}
	for _, c := range candidates {
		plan.Breakpoints = append(plan.Breakpoints, provider.Breakpoint{
			Position: c.Position,
			TTL:      m.ttl(),
		})
	}
	decision.Plan = plan
	decision.RoutingKey = m.routingKey(layout)
	return decision
}

// candidates picks the boundaries worth marking, and records what it turned
// down and why.
func (m *Manager) candidates(layout *prefix.Layout, decision *Decision) []prefix.Boundary {
	var eligible []prefix.Boundary

	for _, b := range layout.Boundaries() {
		if m.Policy.MinTokens > 0 && b.TokensBefore < m.Policy.MinTokens {
			// Accepted by the server and silently ineffective, which is the
			// case §6.2 says to log rather than emit.
			decision.Declined = append(decision.Declined, fmt.Sprintf(
				"the %s boundary covers about %d tokens, below the %d this target will cache",
				b.Zone, b.TokensBefore, m.Policy.MinTokens))
			continue
		}
		eligible = append(eligible, b)
	}

	limit := m.Policy.MaxBreakpoints
	if limit <= 0 || len(eligible) <= limit {
		return eligible
	}

	// Over the limit. The deepest boundary is kept unconditionally, because it
	// covers the whole prefix and is what the next turn reads back; with only
	// one marker to spend, coverage beats stability. Beyond that the shallowest
	// is kept, since the frozen zone is the one prefix certain to be identical
	// next turn. What gets dropped is the middle, which is both less stable
	// than the first and covers less than the last.
	kept := make([]prefix.Boundary, 0, limit)
	deepestFrom := len(eligible) - limit
	if limit > 1 {
		kept = append(kept, eligible[0])
		deepestFrom = len(eligible) - (limit - 1)
	}
	for i := deepestFrom; i < len(eligible); i++ {
		kept = append(kept, eligible[i])
	}

	for _, dropped := range eligible[boundedStart(limit):deepestFrom] {
		decision.Declined = append(decision.Declined, fmt.Sprintf(
			"the %s boundary was dropped: this target allows %d markers, and the ends of the prefix are worth more",
			dropped.Zone, limit))
	}
	return kept
}

func boundedStart(limit int) int {
	if limit > 1 {
		return 1
	}
	return 0
}

// CrossesLookback reports whether the deepest marker sits far enough back that
// the target would stop finding it.
//
// §6.2 asks for history candidates before a growing turn crosses the lookback
// window. This placement makes that condition unreachable rather than handling
// it: the deepest eligible boundary is always kept, and the deepest boundary is
// always the end of history, so a marker is never more than the tail away from
// where the search starts.
//
// That is worth stating as a property rather than trusting, because it stops
// holding the moment placement changes. The test that pins it is the guard.
func (m *Manager) CrossesLookback(layout *prefix.Layout, decision Decision) bool {
	if m.Policy.LookbackBlocks <= 0 || decision.Plan == nil {
		return false
	}
	deepest := -1
	for _, bp := range decision.Plan.Breakpoints {
		if bp.Position.MessageIndex > deepest {
			deepest = bp.Position.MessageIndex
		}
	}
	if deepest < 0 {
		return layout.HistoryBlocks() > m.Policy.LookbackBlocks-m.margin()
	}

	// Blocks between the deepest marker and the end of the request.
	req := layout.Request()
	behind := 0
	for i := deepest + 1; i < len(req.Messages); i++ {
		behind += len(req.Messages[i].Content)
	}
	return behind > m.Policy.LookbackBlocks-m.margin()
}

func (m *Manager) mode() catalog.CacheMode {
	if m.Policy.DefaultMode != "" {
		return m.Policy.DefaultMode
	}
	if len(m.Policy.Modes) > 0 {
		return m.Policy.Modes[0]
	}
	return catalog.CacheNone
}

// ttl picks the retention to request. The shortest offered is the default
// because a longer one bills more to write and only pays back on reuse nobody
// has measured yet; §6.4 is where that trade becomes a calculation.
func (m *Manager) ttl() time.Duration {
	shortest := time.Duration(0)
	for _, raw := range m.Policy.TTLs {
		d, err := time.ParseDuration(raw)
		if err != nil {
			continue
		}
		if shortest == 0 || d < shortest {
			shortest = d
		}
	}
	return shortest
}

// routingKey derives an affinity key for targets that accept one.
//
// It is built from the prefix and the target, per §6.2: not from the tier,
// because a fallback and two tiers bound to the same model would otherwise
// fragment or misattribute state. It is a hash rather than the content, so it
// identifies a prefix to the server without describing it.
func (m *Manager) routingKey(layout *prefix.Layout) string {
	if !m.Policy.RoutingKeySupport {
		return ""
	}
	sum := sha256.Sum256([]byte(string(m.Target) + "\x00" + layout.PrefixHash()))
	return hex.EncodeToString(sum[:16])
}
