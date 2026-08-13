package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`

	// Strict requests provider-enforced schema conformance. An adapter whose
	// target cannot enforce it degrades to a non-strict schema and says so
	// through Probe rather than pretending the guarantee holds.
	Strict bool `json:"strict,omitempty"`
}

type Request struct {
	System   []Block
	Tools    []ToolDefinition
	Messages []Message

	// CachePlan anchors cache breakpoints to canonical positions. Only the
	// breakpoint manager constructs one, which keeps canonical message content
	// stable for hashing (§5.1). The manager is phase 2a, so this is nil today
	// and adapters must treat a non-nil plan they cannot render as an error.
	CachePlan *CachePlan
}

// CachePlan is request-level rather than metadata on blocks, because providers
// place cache markers in different places: on content blocks, on tool
// definitions, or on the request itself.
type CachePlan struct {
	Breakpoints []Breakpoint
}

type Breakpoint struct {
	Position CachePosition
	TTL      time.Duration
}

// CachePosition addresses a canonical location. MessageIndex of -1 addresses
// the system block list; -2 addresses the tool definitions.
type CachePosition struct {
	MessageIndex int
	BlockIndex   int
}

type Provider interface {
	Name() string
	Stream(ctx context.Context, target RouteTarget, req Request) (EventStream, error)
	CountTokens(ctx context.Context, target RouteTarget, req Request) (TokenEstimate, error)
	Probe(ctx context.Context, target RouteTarget) (ProbeResult, error)
}

// CapabilityError reports that a target cannot honor something the request
// asked for. Adapters return it instead of degrading silently; whether to
// emulate the capability is a decision for the visible policy layer, which can
// recheck destination and quality first (§5.2).
type CapabilityError struct {
	Target     RouteTargetID
	Capability string
	Detail     string
}

func (e *CapabilityError) Error() string {
	return fmt.Sprintf("target %s does not support %s: %s", e.Target, e.Capability, e.Detail)
}

// ProtocolError reports content that does not fit the adapter's expected shape.
// It aborts the turn and preserves the session log; it is not returned to the
// model as a tool error, because the model did not cause it and cannot fix it
// (§10.3).
type ProtocolError struct {
	Provider string
	Detail   string
	Err      error
}

func (e *ProtocolError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: malformed response: %s: %v", e.Provider, e.Detail, e.Err)
	}
	return fmt.Sprintf("%s: malformed response: %s", e.Provider, e.Detail)
}

func (e *ProtocolError) Unwrap() error { return e.Err }

// APIError reports a non-success response. Retryable drives the loop's bounded
// backoff; anything else fails the turn immediately rather than burning the
// attempt budget on a request that cannot succeed.
type APIError struct {
	Provider   string
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s: http %d: %s", e.Provider, e.StatusCode, e.Body)
}

func (e *APIError) Retryable() bool {
	return e.StatusCode == 408 || e.StatusCode == 409 || e.StatusCode == 429 || e.StatusCode >= 500
}
