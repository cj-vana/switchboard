package credential

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// OSStore is the macOS Keychain, reached through the `security` command.
//
// The command rather than the framework, because binding SecKeychain means cgo
// and cgo means the static, cross-compiled binary §18 asks for stops being
// possible. §5.3 already describes replaceable auth integrations as helper
// processes with a narrow protocol; this is that shape applied to the platform
// store itself.
//
// The secret never appears in argv. `security add-generic-password -w` with no
// value prompts twice on standard input, so the value is piped, which keeps it
// out of `ps` and out of any process listing a shared machine exposes.
type OSStore struct {
	// bin exists for tests. Empty means `security` on PATH.
	bin string
}

func NewOSStore() *OSStore { return &OSStore{} }

func (s *OSStore) Name() string { return "macOS Keychain" }

func (s *OSStore) tool() string {
	if s.bin != "" {
		return s.bin
	}
	return "security"
}

// notFoundStatus is what `security` exits with when the item is absent. It is
// checked by number because the message is prose and has changed between
// releases.
const notFoundStatus = 44

// deniedStatus is what a write exits with when the keychain will not authorize
// it. The message it prints, "the authorization was canceled by the user",
// describes a dialog the user may never have seen, so the two conditions that
// actually produce it are named instead.
const deniedStatus = 154

func (s *OSStore) Get(ctx context.Context, ref Ref) (Secret, error) {
	if err := ref.valid(); err != nil {
		return Secret{}, err
	}

	cmd := exec.CommandContext(ctx, s.tool(),
		"find-generic-password", "-s", service(ref), "-a", account(ref), "-w")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if unavailable := s.unavailable(err); unavailable != nil {
			return Secret{}, unavailable
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == notFoundStatus {
			return Secret{}, ErrNotFound
		}
		return Secret{}, fmt.Errorf("reading the keychain: %w%s", err, diagnostics(stderr.String()))
	}

	// -w writes the password followed by a newline and nothing else.
	return New(stdout.String(), SourceKeychain, "login keychain"), nil
}

func (s *OSStore) Set(ctx context.Context, ref Ref, value string) error {
	if err := ref.valid(); err != nil {
		return err
	}
	if strings.TrimSpace(value) == "" {
		return errors.New("refusing to store an empty credential")
	}
	if strings.ContainsAny(value, "\r\n") {
		// The command reads the value as a line, so an embedded newline would
		// store a truncated secret and report success.
		return errors.New("a credential containing a newline cannot be stored")
	}

	cmd := exec.CommandContext(ctx, s.tool(),
		"add-generic-password",
		"-s", service(ref),
		"-a", account(ref),
		"-D", "Switchboard provider credential",
		"-U", // update in place rather than failing on an existing item
		"-w", // no value: read it from stdin instead of argv
	)
	// The prompt asks twice.
	cmd.Stdin = strings.NewReader(value + "\n" + value + "\n")

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if unavailable := s.unavailable(err); unavailable != nil {
			return unavailable
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == deniedStatus {
			return fmt.Errorf("the keychain refused the write%s\n\n"+
				"This is usually one of two things: the login keychain is locked and there is no\n"+
				"desktop session to unlock it, which happens over SSH, or HOME points somewhere\n"+
				"without a login keychain. Either way an environment variable or a credential\n"+
				"helper will work where this will not.", diagnostics(stderr.String()))
		}
		return fmt.Errorf("storing in the keychain: %w%s", err, diagnostics(stderr.String()))
	}
	return nil
}

func (s *OSStore) Delete(ctx context.Context, ref Ref) error {
	if err := ref.valid(); err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, s.tool(),
		"delete-generic-password", "-s", service(ref), "-a", account(ref))

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if unavailable := s.unavailable(err); unavailable != nil {
			return unavailable
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == notFoundStatus {
			return ErrNotFound
		}
		return fmt.Errorf("removing from the keychain: %w%s", err, diagnostics(stderr.String()))
	}
	return nil
}

func (s *OSStore) unavailable(err error) error {
	if execErr := new(exec.Error); errors.As(err, &execErr) {
		return &Unavailable{Store: s.Name(), Reason: "the security command is not on PATH"}
	}
	return nil
}
