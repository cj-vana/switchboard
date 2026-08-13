package credential

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const value = "sk-not-a-real-key-0123456789"

// A Secret that reaches a log line, an error, or the JSON session record must
// print as a placeholder. This is the single most consequential property in the
// package: everything else is a policy that can be argued about, and this is
// the one that decides whether a leak is possible at all.
func TestSecretNeverRenders(t *testing.T) {
	s := New(value, SourceEnv, "SB_TEST_API_KEY")

	renderings := map[string]string{
		"%v":       fmt.Sprintf("%v", s),
		"%s":       fmt.Sprintf("%s", s),
		"%q":       fmt.Sprintf("%q", s),
		"%#v":      fmt.Sprintf("%#v", s),
		"%+v":      fmt.Sprintf("%+v", s),
		"print":    fmt.Sprint(s),
		"errorf":   fmt.Errorf("failed with %v", s).Error(),
		"in slice": fmt.Sprintf("%v", []Secret{s}),
		"in map":   fmt.Sprintf("%v", map[string]Secret{"k": s}),
		"in struct": fmt.Sprintf("%v", struct {
			Auth Secret
		}{s}),
	}
	for name, got := range renderings {
		if strings.Contains(got, value) {
			t.Errorf("%s leaked the credential: %s", name, got)
		}
	}

	encoded, err := json.Marshal(map[string]any{"auth": s})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), value) {
		t.Errorf("the JSON encoding leaked the credential: %s", encoded)
	}

	if s.Expose() != value {
		t.Error("the credential did not survive the round trip")
	}
}

func envOf(pairs map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := pairs[name]
		return v, ok
	}
}

