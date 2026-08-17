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

	// checkpoints, when non-nil, captures a file's prior state before write
	// and edit mutate it. Set at assembly; nil means no undo.
	checkpoints Checkpointer
}

// Checkpointer is what the registry needs from a checkpoint recorder. The
// interface lives here so tools does not import the recorder's package.
type Checkpointer interface {
	Record(abs string)
}

// SetCheckpoints wires the recorder in at assembly time.
func (r *Registry) SetCheckpoints(c Checkpointer) { r.checkpoints = c }

// ForgetVersions drops the recorded read state for paths whose contents
// changed outside a tool call — an undo. The next write or edit refuses
// until the model re-reads, which is the read-before-write contract doing
// exactly its job.
func (r *Registry) ForgetVersions(paths []string) {
	for _, p := range paths {
		r.versions.forget(p)
	}
}

// ForgetAllVersions drops every recorded read. It belongs to a session
// swap — /clear, /compact, /fork, an in-place /resume — because those
// replace the context the reads lived in, and the resume rationale on
// fileVersions applies unchanged: the agent's knowledge of a file came from
// a context that no longer exists, so it must read again before it may
// overwrite. A registry that remembered reads across the swap would let a
// fresh context write files it has never seen.
func (r *Registry) ForgetAllVersions() { r.versions.forgetAll() }

// recordUndo is called by mutating tools before they touch a file.
func (r *Registry) recordUndo(abs string) {
	if r.checkpoints != nil {
		r.checkpoints.Record(abs)
	}
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
		versions:   &fileVersions{seen: map[string]string{}, whole: map[string]string{}},
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
	client := newWebClient()
	r.add(&websearchTool{client: client, endpoint: ddgEndpoint})
	r.add(&webfetchTool{client: client})
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

// CoreNames lists the built-in suite, sorted. It exists so assembly can
// validate a configured tool grant — a named agent's — without building a
// registry; the test tying it to NewRegistry is what keeps the two honest.
func CoreNames() []string {
	return []string{"edit", "exec", "glob", "grep", "read", "todo", "webfetch", "websearch", "write"}
}

// Restrict narrows the registry to the named tools. Session assembly only,
// for the same frozen-zone reason as AddExternal: a suite that shrinks after
// the first request would invalidate the cached prefix. It can only narrow —
// a name the registry does not hold is an error, never an addition.
func (r *Registry) Restrict(names []string) error {
	keep := map[string]bool{}
	for _, name := range names {
		if _, ok := r.tools[name]; !ok {
			return fmt.Errorf("tool %s is not in the suite", name)
		}
		keep[name] = true
	}
	kept := r.order[:0]
	for _, name := range r.order {
		if keep[name] {
			kept = append(kept, name)
		} else {
			delete(r.tools, name)
		}
	}
	r.order = kept
	return nil
}

func (r *Registry) Root() string { return r.root }

// Resolve and Display expose workspace containment and the relative
// rendering to first-party tools assembled outside this package — the LSP
// pair — so an external tool answers with the same paths and refuses the
// same escapes as the built-in suite.
func (r *Registry) Resolve(path string) (string, error) { return r.resolve(path) }
func (r *Registry) Display(abs string) string           { return r.display(abs) }

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

	// whole records the hash of content the model received complete: a full
	// read, uncapped. It backs the read tool's re-injection skip (§6.7) and
	// is deliberately narrower than seen — a partial read updates seen for
	// the stale check while proving nothing about what the context holds, so
	// it must never arm the skip.
	whole map[string]string
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
	delete(v.whole, path)
}

func (v *fileVersions) forgetAll() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.seen = map[string]string{}
	v.whole = map[string]string{}
}

func (v *fileVersions) recordWhole(path, hash string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.whole[path] = hash
}

func (v *fileVersions) getWhole(path string) (string, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	hash, ok := v.whole[path]
	return hash, ok
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
