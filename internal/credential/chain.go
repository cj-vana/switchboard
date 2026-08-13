package credential

// Settings is the per-provider auth configuration a user may write.
//
// There is no field for a credential value, and there will not be one. §5.3
// puts credentials out of the ordinary config file entirely, and a key that
// exists is a key someone pastes a secret into.
type Settings struct {
	// Env names an additional variable to consult ahead of the defaults, for a
	// machine whose convention this program does not know.
	Env string

	// Helper is argv for a command whose standard output is the credential.
	Helper []string

	// OAuth configures an authorization-code flow. It is empty unless the user
	// registered a client and wrote it down, because this program ships none.
	OAuth OAuthSettings
}

// Chain builds the resolver for one reference's provider.
//
// The order is environment, then helper, then OAuth, then the platform store.
// It runs from most explicit to most ambient: a variable is set for one
// process, a helper is configured for this program, an OAuth client is
// something the user registered and logged in to, and the credential service is
// whatever the machine happens to hold. Reversing that would make the narrower
// statement lose to the broader one, which is the wrong way for an override to
// behave.
func Chain(s Settings) *Resolver {
	env := &EnvStore{}
	if s.Env != "" {
		named := s.Env
		env.Names = func(ref Ref) []string {
			return append([]string{named}, EnvNames(ref)...)
		}
	}

	sources := []Store{env}
	if len(s.Helper) > 0 {
		sources = append(sources, &HelperStore{Command: s.Helper})
	}
	if s.OAuth.configured() {
		// Ahead of the plain key store, because a provider with an OAuth client
		// configured is one the user has deliberately set up to log in to, and
		// a stale API key left behind from before should not quietly win.
		sources = append(sources, &OAuthStore{Settings: s.OAuth})
	}
	return NewResolver(append(sources, NewOSStore())...)
}
