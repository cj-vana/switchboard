package provider

import (
	"context"
	"encoding/json"
	"errors"
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

	// RoutingKey gives automatic provider caches stable affinity without
	// pretending they accept explicit marker positions. Adapters must render it
	// or return a CapabilityError; silently dropping it changes cache economics.
	RoutingKey string
}

type Breakpoint struct {
	Position CachePosition
	TTL      time.Duration
}

// CachePosition addresses a canonical location.
type CachePosition struct {
	MessageIndex int
	BlockIndex   int
}

// SystemBlocks and ToolDefinitions are the two positions that are not messages.
// They are named because an adapter reading a bare -2 has to guess, and a
// breakpoint placed one position off caches a different prefix than the one
// whose reuse was scored.
const (
	SystemBlocks    = -1
	ToolDefinitions = -2
)

type Provider interface {
	Name() string
	Stream(ctx context.Context, target RouteTarget, req Request) (EventStream, error)
	CountTokens(ctx context.Context, target RouteTarget, req Request) (TokenEstimate, error)
	Probe(ctx context.Context, target RouteTarget) (ProbeResult, error)
}

// ErrStreamIncomplete reports that a stream ended without the provider's
// terminal event. The turn produced real output, so the caller keeps it as an
// incomplete message and decides whether to resume or re-issue (§10.3).
var ErrStreamIncomplete = errors.New("stream ended before the provider signaled completion")

// CapabilityError reports that a target cannot honor something the request
// asked for. Adapters return it instead of degrading silently; whether to
// emulate the capability is a decision for the visible policy layer, which can
// recheck destination and quality first (§5.2).
type CapabilityError struct {
	Target     RouteTargetID
	Capability string
	Detail     string
}

// RequestIssued reports whether an error can have happened after a provider
// request left the process. Unknown errors are conservatively treated as
// issued. Adapters mark local request construction failures so budget ledgers
// can release a reservation without inventing retry debt.
func RequestIssued(err error) bool {
	if err == nil {
		return false
	}
	var marked interface{ RequestIssued() bool }
	if errors.As(err, &marked) {
		return marked.RequestIssued()
	}
	return true
}

// MarkUnissued wraps a failure known to have occurred before transport.
func MarkUnissued(err error) error {
	if err == nil || !RequestIssued(err) {
		return err
	}
	return &unissuedError{err: err}
}

type unissuedError struct{ err error }

func (e *unissuedError) Error() string       { return e.err.Error() }
func (e *unissuedError) Unwrap() error       { return e.err }
func (e *unissuedError) RequestIssued() bool { return false }

// CapabilityError is produced while translating a canonical request, before
// any built-in adapter performs network I/O.
func (*CapabilityError) RequestIssued() bool { return false }

func (e *CapabilityError) Error() string {
	return fmt.Sprintf("target %s does not support %s: %s", DisplayRouteTargetID(e.Target), e.Capability, e.Detail)
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

// APIError reports a provider-reported failure. Retryable drives the loop's
// bounded backoff; anything else fails the turn immediately rather than burning
// the attempt budget on a request that cannot succeed.
//
// StatusCode is 0 when the provider reported the error inside an already
// successful response, as happens with an error object mid-stream. Such errors
// are not retried, because nothing in the status distinguishes a transient
// failure from a permanent one.
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
