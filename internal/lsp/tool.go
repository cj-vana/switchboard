package lsp

// The definition and references tools. Both take {path, line, symbol} and
// resolve the column themselves by finding the symbol on that line, because
// a model reads file:line off a grep result reliably and invents column
// numbers freely — the input shape follows what the caller actually has.
//
// The server starts on the first call, not at assembly: a session that
// never asks pays nothing. Tool presence is still decided at assembly,
// which is what the frozen zone requires; a server that then fails to
// start is a tool error the model reads, not a broken session.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/tools"
)

const (
	startTimeout = 30 * time.Second
	queryTimeout = 20 * time.Second
	closeTimeout = 3 * time.Second
)

// Server lazily starts one language server and shares it across the tools
// and every registry they are added to.
type Server struct {
	Argv []string
	Root string

	once   sync.Once
	client *Client
	err    error
}

func (s *Server) get() (*Client, error) {
	s.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
		defer cancel()
		s.client, s.err = Start(ctx, s.Argv, s.Root)
	})
	return s.client, s.err
}

// Close shuts the server down if a call ever started it.
func (s *Server) Close() {
	s.once.Do(func() { s.err = fmt.Errorf("never started") })
	if s.client != nil {
		s.client.Close()
	}
}

// Resolver is what the tools need from the registry: workspace containment
// for the path the model names, and the workspace-relative rendering every
// other tool answers with.
type Resolver interface {
	Resolve(path string) (string, error)
	Display(abs string) string
}

// NewDefinition and NewReferences build the two tools over one server.
func NewDefinition(s *Server, r Resolver) tools.Tool {
	return &locateTool{server: s, r: r, name: "definition",
		desc: "Where the symbol is defined. Give the file, the 1-based line, and the symbol " +
			"as it appears on that line — taken straight from a grep or astgrep hit — and " +
			"the language server answers with the defining file:line, precisely, from a " +
			"live syntax model rather than a text search.",
		ask: func(ctx context.Context, c *Client, path string, line, col int) ([]Location, error) {
			return c.Definition(ctx, path, line, col)
		}}
}

func NewReferences(s *Server, r Resolver) tools.Tool {
	return &locateTool{server: s, r: r, name: "references",
		desc: "Every place the symbol is used, declaration included. Give the file, the " +
			"1-based line, and the symbol as it appears on that line; the language server " +
			"answers with file:line for each use, precisely, which is the whole-picture " +
			"question a text search only approximates.",
		ask: func(ctx context.Context, c *Client, path string, line, col int) ([]Location, error) {
			return c.References(ctx, path, line, col)
		}}
}

type locateTool struct {
	server *Server
	r      Resolver
	name   string
	desc   string
	ask    func(context.Context, *Client, string, int, int) ([]Location, error)
}

func (t *locateTool) Name() string        { return t.name }
func (t *locateTool) Description() string { return t.desc }

// ParallelSafe: queries are read-only and the client routes concurrent
// requests by id.
func (t *locateTool) ParallelSafe() bool { return true }

func (t *locateTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "File the symbol appears in, relative to the workspace root."},
    "line": {"type": "integer", "description": "1-based line the symbol appears on."},
    "symbol": {"type": "string", "description": "The symbol's name exactly as written on that line."}
  },
  "required": ["path", "line", "symbol"]
}`)
}

type locateInput struct {
	Path   string `json:"path"`
	Line   int    `json:"line"`
	Symbol string `json:"symbol"`
}

func (t *locateTool) Plan(input json.RawMessage) (tools.Plan, error) {
	var in locateInput
	if err := json.Unmarshal(input, &in); err != nil {
		return tools.Plan{}, fmt.Errorf("%s: %w", t.name, err)
	}
	if in.Path == "" || in.Line < 1 || strings.TrimSpace(in.Symbol) == "" {
		return tools.Plan{}, fmt.Errorf("%s: path, a 1-based line, and the symbol are all required", t.name)
	}
	abs, err := t.r.Resolve(in.Path)
	if err != nil {
		return tools.Plan{}, err
	}
	return tools.Plan{
		Request: permission.Request{
			Tool:   t.name,
			Effect: permission.EffectRead,
			Path:   t.r.Display(abs),
			Detail: fmt.Sprintf("%s at line %d", in.Symbol, in.Line),
		},
		Run: func(ctx context.Context) (tools.Result, error) {
			return t.run(ctx, in, abs)
		},
	}, nil
}

func (t *locateTool) run(ctx context.Context, in locateInput, abs string) (tools.Result, error) {
	col, err := columnOf(abs, in.Line, in.Symbol)
	if err != nil {
		return tools.Result{Content: err.Error(), IsError: true}, nil
	}
	client, err := t.server.get()
	if err != nil {
		return tools.Result{Content: fmt.Sprintf("the language server did not start: %v", err), IsError: true}, nil
	}

	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	locs, err := t.ask(ctx, client, abs, in.Line, col)
	if err != nil {
		return tools.Result{Content: fmt.Sprintf("%s failed: %v", t.name, err), IsError: true}, nil
	}
	if len(locs) == 0 {
		return tools.Result{Content: fmt.Sprintf("the server found nothing for %s at %s:%d",
			in.Symbol, t.r.Display(abs), in.Line)}, nil
	}

	var b strings.Builder
	for _, l := range locs {
		fmt.Fprintf(&b, "%s:%d\n", t.r.Display(l.Path), l.Line)
	}
	return tools.Result{Content: strings.TrimRight(b.String(), "\n")}, nil
}

// columnOf finds the symbol on the line and returns its zero-based column.
// Bytes stand in for the UTF-16 units the wire wants, which agree whenever
// the text before the symbol is ASCII; on the rare line where they differ
// the server answers about a nearby position, and the failure mode is a
// miss the model can see, never a wrong location reported as right.
func columnOf(abs string, line int, symbol string) (int, error) {
	data, err := os.ReadFile(abs)
	if err != nil {
		return 0, err
	}
	lines := strings.Split(string(data), "\n")
	if line > len(lines) {
		return 0, fmt.Errorf("%s has %d lines; line %d is past the end", abs, len(lines), line)
	}
	col := strings.Index(lines[line-1], symbol)
	if col < 0 {
		return 0, fmt.Errorf("%q does not appear on line %d; give the symbol exactly as that line writes it", symbol, line)
	}
	return col, nil
}
