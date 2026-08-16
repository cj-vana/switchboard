// Package trust records which workspaces the user has decided to extend
// execution trust to.
//
// The distinction it keeps is the one the custom-command loader already
// draws: ~/.switchboard is the user speaking, a repository's .switchboard is
// whoever was cloned. Content a repository provides may speak to the model;
// configuration that starts processes on the user's machine — an MCP server,
// a hook — runs only after the user has said this specific checkout may do
// that. The grant is per resolved path, persisted, and revocable, so cloning
// a repository is never by itself permission to execute what it declares.
package trust

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
)

const FileName = "trust.toml"

// header explains the file to whoever opens it, the same contract as
// config.toml: the program regenerates it, hand annotation does not survive.
const header = `# Workspaces trusted to run what they declare (MCP servers, hooks).
#
# sb rewrites this file when trust is granted or revoked in the TUI, and a
# rewrite regenerates everything: comments placed here do not survive.
# Remove an entry (or run /trust revoke there) to withdraw a grant.

`

type Grant struct {
	GrantedAt time.Time `toml:"granted_at"`
}

// Store is the persisted set of grants. The zero value is unusable; open one
// with Open or OpenFile.
type Store struct {
	path string

	mu     sync.Mutex
	grants map[string]Grant
}

// Path is where the store lives: beside config.toml, not inside a workspace,
// because a file the repository can edit is a grant the repository can give
// itself.
func Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".switchboard", FileName), nil
}

func Open() (*Store, error) {
	p, err := Path()
	if err != nil {
		return nil, fmt.Errorf("no home directory for the trust store: %w", err)
	}
	return OpenFile(p)
}

func OpenFile(path string) (*Store, error) {
	s := &Store{path: path, grants: map[string]Grant{}}
	var file struct {
		Workspaces map[string]Grant `toml:"workspaces"`
	}
	if _, err := toml.DecodeFile(path, &file); err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if file.Workspaces != nil {
		s.grants = file.Workspaces
	}
	return s, nil
}

// resolve normalizes a workspace the way the tool registry resolves its root,
// so a grant to a path and a session opened through a symlink to it agree.
func resolve(workspace string) (string, error) {
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

func (s *Store) Trusted(workspace string) bool {
	key, err := resolve(workspace)
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.grants[key]
	return ok
}

func (s *Store) Grant(workspace string) error {
	key, err := resolve(workspace)
	if err != nil {
		return fmt.Errorf("cannot resolve %s: %w", workspace, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grants[key] = Grant{GrantedAt: time.Now().UTC()}
	return s.save()
}

func (s *Store) Revoke(workspace string) error {
	key, err := resolve(workspace)
	if err != nil {
		return fmt.Errorf("cannot resolve %s: %w", workspace, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.grants[key]; !ok {
		return nil
	}
	delete(s.grants, key)
	return s.save()
}

// Granted lists the trusted workspaces, sorted, for display.
func (s *Store) Granted() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.grants))
	for k := range s.grants {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// save writes the store atomically, the config.Save contract: a temporary
// file in the same directory, then a rename, so a crash leaves the old file
// rather than half a new one. Caller holds the lock.
func (s *Store) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(s.path), err)
	}

	var file struct {
		Workspaces map[string]Grant `toml:"workspaces"`
	}
	file.Workspaces = s.grants

	tmp, err := os.CreateTemp(filepath.Dir(s.path), FileName+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(header); err != nil {
		tmp.Close()
		return err
	}
	if err := toml.NewEncoder(tmp).Encode(file); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}
