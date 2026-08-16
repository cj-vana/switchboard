package main

// MCP servers join the session at assembly time, before the first request,
// because the tool definitions sit in the frozen zone (§6.1). Connection is
// parallel with one deadline for the lot: a server that cannot say hello in
// time is reported and left behind, not waited on while the session stalls.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/cj-vana/switchboard/internal/mcp"
	"github.com/cj-vana/switchboard/internal/permission"
	"github.com/cj-vana/switchboard/internal/tools"
	"github.com/cj-vana/switchboard/internal/trust"
)

const mcpConnectTimeout = 15 * time.Second

type mcpNote struct {
	level string
	text  string
}

// maxBufferedNotes bounds what accumulates while no surface is listening. A
// chatty server must not grow memory forever into a buffer nobody reads.
const maxBufferedNotes = 200

// mcpState is what the session keeps of its MCP wiring: the live clients for
// /mcp and shutdown, and the notes those clients produce. Notes arrive from
// the connect goroutines and from every client's read loop for as long as
// the session lives, while the surfaces and main append and read their own,
// so every access goes through the mutex here rather than through whatever
// lock a caller happens to hold.
type mcpState struct {
	mu      sync.Mutex
	clients []*mcp.Client
	notes   []mcpNote
	deliver func(mcpNote)
}

// add records a note, or hands it straight to the surface once one attached.
func (s *mcpState) add(n mcpNote) {
	s.mu.Lock()
	d := s.deliver
	if d == nil {
		if len(s.notes) < maxBufferedNotes {
			s.notes = append(s.notes, n)
		}
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	// Delivery happens outside the lock: the TUI's deliver blocks until the
	// program's event loop consumes it, and holding the lock across that
	// would stall every client's read loop behind one paint.
	d(n)
}

// attach registers where later notes go and returns what buffered before the
// surface existed, for the caller to render directly: a surface that is not
// yet running cannot be delivered to without deadlocking its own setup.
func (s *mcpState) attach(d func(mcpNote)) []mcpNote {
	s.mu.Lock()
	defer s.mu.Unlock()
	buffered := s.notes
	s.notes = nil
	s.deliver = d
	return buffered
}

func (s *mcpState) clientList() []*mcp.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*mcp.Client(nil), s.clients...)
}

func (s *mcpState) Close() {
	for _, c := range s.clientList() {
		c.Close()
	}
}

// connectMCP loads the user's servers and, when the workspace is trusted,
// the repository's, connects them, and registers every bridged tool. The
// returned rules carry the config's allow lists into the permission engine.
func connectMCP(ctx context.Context, workspace string, ts *trust.Store, registry *tools.Registry) (*mcpState, []permission.Rule) {
	state := &mcpState{}

	var specs []mcp.Spec
	if home, err := os.UserHomeDir(); err == nil {
		userSpecs, err := mcp.LoadSpecs(filepath.Join(home, ".switchboard", mcp.SpecFileName))
		if err != nil {
			state.add(mcpNote{"error", err.Error()})
		}
		specs = append(specs, userSpecs...)
	}

	repoPath := filepath.Join(workspace, ".switchboard", mcp.SpecFileName)
	if _, err := os.Stat(repoPath); err == nil {
		if ts != nil && ts.Trusted(workspace) {
			repoSpecs, err := mcp.LoadSpecs(repoPath)
			if err != nil {
				state.add(mcpNote{"error", err.Error()})
			}
			specs = append(specs, repoSpecs...)
		} else {
			// The repository asked for servers and the user has not said yes.
			// Saying so once beats silently ignoring the file, which reads as
			// a bug to whoever wrote it.
			state.add(mcpNote{"warn",
				"this repository declares MCP servers in .switchboard/mcp.toml; they stay off until you run /trust grant"})
		}
	}

	if len(specs) == 0 {
		return state, nil
	}

	// A name collision across the two files is a configuration error, not a
	// race to the registry: the user file wins and the repo's double is named.
	seen := map[string]bool{}
	deduped := specs[:0]
	for _, s := range specs {
		if seen[s.Name] {
			state.add(mcpNote{"warn",
				fmt.Sprintf("mcp server %s is declared twice; the first declaration wins", s.Name)})
			continue
		}
		seen[s.Name] = true
		deduped = append(deduped, s)
	}
	specs = deduped

	cctx, cancel := context.WithTimeout(ctx, mcpConnectTimeout)
	defer cancel()

	// logf outlives this function: every connected client's read loop keeps
	// calling it for as long as the session runs, which is why it goes
	// through the state's own lock and not one local to this frame.
	logf := func(level, text string) {
		state.add(mcpNote{level, text})
	}

	clients := make([]*mcp.Client, len(specs))
	var wg sync.WaitGroup
	for i, spec := range specs {
		wg.Add(1)
		go func(i int, spec mcp.Spec) {
			defer wg.Done()
			c, err := mcp.Connect(cctx, spec, logf)
			if err != nil {
				logf("error", fmt.Sprintf("mcp server %s did not connect: %v", spec.Name, err))
				return
			}
			clients[i] = c
		}(i, spec)
	}
	wg.Wait()

	var rules []permission.Rule
	var bridged []tools.Tool
	var connected []*mcp.Client
	for _, c := range clients {
		if c == nil {
			continue
		}
		connected = append(connected, c)
		bridged = append(bridged, c.BridgedTools()...)
		rules = append(rules, c.AllowRules()...)
	}
	state.mu.Lock()
	state.clients = connected
	state.mu.Unlock()

	// Registration is sorted so the frozen-zone ordering never depends on
	// which server answered first.
	mcp.SortTools(bridged)
	count := 0
	for _, t := range bridged {
		if err := registry.AddExternal(t); err != nil {
			state.add(mcpNote{"warn", err.Error()})
			continue
		}
		count++
	}
	if count > 0 {
		names := make([]string, 0, len(connected))
		for _, c := range connected {
			names = append(names, c.Name())
		}
		sort.Strings(names)
		state.add(mcpNote{"",
			fmt.Sprintf("mcp: %d tools from %s", count, joinAnd(names))})
	}
	return state, rules
}

func joinAnd(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	last := names[len(names)-1]
	rest := names[:len(names)-1]
	out := ""
	for i, n := range rest {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out + " and " + last
}
