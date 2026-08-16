// Package tools implements the built-in tool suite.
//
// The suite stays small on purpose. Everything beyond it arrives over MCP,
// because a one-person project cannot build the long tail but can build the
// socket the long tail plugs into (design principle 5). Phase 0 shipped the
// four tools §19.2 names — read, write, edit, and exec — and glob and grep
// joined them so a model can search a tree without shelling out to whatever
// this host happens to have installed.
package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/cj-vana/switchboard/internal/execution"
	"github.com/cj-vana/switchboard/internal/permission"
	"github.com/cj-vana/switchboard/internal/provider"
)

type Result struct {
	Content string
	IsError bool
}

func errorf(format string, args ...any) (Result, error) {
	return Result{Content: fmt.Sprintf(format, args...), IsError: true}, nil
}

// Plan is a validated tool call that has not run yet. Splitting validation from
// execution lets the caller check policy against the real arguments, so a
// prompt names the actual path or command rather than a raw JSON blob.
type Plan struct {
	Request permission.Request
	Run     func(ctx context.Context) (Result, error)
}

type Tool interface {
	Name() string
	Description() string
	Schema() json.RawMessage

	// ParallelSafe reports whether concurrent calls to this tool can be issued
	// together. Only reads qualify in this suite.
	ParallelSafe() bool

	// Plan parses and validates input. A returned error is a malformed call,
	// which the loop reports back to the model as a tool error so it can
	// correct itself (§10.3).
	Plan(input json.RawMessage) (Plan, error)
}

// Registry holds the tool suite for one workspace.
type Registry struct {
	root       string
	capability execution.Capability
	versions   *fileVersions
	todos      *todoState
	tools      map[string]Tool
	order      []string
}

// NewRegistry binds the suite to a workspace and to whatever confinement this
// host provides. The capability is carried rather than re-detected so that the
// wrapper the exec tool applies is the same one the permission engine consulted
// when it decided whether approval was needed.
func NewRegistry(workspace string, capability execution.Capability) (*Registry, error) {
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return nil, err
	}
	// The root is resolved once so that every later containment check compares
	// resolved paths against a resolved root.
	root, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("resolving workspace %s: %w", workspace, err)
	}

	r := &Registry{
		root:       root,
		capability: capability,
		versions:   &fileVersions{seen: map[string]string{}},
		todos:      &todoState{},
		tools:      map[string]Tool{},
	}
	r.add(&readTool{r})
	r.add(&writeTool{r})
	r.add(&editTool{r})
	r.add(&execTool{r})
	r.add(&globTool{r})
	r.add(&grepTool{r})
	r.add(&todoTool{r})
	return r, nil
}

func (r *Registry) add(t Tool) {
	r.tools[t.Name()] = t
	r.order = append(r.order, t.Name())
	sort.Strings(r.order)
}

// AddExternal registers a tool provided from outside the suite — an MCP
// server's, bridged. It exists for session assembly only: the definitions
// sit in the frozen zone of the context layout (§6.1), so the set must be
// complete before the first request goes out and must not change after.
func (r *Registry) AddExternal(t Tool) error {
	if _, exists := r.tools[t.Name()]; exists {
		return fmt.Errorf("tool %s is already registered", t.Name())
	}
	r.add(t)
	return nil
}

func (r *Registry) Root() string { return r.root }

func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Definitions renders the suite for a provider request. The order is
// deterministic because tool definitions sit in the frozen zone of the context
// layout, and a set that reshuffles between requests would invalidate the
// cached prefix on every turn (§6.1).
func (r *Registry) Definitions() []provider.ToolDefinition {
	defs := make([]provider.ToolDefinition, 0, len(r.order))
	for _, name := range r.order {
		t := r.tools[name]
		defs = append(defs, provider.ToolDefinition{
			Name:        t.Name(),
			Description: t.Description(),
			Schema:      t.Schema(),
		})
	}
	return defs
}

// fileVersions records the content hash of every file the agent has read, which
// is what lets a write detect that something else changed the file in between.
//
// The map starts empty on resume. That is correct rather than unfortunate: the
// agent's knowledge of a file came from a context that no longer exists, so it
// must read again before it may overwrite.
type fileVersions struct {
	mu   sync.Mutex
	seen map[string]string
}

func (v *fileVersions) record(path, hash string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.seen[path] = hash
}

func (v *fileVersions) get(path string) (string, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	h, ok := v.seen[path]
	return h, ok
}

func (v *fileVersions) forget(path string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.seen, path)
}

func hashContent(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

var errOutsideWorkspace = errors.New("path is outside the workspace")

// resolve maps a tool-supplied path to an absolute path inside the workspace.
//
// Symlinks are followed before the containment check, because a link inside the
// workspace pointing at /etc is otherwise a boundary that only looks like one.
// Paths that do not exist yet resolve their longest existing ancestor, so
// creating a file through a symlinked directory is checked the same way.
func (r *Registry) resolve(p string) (string, error) {
	if p == "" {
		return "", errors.New("path is required")
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(r.root, p)
	}

	resolved, err := resolveExistingPrefix(filepath.Clean(p))
	if err != nil {
		return "", err
	}

	rel, err := filepath.Rel(r.root, resolved)
	if err != nil {
		return "", errOutsideWorkspace
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %s", errOutsideWorkspace, p)
	}
	return resolved, nil
}

func resolveExistingPrefix(p string) (string, error) {
	var trailing []string
	cur := p
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			parts := append([]string{resolved}, trailing...)
			return filepath.Join(parts...), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err
		}
		trailing = append([]string{filepath.Base(cur)}, trailing...)
		cur = parent
	}
}

// display renders a path relative to the workspace for prompts and messages.
func (r *Registry) display(abs string) string {
	if rel, err := filepath.Rel(r.root, abs); err == nil {
		return rel
	}
	return abs
}
