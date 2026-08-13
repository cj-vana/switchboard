package provider

import "fmt"

// RouteTargetID identifies a target for cache scoping, cost attribution, and
// session records. Two tiers bound to the same model with different inference
// parameters are different targets, so the ID must include those parameters
// wherever they change capability, price, context rendering, or cache identity
// (§3.1).
type RouteTargetID string

// RouteTarget is the unit of execution: provider, serving surface, model
// snapshot, and inference configuration. Cache state, price, and capability
// attach here rather than to a bare model name.
type RouteTarget struct {
	Provider string
	Surface  string
	ModelID  string
	Params   Params
}

// Params holds inference configuration. A nil pointer means "provider default",
// which is distinct from an explicit zero.
type Params struct {
	MaxOutputTokens int
	Temperature     *float64
	Reasoning       *Reasoning
}

// Reasoning requests thinking output. Effort is an unvalidated provider-level
// string here; adapters map it against what the target actually supports and
// return a CapabilityError when it does not, rather than quietly ignoring it.
type Reasoning struct {
	Enabled bool
	Effort  string
}

func (t RouteTarget) ID() RouteTargetID {
	id := fmt.Sprintf("%s/%s/%s", t.Provider, t.Surface, t.ModelID)
	if r := t.Params.Reasoning; r != nil && r.Enabled {
		if r.Effort != "" {
			id += "+think:" + r.Effort
		} else {
			id += "+think"
		}
	}
	return RouteTargetID(id)
}

func (t RouteTarget) String() string { return string(t.ID()) }

// ToolSupport records how reliably a target handles tool calls. Serial versus
// parallel is a wire-format question; Unreliable is a measured judgment a probe
// can only hint at (§4).
type ToolSupport string

const (
	ToolsNone       ToolSupport = "none"
	ToolsSerial     ToolSupport = "serial"
	ToolsParallel   ToolSupport = "parallel"
	ToolsUnreliable ToolSupport = "unreliable"
)

// ProbeResult establishes API compatibility. It does not establish tool-calling
// quality, which needs evaluation rather than a single successful call (§4).
type ProbeResult struct {
	Reachable    bool
	ModelPresent bool
	Tools        ToolSupport
	Vision       bool
	Detail       string
}

// TokenEstimate reports a count. Exact is false when the number came from a
// local approximation rather than the provider, so callers can widen budget
// margins accordingly.
type TokenEstimate struct {
	InputTokens int
	Exact       bool
}
