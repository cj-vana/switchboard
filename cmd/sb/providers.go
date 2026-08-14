package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/cj-vana/switchboard/internal/config"
	"github.com/cj-vana/switchboard/internal/credential"
	"github.com/cj-vana/switchboard/internal/provider"
	"github.com/cj-vana/switchboard/internal/provider/anthropic"
	"github.com/cj-vana/switchboard/internal/provider/kimi"
	"github.com/cj-vana/switchboard/internal/provider/ollama"
	"github.com/cj-vana/switchboard/internal/provider/openai"
	"github.com/cj-vana/switchboard/internal/provider/openaicompat"
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

	// openai is keyed by surface: the developer API and the subscription
	// backend are different endpoints with different credentials.
	openai map[string]*openaicompat.Client

	anthropic *anthropic.Client
	kimi      *anthropic.Client

	// responses serves the subscription surface, which speaks a third wire
	// format and cannot share the compatible client.
	responses *openai.ResponsesClient

	config *config.Config
}

func newProviders(host string, cfg *config.Config) *providers {
	return &providers{
		ollama: ollama.New(ollama.WithBaseURL(host)),
		compat: map[string]*openaicompat.Client{},
		openai: map[string]*openaicompat.Client{},
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

	case kimi.Name:
		if p.kimi != nil {
			return p.kimi, nil
		}
		key, err := p.credential(target)
		if err != nil {
			return nil, err
		}
		p.kimi = kimi.New(key, anthropic.WithBaseURL(p.baseURL(kimi.Name)))
		return p.kimi, nil

	case openai.Name:
		if target.Surface == openai.Subscription {
			// A different wire format, so a different client. The compatible
			// one cannot serve this endpoint at all.
			if p.responses != nil {
				return p.responses, nil
			}
			token, err := p.credential(target)
			if err != nil {
				return nil, err
			}
			p.responses = openai.NewResponses(
				openai.WithResponsesToken(token),
				openai.WithResponsesBaseURL(p.baseURL(openai.Name)),
			)
			return p.responses, nil
		}
		if c, ok := p.openai[target.Surface]; ok {
			return c, nil
		}
		opts, err := p.authOptions(target)
		if err != nil {
			return nil, err
		}
		if base := p.baseURL(openai.Name); base != "" {
			opts = append(opts, openaicompat.WithBaseURL(base))
		}
		c := openai.New(target.Surface, opts...)
		p.openai[target.Surface] = c
		return c, nil

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
		"target %s names provider %q; this build has adapters for %s, %s, %s, %s, and %s",
		target.ID(), target.Provider,
		anthropic.Name, kimi.Name, ollama.Name, openai.Name, openaicompat.Name)
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
	resolver := credential.Chain(authSettings(p.config, target))

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

// authSettings resolves the auth configuration for a target, filling in a
// bundled OAuth client where one exists and the user has not named their own.
//
// Configuration always wins. A user who registers their own client and writes
// it down uses theirs, and the bundled one is only what makes a surface work
// without any configuration at all.
func authSettings(cfg *config.Config, target provider.RouteTarget) credential.Settings {
	settings := cfg.AuthFor(target.Provider)
	if settings.OAuth.ClientID != "" {
		return settings
	}
	if target.Provider == openai.Name {
		if bundled := openai.DefaultOAuth(target.Surface); bundled.ClientID != "" {
			settings.OAuth = bundled
		}
	}
	return settings
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
