package credential

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSecurity stands in for the system command and records exactly what it was
// invoked with. It is a shell script rather than a Go test binary re-exec so
// that the recorded argv is the real argv the operating system saw.
func fakeSecurity(t *testing.T) (store *OSStore, argvLog string) {
	t.Helper()
	dir := t.TempDir()
	argvLog = filepath.Join(dir, "argv")
	vault := filepath.Join(dir, "vault")

	body := `#!/bin/sh
printf '%s\n' "$*" >> ` + argvLog + `
case "$1" in
  add-generic-password)
    # The real command prompts twice and reads both from stdin.
    read -r first
    read -r second
    [ "$first" = "$second" ] || exit 1
    printf '%s' "$first" > ` + vault + `
    ;;
  find-generic-password)
    [ -f ` + vault + ` ] || exit 44
    cat ` + vault + `
    printf '\n'
    ;;
  delete-generic-password)
    [ -f ` + vault + ` ] || exit 44
    rm -f ` + vault + `
    ;;
esac
`
	path := filepath.Join(dir, "security")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return &OSStore{bin: path}, argvLog
}

// The command line of every process is readable by every user on the machine.
// Passing a credential as an argument would publish it there for as long as the
// call runs, which is why the value goes over standard input instead.
func TestKeychainKeepsTheSecretOutOfArgv(t *testing.T) {
	store, argvLog := fakeSecurity(t)
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
	// The empty -w flag is what makes the command read from stdin. Losing it is
	// exactly the change this test exists to catch, and without it the flag
	// would take the next argument as the value.
	if !strings.Contains(string(argv), "add-generic-password") {
		t.Errorf("no store call was made:\n%s", argv)
	}
}

func TestKeychainMissIsNotAFailure(t *testing.T) {
	store, _ := fakeSecurity(t)

	_, err := store.Get(context.Background(), Ref{Provider: "anthropic"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want a miss so the resolver moves on", err)
	}
}

// The command reads the value as a line, so an embedded newline would store a
// truncated secret and report success.
func TestKeychainRefusesAMultilineValue(t *testing.T) {
	store, _ := fakeSecurity(t)

	err := store.Set(context.Background(), Ref{Provider: "anthropic"}, "first-line\nsecond-line")
	if err == nil {
		t.Fatal("a value with a newline was accepted; it would have been silently truncated")
	}
}

// requireLiveKeychain guards the tests that touch the user's real login
// keychain. They are not part of an ordinary run, because a test suite should
// not write to a credential store as a side effect of `go test ./...`.
func requireLiveKeychain(t *testing.T) {
	t.Helper()
	if os.Getenv("SB_LIVE") == "" {
		t.Skip("set SB_LIVE=1 to exercise the real login keychain")
	}
}

func TestLiveKeychainRoundTrip(t *testing.T) {
	requireLiveKeychain(t)

	ctx := context.Background()
	store := NewOSStore()
	ref := Ref{Provider: "sb-selftest", Account: "round-trip"}

	// A leftover item from an interrupted run would make the write look like it
	// worked when it had not.
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
	if got.Source != SourceKeychain {
		t.Errorf("source = %q", got.Source)
	}

	// Updating in place rather than failing on an existing item is what -U
	// buys, and a store that silently kept the old value would authenticate
	// with a key the user had already replaced.
	const rotated = "sk-rotated-9876543210"
	if err := store.Set(ctx, ref, rotated); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Get(ctx, ref); err != nil {
		t.Fatal(err)
	} else if got.Expose() != rotated {
		t.Errorf("after rotation the store returned %q", got.Expose())
	}

	if err := store.Delete(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, ref); !errors.Is(err, ErrNotFound) {
		t.Errorf("after deletion, err = %v, want a miss", err)
	}
}
