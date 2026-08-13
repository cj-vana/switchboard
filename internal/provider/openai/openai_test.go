package openai

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cjvana/switchboard/internal/provider"
	"github.com/cjvana/switchboard/internal/provider/openaicompat"
)

// OpenAI is its own provider, not a profile of the compatible adapter. Sharing
// the decoder is an implementation detail; sharing the identity would file
// OpenAI's price sheet and OpenAI's credential under a name that means "some
// server speaking this format".
func TestIdentityIsNotTheCompatibleAdapter(t *testing.T) {
	target := Target("gpt-5-mini")

	if target.Provider != "openai" || target.Surface != "first-party" {
		t.Errorf("target = %+v", target)
	}
	if strings.Contains(string(target.ID()), openaicompat.Name) {
		t.Errorf("target id %s files OpenAI under the compatible adapter", target.ID())
	}

	// The credential is looked up by provider and surface, so the identity
	// above is what decides which key pays for the request.
	if got := New().Name(); got != Name {
		t.Errorf("client reports provider %q, so its errors and its credential would be attributed to the wrong vendor", got)
	}
}

// Nothing in this package has been run against the live API. Until it has,
// asking for reasoning has to be a capability error the caller sees rather than
// a parameter quietly dropped (§5.2), because a silently ignored reasoning
// request produces a cheaper, worse answer that looks like a correct one.
func TestUntestedCapabilitiesAreRefusedNotDropped(t *testing.T) {
	target := Target("gpt-5-mini")
	target.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: "high"}

	_, err := New().Stream(context.Background(), target, provider.Request{
		Messages: []provider.Message{provider.UserText("hello")},
	})

	var capErr *provider.CapabilityError
	if !errors.As(err, &capErr) {
		t.Fatalf("err = %v, want a CapabilityError while the profile is unverified", err)
	}
}

// A cache plan cannot be rendered into this format at all, so a non-nil plan is
// an error rather than a request sent without the markers the caller asked for.
func TestCachePlanIsRefused(t *testing.T) {
	_, err := New().Stream(context.Background(), Target("gpt-5-mini"), provider.Request{
		Messages:  []provider.Message{provider.UserText("hello")},
		CachePlan: &provider.CachePlan{Breakpoints: []provider.Breakpoint{{}}},
	})

	var capErr *provider.CapabilityError
	if !errors.As(err, &capErr) {
		t.Fatalf("err = %v, want a CapabilityError", err)
	}
}
