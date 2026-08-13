package credential

import (
	"context"
	"os"
	"strings"
)

// EnvStore reads a credential from the environment.
//
// This is the headless path §5.3 allows. It is not a fallback for an
// interactive machine that has a credential service: it is first in the chain
// precisely so that a deliberately exported variable overrides what is stored.
type EnvStore struct {
	// Names is consulted per reference. A nil value uses EnvNames.
	Names func(Ref) []string

	// lookup exists so tests can supply an environment without mutating the
	// process, which would make them order-dependent.
	lookup func(string) (string, bool)
}

func (s *EnvStore) Name() string { return "environment" }

func (s *EnvStore) Get(_ context.Context, ref Ref) (Secret, error) {
	names := EnvNames
	if s.Names != nil {
		names = s.Names
	}
	get := s.lookup
	if get == nil {
		get = os.LookupEnv
	}

	for _, name := range names(ref) {
		if v, ok := get(name); ok && strings.TrimSpace(v) != "" {
			return New(v, SourceEnv, name), nil
		}
	}
	return Secret{}, ErrNotFound
}

// EnvNames lists the variables consulted for a reference, most specific first.
//
// The namespaced form always comes first so a user who has several tools on one
// machine can give this one its own key without disturbing the others.
//
// The conventional name is only offered where the provider *is* that vendor.
// This is the part worth reading twice: an OpenAI-compatible endpoint is not
// OpenAI. Honoring OPENAI_API_KEY for a target on the openaicompat provider
// would take a key issued to one company and put it in an Authorization header
// bound for whatever server the profile points at. The convenience is not worth
// the class of accident.
func EnvNames(ref Ref) []string {
	names := []string{envVar("SB", ref.Provider, ref.Account, "API_KEY")}
	if ref.Account != "" {
		names = append(names, envVar("SB", ref.Provider, "", "API_KEY"))
	}
	if conventional, ok := conventionalEnv[ref.Provider]; ok {
		names = append(names, conventional)
	}
	return names
}

// conventionalEnv maps a provider to the variable its own vendor documents.
// Adding an entry here is a statement that traffic for this provider goes to
// that vendor and nowhere else, so a gateway or compatibility provider must not
// appear in it.
var conventionalEnv = map[string]string{
	"anthropic": "ANTHROPIC_API_KEY",
	"openai":    "OPENAI_API_KEY",
}

func envVar(parts ...string) string {
	var kept []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		// Provider and surface names carry punctuation that is not legal in a
		// variable name.
		p = strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
				return r
			default:
				return '_'
			}
		}, p)
		kept = append(kept, strings.ToUpper(p))
	}
	return strings.Join(kept, "_")
}
