package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/cjvana/switchboard/internal/config"
	"github.com/cjvana/switchboard/internal/credential"
)

const authUsage = `usage:
  sb auth status                    show where each provider's credential would come from
  sb auth login <provider>[/<surface>]   store a credential
  sb auth logout <provider>[/<surface>]  remove a stored credential

A credential is read from standard input, so it can be piped in a script and is
never taken from the command line, where any user on the machine could read it.`

func runAuth(ctx context.Context, args []string, cfg *config.Config) error {
	if len(args) == 0 {
		return errors.New(authUsage)
	}
	switch args[0] {
	case "status":
		return authStatus(ctx, cfg)
	case "login":
		if len(args) != 2 {
			return errors.New(authUsage)
		}
		return authLogin(ctx, args[1])
	case "logout":
		if len(args) != 2 {
			return errors.New(authUsage)
		}
		return authLogout(ctx, args[1])
	default:
		return fmt.Errorf("unknown auth command %q\n\n%s", args[0], authUsage)
	}
}

// parseRef reads "provider" or "provider/surface". The surface is part of the
// reference because one provider legitimately has more than one credential: a
// first-party key and a gateway key are not interchangeable.
func parseRef(arg string) (credential.Ref, error) {
	providerName, surface, _ := strings.Cut(strings.TrimSpace(arg), "/")
	if providerName == "" {
		return credential.Ref{}, errors.New("name a provider, for example: sb auth login anthropic/first-party")
	}
	return credential.Ref{Provider: providerName, Account: surface}, nil
}

// authStatus reports where each configured target's credential would come from,
// and never what it is. Answering "will this work" is the whole job; a status
// command that prints a key is a status command that ends up in a screen share.
func authStatus(ctx context.Context, cfg *config.Config) error {
	refs := refsInUse(cfg)
	if len(refs) == 0 {
		fmt.Println("no tiers are configured, so no credentials are needed yet")
		return nil
	}

	for _, ref := range refs {
		resolver := credential.Chain(cfg.AuthFor(ref.Provider))
		fmt.Printf("%s\n", ref)

		secret, err := resolver.Get(ctx, ref)
		if err != nil {
			var notFound *credential.NotFoundError
			if errors.As(err, &notFound) {
				fmt.Printf("  not found; looked in %s\n", strings.Join(notFound.Consulted, ", then "))
				for _, u := range notFound.Unavailable {
					fmt.Printf("    %s\n", u)
				}
			} else {
				fmt.Printf("  error: %v\n", err)
			}
			continue
		}
		// secret.Source and secret.Detail describe the resolution. The value
		// itself has no rendering that shows it.
		fmt.Printf("  found in %s (%s)\n", secret.Source, secret.Detail)
	}
	return nil
}

// refsInUse lists the credentials the configured ladder would need, so status
// answers a question about this machine's setup rather than listing every
// provider that exists.
func refsInUse(cfg *config.Config) []credential.Ref {
	var refs []credential.Ref
	seen := map[string]bool{}
	for _, tier := range cfg.Tiers {
		ref := credential.Ref{Provider: tier.Target.Provider, Account: tier.Target.Surface}
		if seen[ref.String()] {
			continue
		}
		seen[ref.String()] = true
		refs = append(refs, ref)
	}
	return refs
}

func authLogin(ctx context.Context, arg string) error {
	ref, err := parseRef(arg)
	if err != nil {
		return err
	}

	store := credential.NewOSStore()
	writer, ok := any(store).(credential.Writer)
	if !ok {
		return fmt.Errorf("%s cannot store credentials on this platform; "+
			"set an environment variable or configure a credential helper", store.Name())
	}

	value, err := readSecret(fmt.Sprintf("Credential for %s: ", ref))
	if err != nil {
		return err
	}
	if strings.TrimSpace(value) == "" {
		return errors.New("no credential was supplied")
	}

	if err := writer.Set(ctx, ref, value); err != nil {
		return err
	}
	fmt.Printf("stored %s in the %s\n", ref, store.Name())
	return nil
}

func authLogout(ctx context.Context, arg string) error {
	ref, err := parseRef(arg)
	if err != nil {
		return err
	}

	store := credential.NewOSStore()
	writer, ok := any(store).(credential.Writer)
	if !ok {
		return fmt.Errorf("%s stores nothing on this platform", store.Name())
	}

	if err := writer.Delete(ctx, ref); err != nil {
		if errors.Is(err, credential.ErrNotFound) {
			return fmt.Errorf("no stored credential for %s", ref)
		}
		return err
	}
	fmt.Printf("removed %s from the %s\n", ref, store.Name())

	// A variable in the environment outlives the stored copy and would keep
	// working, which looks like the removal silently failed.
	if leftover := environmentStillSupplies(ctx, ref); leftover != "" {
		fmt.Printf("\nnote: %s is still set in this environment and takes precedence,\n"+
			"so requests will keep authenticating until it is unset\n", leftover)
	}
	return nil
}

func environmentStillSupplies(ctx context.Context, ref credential.Ref) string {
	env := &credential.EnvStore{}
	secret, err := env.Get(ctx, ref)
	if err != nil {
		return ""
	}
	return secret.Detail
}

// readSecret takes the credential from standard input. When that is a terminal
// it turns off echo first, so the value is not left on screen or in a scrollback
// buffer; when it is a pipe it reads a line, which is what a script needs.
func readSecret(prompt string) (string, error) {
	info, err := os.Stdin.Stat()
	if err != nil {
		return "", err
	}
	interactive := info.Mode()&os.ModeCharDevice != 0

	if !interactive {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && err != io.EOF {
			return "", err
		}
		return strings.TrimRight(line, "\r\n"), nil
	}

	fmt.Print(prompt)
	restore, echoOff := suppressEcho()
	if !echoOff {
		fmt.Println("\n(echo could not be turned off, so what you type will be visible)")
		fmt.Print(prompt)
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	restore()
	fmt.Println()
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// suppressEcho turns off terminal echo through stty.
//
// The alternative is a termios ioctl per platform, which means either a
// dependency or unsafe pointer work that would have to be tested on every
// target. Shelling out to a POSIX-standard tool is the same trade already made
// for the credential stores themselves, and it fails visibly: when stty is not
// available the caller says so rather than reading a credential onto the screen
// while pretending otherwise.
func suppressEcho() (restore func(), ok bool) {
	cmd := exec.Command("stty", "-echo")
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		return func() {}, false
	}
	return func() {
		on := exec.Command("stty", "echo")
		on.Stdin = os.Stdin
		_ = on.Run()
	}, true
}
