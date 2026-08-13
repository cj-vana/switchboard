// Package kimi binds Moonshot's Kimi Code endpoint as a route target.
//
// It serves the Messages API, so the adapter is the one already written against
// a capture of Anthropic's: thinking blocks arrive with signatures, tool calls
// stream as partial JSON, and usage reports cache reads and writes as counts
// disjoint from the input total. All of that was confirmed against this
// endpoint rather than assumed from the format's name.
//
// Kimi is its own provider all the same. Serving a format someone else defined
// says nothing about who is paid, which key authenticates, what a token costs,
// or which models exist, and target identity is built from those.
package kimi

import (
	"github.com/cjvana/switchboard/internal/provider"
	"github.com/cjvana/switchboard/internal/provider/anthropic"
)

const (
	Name = "kimi"

	// Surface is the coding plan's endpoint. Moonshot also serves a
	// general-purpose OpenAI-compatible API at another host, which is a
	// different surface with different models and would be a different target.
	Surface = "coding"

	// BaseURL has no /v1: the adapter appends the version, so this is the
	// prefix the coding plan is served under.
	BaseURL = "https://api.kimi.com/coding"
)

// New builds a client. The credential is supplied by the caller rather than
// read from the environment here, so credential resolution stays in one place
// (§5.3).
func New(apiKey string, opts ...anthropic.Option) *anthropic.Client {
	return anthropic.New(append([]anthropic.Option{
		anthropic.WithProvider(Name),
		anthropic.WithBaseURL(BaseURL),
		anthropic.WithAPIKey(apiKey),
	}, opts...)...)
}

func Target(model string) provider.RouteTarget {
	return provider.RouteTarget{Provider: Name, Surface: Surface, ModelID: model}
}
