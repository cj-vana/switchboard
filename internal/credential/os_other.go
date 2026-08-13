//go:build !darwin && !linux

package credential

import "context"

// OSStore stands in on platforms whose credential service this build has not
// been run against.
//
// Windows has a credential manager and §5.3 names it, but there is no shipped
// command-line tool that reads and writes generic credentials the way
// `security` and `secret-tool` do, and reaching the API means cgo or a
// hand-written syscall layer that nobody here has tested on Windows. Shipping a
// backend nobody has run would put a claim in `sb auth status` that the machine
// cannot honor.
//
// So this reports itself unavailable and names the two paths that do work
// everywhere. Storage stays absent rather than degrading to a file, because a
// mode 0600 file is access control and not encryption (§5.3), and the failure
// mode of writing one is that a credential is at rest in the clear.
type OSStore struct{}

func NewOSStore() *OSStore { return &OSStore{} }

func (s *OSStore) Name() string { return "OS credential service" }

func (s *OSStore) Get(context.Context, Ref) (Secret, error) {
	return Secret{}, s.unavailable()
}

func (s *OSStore) Set(context.Context, Ref, string) error { return s.unavailable() }

func (s *OSStore) Delete(context.Context, Ref) error { return s.unavailable() }

func (s *OSStore) unavailable() error {
	return &Unavailable{
		Store: s.Name(),
		Reason: "this build has no tested credential-service backend for this platform; " +
			"use an environment variable or a credential helper",
	}
}