func TestEnvNamespacedNameWins(t *testing.T) {
	ref := Ref{Provider: "anthropic", Account: "first-party"}
	store := &EnvStore{lookup: envOf(map[string]string{
		"SB_ANTHROPIC_FIRST_PARTY_API_KEY": "surface-scoped",
		"SB_ANTHROPIC_API_KEY":             "provider-scoped",
		"ANTHROPIC_API_KEY":                "conventional",
	})}

	got, err := store.Get(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if got.Expose() != "surface-scoped" {
		t.Errorf("resolved %q; the most specific name has to win, or a second key for one provider is unreachable", got.Expose())
	}
}

func TestEnvFallsBackToTheConventionalName(t *testing.T) {
	store := &EnvStore{lookup: envOf(map[string]string{"ANTHROPIC_API_KEY": "conventional"})}

	got, err := store.Get(context.Background(), Ref{Provider: "anthropic", Account: "first-party"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Expose() != "conventional" {
		t.Errorf("resolved %q, want the vendor's documented variable", got.Expose())
	}
	if got.Detail != "ANTHROPIC_API_KEY" {
		t.Errorf("detail = %q; status output has to be able to name the variable", got.Detail)
	}
}

// An OpenAI-compatible endpoint is not OpenAI. Honoring OPENAI_API_KEY here
// would put a key issued to one company into a request bound for whatever
// server the profile points at.
func TestCompatibleEndpointDoesNotBorrowAVendorKey(t *testing.T) {
	store := &EnvStore{lookup: envOf(map[string]string{"OPENAI_API_KEY": "issued-to-openai"})}

	_, err := store.Get(context.Background(), Ref{Provider: "openaicompat", Account: "some-gateway"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v; a compatibility provider must not pick up a vendor's key", err)
	}

	for _, name := range EnvNames(Ref{Provider: "openaicompat", Account: "some-gateway"}) {
		if name == "OPENAI_API_KEY" {
			t.Error("OPENAI_API_KEY is in the openaicompat lookup list")
		}
	}
}

func TestEmptyEnvIsNotACredential(t *testing.T) {
	store := &EnvStore{lookup: envOf(map[string]string{"SB_ANTHROPIC_API_KEY": "   "})}

	_, err := store.Get(context.Background(), Ref{Provider: "anthropic"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v; whitespace is not a credential", err)
	}
}

// script writes a helper command for the tests below.
func script(t *testing.T, body string) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "helper.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return []string{path}
}

func TestHelperSuppliesTheCredential(t *testing.T) {
	store := &HelperStore{Command: script(t, `printf '%s\n' "`+value+`"`)}

	got, err := store.Get(context.Background(), Ref{Provider: "anthropic"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Expose() != value {
		t.Errorf("helper output = %q", got.Expose())
	}
	if got.Source != SourceHelper {
		t.Errorf("source = %q", got.Source)
	}
}

func TestHelperIsToldWhichCredentialIsWanted(t *testing.T) {
	store := &HelperStore{Command: script(t, `printf '%s/%s\n' "$SB_CREDENTIAL_PROVIDER" "$SB_CREDENTIAL_ACCOUNT"`)}

	got, err := store.Get(context.Background(), Ref{Provider: "anthropic", Account: "bedrock"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Expose() != "anthropic/bedrock" {
		t.Errorf("the helper saw %q; one command has to be able to serve every provider", got.Expose())
	}
}

// A helper that is present and broken is a configuration error the user has to
// see. Falling through to the next source would report it as "you have no
// credential", and the user would go store a second copy of one they already
// have.
func TestBrokenHelperStopsTheChain(t *testing.T) {
	resolver := NewResolver(
		&HelperStore{Command: script(t, `echo "vault is locked" >&2; exit 1`)},
		staticStore{secret: New("from-the-next-source", SourceKeychain, "")},
	)

	_, err := resolver.Get(context.Background(), Ref{Provider: "anthropic"})
	if err == nil {
		t.Fatal("a failing helper resolved successfully from a later source")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want a configuration error rather than a miss", err)
	}
	if !strings.Contains(err.Error(), "vault is locked") {
		t.Errorf("err = %v; the helper's diagnostics are the only clue the user gets", err)
	}
}

// stdout is the credential channel. A helper that fails partway may have
// written part of a secret to it, so it is never quoted back.
func TestHelperStdoutStaysOutOfErrors(t *testing.T) {
	store := &HelperStore{Command: script(t, `printf '%s' "`+value+`"; echo "then it died" >&2; exit 3`)}

	_, err := store.Get(context.Background(), Ref{Provider: "anthropic"})
	if err == nil {
		t.Fatal("a nonzero exit resolved successfully")
	}
	if strings.Contains(err.Error(), value) {
		t.Errorf("the error quoted the helper's stdout: %v", err)
	}
	if !strings.Contains(err.Error(), "then it died") {
		t.Errorf("the error dropped the helper's diagnostics: %v", err)
	}
}

func TestMissingHelperIsUnavailableNotBroken(t *testing.T) {
	resolver := NewResolver(
		&HelperStore{Command: []string{"sb-no-such-helper-exists"}},
		staticStore{secret: New("from-the-next-source", SourceKeychain, "")},
	)

	got, err := resolver.Get(context.Background(), Ref{Provider: "anthropic"})
	if err != nil {
		t.Fatalf("a helper that is not installed should not stop the chain: %v", err)
	}
	if got.Expose() != "from-the-next-source" {
		t.Errorf("resolved %q", got.Expose())
	}
}

func TestEnvBeatsTheStore(t *testing.T) {
	resolver := NewResolver(
		&EnvStore{lookup: envOf(map[string]string{"SB_ANTHROPIC_API_KEY": "from-the-environment"})},
		staticStore{secret: New("from-the-keychain", SourceKeychain, "")},
	)

	got, err := resolver.Get(context.Background(), Ref{Provider: "anthropic"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Expose() != "from-the-environment" {
		t.Errorf("resolved %q; an exported variable is a deliberate override and has to win", got.Expose())
	}
}

func TestNotFoundNamesWhatWasConsulted(t *testing.T) {
	resolver := NewResolver(
		&EnvStore{lookup: envOf(nil)},
		unavailableStore{name: "Secret Service keyring", reason: "no D-Bus session bus"},
	)

	_, err := resolver.Get(context.Background(), Ref{Provider: "anthropic", Account: "first-party"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want a not-found", err)
	}
	msg := err.Error()
	for _, want := range []string{"anthropic/first-party", "environment", "no D-Bus session bus", "sb auth login"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message does not mention %q:\n%s", want, msg)
		}
	}
}

func TestReferenceWithoutAProviderIsRejected(t *testing.T) {
	if _, err := NewResolver().Get(context.Background(), Ref{}); err == nil {
		t.Error("an empty reference resolved")
	}
}

// The config file has no field a secret can be written into, which is the
// mechanism §5.3 relies on rather than a warning in the documentation.
//
// Names that merely contain "token" are fine: a token endpoint is a URL. What
// must not exist is a field that holds a credential value.
func TestSettingsCannotCarryASecret(t *testing.T) {
	encoded, err := json.Marshal(Settings{
		Env:    "SOME_VAR",
		Helper: []string{"op", "read", "op://vault/item"},
		OAuth:  OAuthSettings{ClientID: "abc", AuthorizeURL: "https://x/authorize", TokenURL: "https://x/token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	lowered := strings.ToLower(string(encoded))

	for _, forbidden := range []string{
		"apikey", "api_key", "accesstoken", "access_token",
		"refreshtoken", "refresh_token", "password", "secret",
	} {
		if strings.Contains(lowered, `"`+forbidden) {
			t.Errorf("Settings has a %q field, which is somewhere a secret can be pasted: %s", forbidden, encoded)
		}
	}
}

// A command-line client cannot keep a client secret, so this flow is a public
// client and PKCE is what stands in for one. A ClientSecret field would invite
// exactly the plaintext-in-config storage §5.3 rules out.
func TestOAuthHasNoClientSecret(t *testing.T) {
	encoded, err := json.Marshal(OAuthSettings{ClientID: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(encoded)), "clientsecret") {
		t.Errorf("OAuthSettings carries a client secret: %s", encoded)
	}
}

func TestChainOrder(t *testing.T) {
	names := []string{}
	for _, s := range Chain(Settings{Helper: []string{"true"}}).Sources() {
		names = append(names, s.Name())
	}
	if len(names) != 3 || names[0] != "environment" || !strings.Contains(names[1], "helper") {
		t.Errorf("chain = %v, want environment, then helper, then the platform store", names)
	}

	bare := Chain(Settings{}).Sources()
	if len(bare) != 2 {
		t.Errorf("an unconfigured chain has %d sources, want the environment and the platform store", len(bare))
	}
}

// Storing a credential that a store would silently truncate is worse than
// refusing: the user would believe the key is saved and every request would
// fail authentication with no explanation.
func TestUnstorableValuesAreRefused(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no writable credential store on this platform")
	}
	store := NewOSStore()
	w, ok := any(store).(Writer)
	if !ok {
		t.Skip("this platform's store does not write")
	}
	if err := w.Set(context.Background(), Ref{Provider: "sb-test"}, "  "); err == nil {
		t.Error("an empty credential was accepted")
	}
}

type staticStore struct{ secret Secret }

func (s staticStore) Name() string { return "test store" }
func (s staticStore) Get(context.Context, Ref) (Secret, error) {
	return s.secret, nil
}

type unavailableStore struct{ name, reason string }

func (s unavailableStore) Name() string { return s.name }
func (s unavailableStore) Get(context.Context, Ref) (Secret, error) {
	return Secret{}, &Unavailable{Store: s.name, Reason: s.reason}
}
