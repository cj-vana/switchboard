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
}

// Chain builds the resolver for one reference's provider.
//
// The order is environment, then helper, then the platform store. It runs from
// most explicit to most ambient: a variable is set for one process, a helper is
// configured for this program, and the credential service is whatever the
// machine happens to hold. Reversing that would make the narrower statement
// lose to the broader one, which is the wrong way for an override to behave.
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
	return NewResolver(append(sources, NewOSStore())...)
}
