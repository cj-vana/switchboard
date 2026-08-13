// Package openai binds OpenAI's own API as a route target.
//
// The wire format is the one `openaicompat` already speaks, so this package is
// deliberately thin: it supplies the endpoint, the profile, and the identity,
// and reuses the decoder that was written against a recorded capture.
//
// It is a provider in its own right rather than a profile of the compatible
// adapter, because target identity is provider plus surface plus model (§4).
// "Reached through a format OpenAI also invented" is a fact about this
// codebase's implementation, not about where the request goes, what it costs,
// or whose key pays for it. Filing it under `openaicompat` would put OpenAI's
// price sheet and OpenAI's credential under a name that means "some server
// speaking this format", which is exactly the confusion the surface field
// exists to prevent.
package openai

import (
	"github.com/cjvana/switchboard/internal/provider"
	"github.com/cjvana/switchboard/internal/provider/openaicompat"
)

const (
	Name = "openai"

	// Surface is first-party because that is what this endpoint is. A gateway
	// or an Azure deployment reaching the same models is a different surface
	// with different pricing and retention, and would be a different target.
	Surface = "first-party"
)

// profile describes the endpoint.
//
// Every field here is an unverified claim until someone runs it. Unlike the
// Ollama profile, which was written from a recorded capture, nothing in this
// package has been exercised against the live API: there has been no key to do
// it with. Reasoning is left false and EffortLevels empty for that reason,
// so asking for reasoning is a capability error the caller sees rather than a
// parameter that is silently dropped. Both get filled in from a capture, not
// from documentation.
var profile = openaicompat.Profile{
	Provider:    Name,
	BaseURL:     "https://api.openai.com/v1",
	Tools:       true,
	StreamUsage: true,
}

// New builds a client. The credential is supplied by the caller rather than
// read from the environment here, so credential resolution stays in one place
// (§5.3).
func New(opts ...openaicompat.Option) *openaicompat.Client {
	return openaicompat.NewFor(Surface, profile, opts...)
}

func Target(model string) provider.RouteTarget {
	return provider.RouteTarget{Provider: Name, Surface: Surface, ModelID: model}
}
