package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/cjvana/switchboard/internal/config"
	"github.com/cjvana/switchboard/internal/credential"
	"github.com/cjvana/switchboard/internal/provider"
	"github.com/cjvana/switchboard/internal/provider/anthropic"
	"github.com/cjvana/switchboard/internal/provider/ollama"
	"github.com/cjvana/switchboard/internal/provider/openai"
	"github.com/cjvana/switchboard/internal/provider/openaicompat"
)

// providers binds a route target to the adapter that can serve it.
//
// Every target the loop can run passes through here, so a tier naming a
// provider this build has no adapter for fails at startup with a list of what
// it does have, rather than partway through the first turn.
type providers struct {
	ollama *ollama.Client

	// compat is keyed by serving surface, which for this adapter is also the
	// profile name: two profiles are two different servers with different
	// capabilities, so they cannot share a client.
	compat map[string]*openaicompat.Client

	// openai is its own provider rather than a compat profile, so it gets its
	// own client, its own credential, and its own catalog entry.
	openai *openaicompat.Client

	anthropic *anthropic.Client

	config *config.Config
}

func newProviders(host string, cfg *config.Config) *providers {
	return &providers{
		ollama: ollama.New(ollama.WithBaseURL(host)),
		compat: map[string]*openaicompat.Client{},
		config: cfg,
	}
}

// baseURL is the configured address for a provider, or empty for its default.
func (p *providers) baseURL(name string) string {
	return p.config.ProviderFor(name).BaseURL
}

func (p *providers) get(target provider.RouteTarget) (provider.Provider, error) {
	switch target.Provider {
	case ollama.Name:
		return p.ollama, nil

	case anthropic.Name:
		if p.anthropic != nil {
			return p.anthropic, nil
		}
		key, err := p.credential(target)
		if err != nil {
			return nil, err
		}
		p.anthropic = anthropic.New(
			anthropic.WithAPIKey(key),
			anthropic.WithBaseURL(p.baseURL(anthropic.Name)),
		)
		return p.anthropic, nil

	case openai.Name:
		if p.openai != nil {
			return p.openai, nil
		}
		opts, err := p.authOptions(target)
		if err != nil {
			return nil, err
		}
		if base := p.baseURL(openai.Name); base != "" {
			opts = append(opts, openaicompat.WithBaseURL(base))
		}
		p.openai = openai.New(opts...)
		return p.openai, nil

	case openaicompat.Name:
		if c, ok := p.compat[target.Surface]; ok {
			return c, nil
		}
		opts, err := p.authOptions(target)
		if err != nil {
			return nil, err
		}
		switch {
		case p.baseURL(openaicompat.Name) != "":
			opts = append(opts, openaicompat.WithBaseURL(p.baseURL(openaicompat.Name)))
		case target.Surface == "ollama":
			// The same server, reached through its compatibility endpoint. The
			// host was already resolved from the flag and the environment for
			// the native adapter; resolving it twice invites the two to
			// disagree about which server the user meant.
			opts = append(opts, openaicompat.WithBaseURL(p.ollama.BaseURL()+"/v1"))
		}
		c, newErr := openaicompat.New(target.Surface, opts...)
		if newErr != nil {
			return nil, fmt.Errorf(
				"target %s names serving surface %q, which is not a profile this build has tested: %w",
				target.ID(), target.Surface, newErr)
		}
		p.compat[target.Surface] = c
		return c, nil
	}

	return nil, fmt.Errorf(
		"target %s names provider %q; this build has adapters for %s, %s, %s, and %s",
		target.ID(), target.Provider, anthropic.Name, ollama.Name, openai.Name, openaicompat.Name)
}

// authOptions resolves the credential for a target, if there is one to find.
//
// A missing credential is not an error here. Every profile this build ships
// points at a local server that wants no authorization, and refusing to start
// without a key nobody needs would be worse than useless.
//
// Nor does an absent credential get mentioned when a probe fails: on a local
// server there is correctly nothing to find, so pointing at authentication
// would send the user to `sb auth login` when the real answer is `ollama pull`.
// Turning a rejection into "you have no credential" needs a server that can
// actually issue one, and this build has no adapter that reaches such a server.
// That message gets written against a real 401 rather than a guess at one.
func (p *providers) credential(target provider.RouteTarget) (string, error) {
	ref := credential.Ref{Provider: target.Provider, Account: target.Surface}
	resolver := credential.Chain(p.config.AuthFor(target.Provider))

	secret, err := resolver.Get(context.Background(), ref)
	if err != nil {
		if errors.Is(err, credential.ErrNotFound) {
			return "", nil
		}
		// A configured helper that is present and broken is the user's problem
		// to fix, and starting without the key it would have supplied only
		// moves the failure somewhere less legible.
		return "", err
	}
	// Exposed at the point of use and handed straight to the adapter, which is
	// the only place a credential is meant to be a plain string.
	return secret.Expose(), nil
}

// authOptions adapts credential resolution for the two adapters built on the
// OpenAI-compatible client.
func (p *providers) authOptions(target provider.RouteTarget) ([]openaicompat.Option, error) {
	key, err := p.credential(target)
	if err != nil || key == "" {
		return nil, err
	}
	return []openaicompat.Option{openaicompat.WithAPIKey(key)}, nil
}

// servedByOllama reports whether a target reaches an Ollama server, whether
// through the native API or the compatibility endpoint. It decides only whether
// "ollama pull" is useful advice.
func servedByOllama(target provider.RouteTarget) bool {
	return target.Provider == ollama.Name ||
		(target.Provider == openaicompat.Name && target.Surface == "ollama")
}

// probeTier confirms the target can actually drive the loop before a turn
// starts, so a missing model is an error now rather than halfway through.
func (p *providers) probeTier(ctx context.Context, tier config.Tier) (config.Tier, provider.Provider, error) {
	client, err := p.get(tier.Target)
	if err != nil {
		return config.Tier{}, nil, fmt.Errorf("tier %s: %w", tier.ID, err)
	}

	probe, err := client.Probe(ctx, tier.Target)
	if err != nil {
		return config.Tier{}, nil, err
	}
	switch {
	case !probe.Reachable:
		return config.Tier{}, nil, fmt.Errorf("no server responded for %s: %s", tier.Target.ID(), probe.Detail)
	case !probe.ModelPresent:
		if servedByOllama(tier.Target) {
			return config.Tier{}, nil, fmt.Errorf("%s\nrun: ollama pull %s", probe.Detail, tier.Target.ModelID)
		}
		return config.Tier{}, nil, fmt.Errorf("%s", probe.Detail)
	case probe.Tools == provider.ToolsNone:
		return config.Tier{}, nil, fmt.Errorf(
			"%s does not support tool calling, so it cannot drive the agent loop", tier.Target.ModelID)
	}
	return tier, client, nil
}
