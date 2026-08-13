// Package credential resolves provider secrets without ever writing them where
// they can be read back in the clear.
//
// The rules come from §5.3 and they are all negative, which is the point:
// credentials never live in the ordinary config file or the session log; a file
// fallback is not offered, because mode 0600 is access control and not
// encryption; and nothing here copies a token out of another application or
// stands in for a login flow a provider has not published for third-party
// clients.
//
// What is left is three places a secret may come from: the environment, a
// command the user explicitly configured, and the operating system's credential
// service. Which one answered is recorded. The secret itself is not.
package credential

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrNotFound means a source was consulted and held nothing. It is not a
// failure: the resolver moves to the next source. A source that is broken
// rather than empty returns a different error and stops the chain, because
// falling through a misconfigured helper would present "your helper is wrong"
// as "you have no credential".
var ErrNotFound = errors.New("no credential")

// Ref names one credential.
//
// Account is the serving surface rather than a username, because that is the
// axis along which a second credential for the same provider actually appears:
// one key for the first-party API and another for a gateway is normal, and a
// single provider-keyed slot would silently overwrite one with the other.
type Ref struct {
	Provider string
	Account  string
}

func (r Ref) String() string {
	if r.Account == "" {
		return r.Provider
	}
	return r.Provider + "/" + r.Account
}

func (r Ref) valid() error {
	if strings.TrimSpace(r.Provider) == "" {
		return errors.New("credential reference names no provider")
	}
	return nil
}

// Source names where a secret came from. It is safe to log; it is the only part
// of a resolution that is.
type Source string

const (
	SourceEnv      Source = "environment"
	SourceHelper   Source = "helper"
	SourceKeychain Source = "os credential service"
	SourceOAuth    Source = "oauth"
)

// Secret carries a credential value.
//
// The value is unexported and every rendering method redacts, so a Secret that
// reaches a log line, a formatted error, or the JSON session record prints as a
// placeholder rather than as itself. Reading the value takes an explicit call
// that is easy to grep for.
type Secret struct {
	value string

	// Source and Detail describe the resolution, not the secret, so `sb auth
	// status` can answer "will this work, and from where" without revealing
	// anything.
	Source Source
	Detail string
}

func New(value string, source Source, detail string) Secret {
	return Secret{value: strings.TrimSpace(value), Source: source, Detail: detail}
}

// Expose returns the credential. Call it at the point of use, never earlier.
func (s Secret) Expose() string { return s.value }

func (s Secret) Empty() bool { return s.value == "" }

const redacted = "<credential redacted>"

func (s Secret) String() string   { return redacted }
func (s Secret) GoString() string { return redacted }

// MarshalJSON exists because the session log is JSON and is written by code
// that has no reason to know a Secret passed through it.
func (s Secret) MarshalJSON() ([]byte, error) { return json.Marshal(redacted) }

func (s Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// Store is one place credentials may be read from, and in some cases written
// to. Sources that cannot accept a write (the environment, a helper command)
// report that rather than pretending to store.
type Store interface {
	// Name is what `sb auth status` shows.
	Name() string

	// Get returns ErrNotFound when the store works and holds nothing.
	Get(ctx context.Context, ref Ref) (Secret, error)
}

// Writer is implemented by stores that can hold a secret the user supplies.
type Writer interface {
	Store
	Set(ctx context.Context, ref Ref, value string) error
	Delete(ctx context.Context, ref Ref) error
}

// Unavailable reports a store that cannot run on this machine, and says why in
// terms the user can act on.
type Unavailable struct {
	Store  string
	Reason string
}

func (e *Unavailable) Error() string {
	return fmt.Sprintf("%s is not available: %s", e.Store, e.Reason)
}

// Resolver consults sources in order.
//
// The environment comes first even though §5.3 calls the OS credential service
// the preferred place to *store* a secret. Storage preference and lookup order
// are different questions: an environment variable is set deliberately, for one
// process, usually to override whatever is on the machine, and a resolver that
// silently preferred the keychain would make that override fail quietly.
type Resolver struct {
	sources []Store
}

func NewResolver(sources ...Store) *Resolver {
	return &Resolver{sources: sources}
}

// Sources reports the chain, so status output describes the resolver that will
// actually run rather than a second description of it that can drift.
func (r *Resolver) Sources() []Store { return r.sources }

func (r *Resolver) Get(ctx context.Context, ref Ref) (Secret, error) {
	if err := ref.valid(); err != nil {
		return Secret{}, err
	}

	var unavailable []string
	for _, src := range r.sources {
		secret, err := src.Get(ctx, ref)
		switch {
		case err == nil:
			if secret.Empty() {
				// A source that answers with an empty string has not supplied a
				// credential, whatever it thinks. Treating it as one produces an
				// Authorization header with nothing after "Bearer".
				continue
			}
			return secret, nil

		case errors.Is(err, ErrNotFound):
			continue

		default:
			var un *Unavailable
			if errors.As(err, &un) {
				// A missing keychain on a headless box is a fact about the
				// machine, not a failure of this lookup. It is collected so the
				// final error can say the chain was short rather than empty.
				unavailable = append(unavailable, un.Error())
				continue
			}
			return Secret{}, fmt.Errorf("%s: %w", src.Name(), err)
		}
	}

	return Secret{}, &NotFoundError{Ref: ref, Consulted: r.names(), Unavailable: unavailable}
}

func (r *Resolver) names() []string {
	names := make([]string, 0, len(r.sources))
	for _, s := range r.sources {
		names = append(names, s.Name())
	}
	return names
}

// NotFoundError reports which sources were consulted, because "no credential
// found" without that list leaves the user guessing where to put one.
type NotFoundError struct {
	Ref         Ref
	Consulted   []string
	Unavailable []string
}

func (e *NotFoundError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "no credential for %s", e.Ref)
	if len(e.Consulted) > 0 {
		fmt.Fprintf(&b, "; looked in %s", strings.Join(e.Consulted, ", then "))
	}
	for _, u := range e.Unavailable {
		fmt.Fprintf(&b, "\n  %s", u)
	}
	fmt.Fprintf(&b, "\n\nStore one with:  sb auth login %s", e.Ref)
	return b.String()
}

func (e *NotFoundError) Is(target error) bool { return target == ErrNotFound }
