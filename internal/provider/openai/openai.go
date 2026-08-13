// Package openai binds OpenAI's own API as a route target.
//
// The wire format is the one `openaicompat` already speaks, so this package is
// deliberately thin: it supplies the endpoints, the profiles, and the identity,
// and reuses the decoder that was written against a recorded capture.
//
// It is a provider in its own right rather than a profile of the compatible
// adapter, because target identity is provider plus surface plus model (§4).
// "Reached through a format OpenAI also invented" is a fact about this
// codebase's implementation, not about where the request goes, what it costs,
// or whose key pays for it.
package openai

import (
	"github.com/cjvana/switchboard/internal/credential"
	"github.com/cjvana/switchboard/internal/provider"
	"github.com/cjvana/switchboard/internal/provider/openaicompat"
)

const Name = "openai"

// The two serving surfaces differ in every way the surface field exists to
// capture: a different endpoint, a different credential, and a different
// billing model. They are not interchangeable and are not the same target.
const (
	// FirstParty is the developer API, paid per token against an API key.
	FirstParty = "first-party"

	// Subscription is the backend behind a ChatGPT plan, reached with an OAuth
	// token and billed as a flat subscription rather than per token.
	Subscription = "subscription"
)

const (
	FirstPartyBaseURL   = "https://api.openai.com/v1"
	SubscriptionBaseURL = "https://chatgpt.com/backend-api/codex"
)

// SubscriptionOAuth is the client this build presents when signing in to a
// ChatGPT plan.
//
// The client ID is the one OpenAI's own Codex CLI registers. Switchboard is not
// affiliated with or endorsed by OpenAI, and this is not a flow OpenAI has
// published for third-party clients; it works because the authorization server
// accepts the registration, not because anyone was given permission to use it.
// The consequences land on the account that signs in, and OpenAI's Terms of Use
// govern that account regardless of what this program presents itself as.
//
// It is overridable. Anything set under [auth.openai.oauth] wins, so a user who
// registers their own client uses theirs.
var SubscriptionOAuth = credential.OAuthSettings{
	ClientID:     "app_EMoamEEZ73f0CkXaXp7hrann",
	AuthorizeURL: "https://auth.openai.com/oauth/authorize",
	TokenURL:     "https://auth.openai.com/oauth/token",
	Scopes:       []string{"openid", "profile", "email", "offline_access"},
}

// profiles record what each endpoint actually does.
//
// Neither has been run against the live service, so both claim the floor:
// tools, because that is what the adapter is for, and nothing else. Reasoning
// is left unsupported so that asking for it is a capability error the caller
// sees rather than a parameter silently dropped, which would return a cheaper,
// worse answer looking like a correct one. Both get filled in from a capture.
var profiles = map[string]openaicompat.Profile{
	FirstParty: {
		Provider:    Name,
		BaseURL:     FirstPartyBaseURL,
		Tools:       true,
		StreamUsage: true,
	},
	Subscription: {
		Provider:    Name,
		BaseURL:     SubscriptionBaseURL,
		Tools:       true,
		StreamUsage: true,
	},
}

// New builds a client for a serving surface. An unknown surface falls back to
// the developer API, which is the one that behaves like the documented format.
func New(surface string, opts ...openaicompat.Option) *openaicompat.Client {
	profile, ok := profiles[surface]
	if !ok {
		profile = profiles[FirstParty]
		surface = FirstParty
	}
	return openaicompat.NewFor(surface, profile, opts...)
}

// DefaultBaseURL reports where a surface is reached before any configured
// override, so a caller can tell whether one is in effect without knowing the
// address.
func DefaultBaseURL(surface string) string {
	if p, ok := profiles[surface]; ok {
		return p.BaseURL
	}
	return FirstPartyBaseURL
}

// DefaultOAuth returns the bundled client for a surface, which exists only for
// the subscription one. The developer API takes an API key and has no login
// flow to offer.
func DefaultOAuth(surface string) credential.OAuthSettings {
	if surface == Subscription {
		return SubscriptionOAuth
	}
	return credential.OAuthSettings{}
}

func Target(model string) provider.RouteTarget {
	return provider.RouteTarget{Provider: Name, Surface: FirstParty, ModelID: model}
}

func SubscriptionTarget(model string) provider.RouteTarget {
	return provider.RouteTarget{Provider: Name, Surface: Subscription, ModelID: model}
}
