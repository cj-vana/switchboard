package credential

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSecretTool stands in for the system command and records exactly what it
// was invoked with.
func fakeSecretTool(t *testing.T) (store *OSStore, argvLog string) {
	t.Helper()
	dir := t.TempDir()
	argvLog = filepath.Join(dir, "argv")
	vault := filepath.Join(dir, "vault")

	// lookup writes the secret with no trailing newline, and is silent on a
	// miss: that silence is how a miss is told from a broken bus, so the fake
	// has to reproduce it exactly.
	body := `#!/bin/sh
printf '%s\n' "$*" >> ` + argvLog + `
case "$1" in
  store)  cat > ` + vault + ` ;;
  lookup) [ -f ` + vault + ` ] || exit 1
          cat ` + vault + ` ;;
  clear)  rm -f ` + vault + ` ;;
esac
`
	path := filepath.Join(dir, "secret-tool")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/fake/for/this/test")
	return &OSStore{bin: path}, argvLog
}

// The command line of every process is readable by every user on the machine.
func TestKeyringKeepsTheSecretOutOfArgv(t *testing.T) {
	store, argvLog := fakeSecretTool(t)
	ctx := context.Background()
	ref := Ref{Provider: "anthropic", Account: "first-party"}

	if err := store.Set(ctx, ref, value); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if got.Expose() != value {
		t.Errorf("read back %q", got.Expose())
	}

	argv, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(argv), value) {
		t.Errorf("the credential appeared on a command line, where any user on the machine can read it:\n%s", argv)
	}
}

func TestKeyringMissIsNotAFailure(t *testing.T) {
	store, _ := fakeSecretTool(t)

	_, err := store.Get(context.Background(), Ref{Provider: "anthropic"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want a miss so the resolver moves on", err)
	}
}

// clear succeeds whether or not anything matched. Reporting a removal that did
// not happen would let a user believe a credential is gone when it is stored
// under another name and still authenticating.
func TestKeyringDeletingWhatIsNotThereReportsAMiss(t *testing.T) {
	store, _ := fakeSecretTool(t)

	err := store.Delete(context.Background(), Ref{Provider: "anthropic"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want a miss rather than a false success", err)
	}
}

// A machine with no session bus has no keyring. That is a fact about the
// machine, not a failure of the lookup, and the resolver has to be able to
// carry on to the sources that work headlessly.
func TestNoSessionBusIsUnavailableNotAnError(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	store := NewOSStore()

	_, err := store.Get(context.Background(), Ref{Provider: "anthropic"})
	var un *Unavailable
	if !errors.As(err, &un) {
		t.Fatalf("err = %v, want an Unavailable", err)
	}

	resolver := NewResolver(&EnvStore{lookup: envOf(map[string]string{"SB_ANTHROPIC_API_KEY": "headless"})}, store)
	got, resolveErr := resolver.Get(context.Background(), Ref{Provider: "anthropic"})
	if resolveErr != nil {
		t.Fatalf("a missing keyring stopped the chain: %v", resolveErr)
	}
	if got.Expose() != "headless" {
		t.Errorf("resolved %q", got.Expose())
	}
}

func requireLiveKeyring(t *testing.T) {
	t.Helper()
	if os.Getenv("SB_LIVE") == "" {
		t.Skip("set SB_LIVE=1 to exercise a real Secret Service")
	}
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		t.Skip("no session bus, so there is no keyring to test against")
	}
}

func TestLiveKeyringRoundTrip(t *testing.T) {
	requireLiveKeyring(t)

	ctx := context.Background()
	store := NewOSStore()
	ref := Ref{Provider: "sb-selftest", Account: "round-trip"}

	_ = store.Delete(ctx, ref)
	if _, err := store.Get(ctx, ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the test item exists before the test wrote it: %v", err)
	}
	t.Cleanup(func() { _ = store.Delete(ctx, ref) })

	if err := store.Set(ctx, ref, value); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if got.Expose() != value {
		t.Errorf("read back %q, want the value that was stored", got.Expose())
	}

	const rotated = "sk-rotated-9876543210"
	if err := store.Set(ctx, ref, rotated); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Get(ctx, ref); err != nil {
		t.Fatal(err)
	} else if got.Expose() != rotated {
		t.Errorf("after rotation the store returned %q; a store that keeps the old value "+
			"authenticates with a key the user has already replaced", got.Expose())
	}

	if err := store.Delete(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, ref); !errors.Is(err, ErrNotFound) {
		t.Errorf("after deletion, err = %v, want a miss", err)
	}
}
