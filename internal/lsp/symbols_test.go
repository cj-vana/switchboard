package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDecodeHierarchicalDocumentSymbolsPreservesSortedDepth(t *testing.T) {
	raw := json.RawMessage(`[
  {
    "name":"Later","kind":12,
    "range":{"start":{"line":10,"character":0},"end":{"line":12,"character":0}},
    "selectionRange":{"start":{"line":10,"character":5},"end":{"line":10,"character":10}}
  },
  {
    "name":"First","detail":"struct detail","kind":23,
    "range":{"start":{"line":0,"character":0},"end":{"line":9,"character":0}},
    "selectionRange":{"start":{"line":0,"character":5},"end":{"line":0,"character":10}},
    "children":[
      {
        "name":"Zed","kind":6,
        "range":{"start":{"line":5,"character":0},"end":{"line":6,"character":0}},
        "selectionRange":{"start":{"line":5,"character":5},"end":{"line":5,"character":8}}
      },
      {
        "name":"Alpha","kind":8,"tags":[1],
        "range":{"start":{"line":2,"character":0},"end":{"line":2,"character":10}},
        "selectionRange":{"start":{"line":2,"character":1},"end":{"line":2,"character":6}}
      }
    ]
  }
]`)

	path := filepath.Join(t.TempDir(), "outline.go")
	symbols, truncated, err := decodeDocumentSymbols(raw, path, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || len(symbols) != 3 {
		t.Fatalf("bounded symbols = %d truncated=%v, want 3 and true", len(symbols), truncated)
	}
	wantNames := []string{"First", "Alpha", "Zed"}
	wantDepths := []int{0, 1, 1}
	for i := range wantNames {
		if symbols[i].Name != wantNames[i] || symbols[i].Depth != wantDepths[i] || symbols[i].Path != path {
			t.Fatalf("symbol %d = %+v, want name %s depth %d path %s", i, symbols[i], wantNames[i], wantDepths[i], path)
		}
	}
	if symbols[0].Detail != "struct detail" || symbols[0].Kind != SymbolStruct {
		t.Fatalf("parent metadata = %+v", symbols[0])
	}
	if !symbols[1].Deprecated {
		t.Fatal("deprecated tag was not normalized")
	}
}

func TestDecodeFlatDocumentSymbolsKeepsContainersFlatAndDeterministic(t *testing.T) {
	raw := json.RawMessage(`[
  {
    "name":"Second","kind":12,"containerName":"Outer",
    "location":{"uri":"file:///tmp/a%20file.go","range":{"start":{"line":9,"character":2},"end":{"line":9,"character":8}}}
  },
  {
    "name":"First","kind":13,"containerName":"Outer",
    "location":{"uri":"file:///tmp/a%20file.go","range":{"start":{"line":1,"character":0},"end":{"line":1,"character":5}}}
  }
]`)

	symbols, truncated, err := decodeDocumentSymbols(raw, "/ignored/document/path", 20)
	if err != nil {
		t.Fatal(err)
	}
	if truncated || len(symbols) != 2 {
		t.Fatalf("flat symbols = %d truncated=%v", len(symbols), truncated)
	}
	if symbols[0].Name != "First" || symbols[1].Name != "Second" {
		t.Fatalf("flat deterministic order = %+v", symbols)
	}
	for _, symbol := range symbols {
		if symbol.Depth != 0 || symbol.Container != "Outer" || symbol.Path != filepath.FromSlash("/tmp/a file.go") {
			t.Fatalf("flat symbol incorrectly inferred hierarchy or path: %+v", symbol)
		}
		if symbol.SelectionRange != symbol.Range {
			t.Fatalf("flat selection range = %+v, want location range %+v", symbol.SelectionRange, symbol.Range)
		}
	}
}

func TestDecodeWorkspaceSymbolsSortsDeduplicatesAndTruncates(t *testing.T) {
	raw := json.RawMessage(`[
  {
    "name":"Bee","kind":5,
    "location":{"uri":"file:///tmp/b.go","range":{"start":{"line":3,"character":0},"end":{"line":3,"character":3}}}
  },
  {
    "name":"Aye","kind":12,"containerName":"pkg",
    "location":{"uri":"file:///tmp/a.go","range":{"start":{"line":7,"character":1},"end":{"line":7,"character":4}}}
  },
  {
    "name":"Aye","kind":12,"containerName":"pkg",
    "location":{"uri":"file:///tmp/a.go","range":{"start":{"line":7,"character":1},"end":{"line":7,"character":4}}}
  }
]`)

	symbols, truncated, err := decodeWorkspaceSymbols(raw, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || len(symbols) != 1 || symbols[0].Name != "Aye" || symbols[0].Container != "pkg" {
		t.Fatalf("normalized workspace symbols = %+v truncated=%v", symbols, truncated)
	}
	if symbols[0].Kind.String() != "function" || SymbolKind(99).String() != "unknown(99)" {
		t.Fatalf("symbol kind labels = %q / %q", symbols[0].Kind, SymbolKind(99))
	}
}

func TestSymbolNormalizationRejectsMalformedRangesAndLocations(t *testing.T) {
	tests := []struct {
		name      string
		document  bool
		raw       string
		wantError string
	}{
		{
			name: "document selection outside range", document: true, wantError: "outside",
			raw: `[{
  "name":"bad","kind":12,
  "range":{"start":{"line":2,"character":0},"end":{"line":2,"character":3}},
  "selectionRange":{"start":{"line":3,"character":0},"end":{"line":3,"character":1}}
}]`,
		},
		{
			name: "workspace location has no range", wantError: "no concrete location range",
			raw: `[{"name":"bad","kind":12,"location":{"uri":"file:///tmp/a.go"}}]`,
		},
		{
			name: "workspace URI is not a file", wantError: "not a hierarchical file URI",
			raw: `[{"name":"bad","kind":12,"location":{"uri":"https://example.test/a.go","range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}}}}]`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var err error
			if tt.document {
				_, _, err = decodeDocumentSymbols(json.RawMessage(tt.raw), "/tmp/a.go", 10)
			} else {
				_, _, err = decodeWorkspaceSymbols(json.RawMessage(tt.raw), 10)
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("error = %v, want one containing %q", err, tt.wantError)
			}
		})
	}
}

func TestDocumentSymbolsSynchronizeBeforeRequest(t *testing.T) {
	c, server, root := newScriptedServer(t)
	path := filepath.Join(root, "a.go")
	c.setCapabilities(Capabilities{
		PositionEncoding: PositionEncodingUTF16,
		Sync:             SyncOptions{OpenClose: true, Change: SyncFull},
		DocumentSymbols:  true,
	})
	type result struct {
		symbols   []Symbol
		truncated bool
		err       error
	}
	done := make(chan result, 1)
	go func() {
		symbols, truncated, err := c.DocumentSymbolsBounded(context.Background(), path, 10)
		done <- result{symbols, truncated, err}
	}()

	assertMethod(t, server.recv(t), "textDocument/didOpen")
	request := server.recv(t)
	assertMethod(t, request, "textDocument/documentSymbol")
	if got := frameParams(t, request)["textDocument"].(map[string]any)["uri"]; got != pathToURI(path) {
		t.Fatalf("document symbol URI = %v, want %s", got, pathToURI(path))
	}
	server.send(t, map[string]any{
		"jsonrpc": "2.0", "id": request["id"],
		"result": []map[string]any{{
			"name": "Thing", "kind": 13,
			"range":          map[string]any{"start": map[string]any{"line": 2}, "end": map[string]any{"line": 2, "character": 13}},
			"selectionRange": map[string]any{"start": map[string]any{"line": 2, "character": 4}, "end": map[string]any{"line": 2, "character": 9}},
		}},
	})
	select {
	case got := <-done:
		if got.err != nil || got.truncated || len(got.symbols) != 1 || got.symbols[0].Name != "Thing" {
			t.Fatalf("document symbols result = %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("document symbol request did not complete")
	}
}

func TestWorkspaceSymbolsFlushOpenDiskEditsBeforeSearch(t *testing.T) {
	c, server, root := newScriptedServer(t)
	path := filepath.Join(root, "a.go")
	first := asyncDefinition(c, path)
	server.recv(t) // didOpen
	request := server.recv(t)
	server.send(t, map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": nil})
	if err := awaitTestError(t, first); err != nil {
		t.Fatal(err)
	}

	c.setCapabilities(Capabilities{
		PositionEncoding: PositionEncodingUTF16,
		Sync:             SyncOptions{OpenClose: true, Change: SyncFull},
		WorkspaceSymbols: true,
	})
	if err := os.WriteFile(path, []byte("package a\nvar Fresh = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	type result struct {
		symbols []Symbol
		err     error
	}
	done := make(chan result, 1)
	go func() {
		symbols, _, err := c.WorkspaceSymbols(context.Background(), "Fresh", 10)
		done <- result{symbols, err}
	}()

	change := server.recv(t)
	assertMethod(t, change, "textDocument/didChange")
	search := server.recv(t)
	assertMethod(t, search, "workspace/symbol")
	if got := frameParams(t, search)["query"]; got != "Fresh" {
		t.Fatalf("workspace query = %v, want Fresh", got)
	}
	server.send(t, map[string]any{
		"jsonrpc": "2.0", "id": search["id"],
		"result": []map[string]any{{
			"name": "Fresh", "kind": 13,
			"location": map[string]any{
				"uri":   pathToURI(path),
				"range": map[string]any{"start": map[string]any{"line": 1, "character": 4}, "end": map[string]any{"line": 1, "character": 9}},
			},
		}},
	})
	select {
	case got := <-done:
		if got.err != nil || len(got.symbols) != 1 || got.symbols[0].Name != "Fresh" {
			t.Fatalf("workspace symbol result = %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("workspace symbol request did not complete")
	}
}

func TestServerProblemsStoreIsStableBeforeStartupAndUnavailableAfterClose(t *testing.T) {
	server := &Server{Root: t.TempDir()}
	first, second := server.Problems(), server.Problems()
	if first != second {
		t.Fatal("Server.Problems returned different stores")
	}
	server.Close()
	if first.Snapshot(ProblemFilter{}).Available {
		t.Fatal("never-started server left its diagnostics store available after Close")
	}
}

func TestPreCanceledServerCallDoesNotConsumeLazyStart(t *testing.T) {
	root := t.TempDir()
	server := &Server{Argv: []string{filepath.Join(root, "missing-language-server")}, Root: root}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := server.Capabilities(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled capabilities error = %v", err)
	}
	if _, err := server.Capabilities(context.Background()); err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("second capabilities error = %v, want an actual startup attempt", err)
	}
	server.Close()
}

func TestCanceledStartupAttemptCanRetrySuccessfully(t *testing.T) {
	root := t.TempDir()
	var attempts int
	server := &Server{Root: root}
	server.startClient = func(ctx context.Context, _ []string, _ string, store *ProblemStore) (*Client, error) {
		attempts++
		if attempts == 1 {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return testStartedClient(store, "recovered"), nil
	}

	first, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := server.Capabilities(first); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first startup error = %v, want deadline", err)
	}
	capabilities, err := server.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("retry startup: %v", err)
	}
	if attempts != 2 || capabilities.ServerName != "recovered" {
		t.Fatalf("retry attempts=%d capabilities=%+v", attempts, capabilities)
	}
	if !server.Problems().Snapshot(ProblemFilter{}).Available {
		t.Fatal("successful retry did not restore runtime availability")
	}
	server.Close()
}

func TestServerOpenCloseSyncIsAnExplicitProfileOverride(t *testing.T) {
	server := &Server{Root: t.TempDir(), OpenCloseSync: true}
	server.startClient = func(_ context.Context, _ []string, _ string, store *ProblemStore) (*Client, error) {
		client := testStartedClient(store, "legacy-sync")
		client.capabilities.Sync.Change = SyncIncremental
		return client, nil
	}

	capabilities, err := server.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !capabilities.Sync.OpenClose || capabilities.Sync.Change != SyncIncremental {
		t.Fatalf("profiled synchronization = %+v", capabilities.Sync)
	}
	server.Close()
}

func TestConcurrentServerCallsShareOneStartupAttempt(t *testing.T) {
	root := t.TempDir()
	server := &Server{Root: root}
	var attempts atomic.Int32
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	server.startClient = func(_ context.Context, _ []string, _ string, store *ProblemStore) (*Client, error) {
		attempts.Add(1)
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return testStartedClient(store, "shared"), nil
	}

	const callers = 20
	var wait sync.WaitGroup
	errorsFound := make(chan error, callers)
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			capabilities, err := server.Capabilities(context.Background())
			if err == nil && capabilities.ServerName != "shared" {
				err = fmt.Errorf("server name = %q", capabilities.ServerName)
			}
			errorsFound <- err
		}()
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("startup attempt did not begin")
	}
	close(release)
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("concurrent calls started %d runtimes, want 1", got)
	}
	server.Close()
}

func TestClosePublishesFailureToEverySharedStartupCaller(t *testing.T) {
	root := t.TempDir()
	server := &Server{Root: root}
	started := make(chan struct{})
	release := make(chan struct{})
	server.startClient = func(_ context.Context, _ []string, _ string, store *ProblemStore) (*Client, error) {
		close(started)
		<-release // Deliberately ignore cancellation and finish after Server.Close.
		return testStartedClient(store, "discarded"), nil
	}

	first := make(chan error, 1)
	go func() {
		_, err := server.Capabilities(context.Background())
		first <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("startup attempt did not begin")
	}

	waiting := make(chan struct{})
	second := make(chan error, 1)
	go func() {
		_, err := server.Capabilities(&waiterContext{waiting: waiting})
		second <- err
	}()
	select {
	case <-waiting: // Done is consulted only once the second caller is waiting on the shared attempt.
	case <-time.After(time.Second):
		t.Fatal("second caller did not join the shared startup attempt")
	}

	server.Close()
	close(release)
	for index, result := range []<-chan error{first, second} {
		select {
		case err := <-result:
			if err == nil || !strings.Contains(err.Error(), "language server is closed") {
				t.Fatalf("caller %d error = %v, want the published closed error", index+1, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("caller %d remained blocked after Close", index+1)
		}
	}
}

type waiterContext struct {
	waiting chan struct{}
	once    sync.Once
}

func (c *waiterContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *waiterContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return nil
}
func (c *waiterContext) Err() error    { return nil }
func (c *waiterContext) Value(any) any { return nil }

func testStartedClient(store *ProblemStore, name string) *Client {
	return &Client{
		in: &failFirstWriteCloser{}, pending: map[int64]chan *response{},
		documents: map[string]*documentState{}, problems: store,
		capabilities: Capabilities{ServerName: name, PositionEncoding: PositionEncodingUTF16},
		closeGrace:   time.Millisecond,
	}
}

func TestSymbolToolRenderingIsBoundedAndNavigable(t *testing.T) {
	root := t.TempDir()
	resolver := testSymbolResolver{root: root}
	path := filepath.Join(root, "a.go")
	emojiPosition, err := symbolPosition([]byte("header\n😀Outer\n"), 2, "Outer", 1)
	if err != nil {
		t.Fatal(err)
	}
	symbols := []Symbol{{
		Name: "Outer", Kind: SymbolStruct, Path: path,
		// emojiPosition.Character is zero-based UTF-16 column 2, while the
		// declaration's one-based rune column is 2; neither unit is presented
		// as interchangeable.
		SelectionRange: Range{Start: emojiPosition},
	}, {
		Name: "Method", Detail: "() error", Kind: SymbolMethod, Path: path, Depth: 1,
		SelectionRange: Range{Start: Position{Line: 3, Character: 4}},
	}}
	outline := renderOutline(symbols, true, resolver)
	for _, want := range []string{"struct Outer — a.go:2", "  method Method — () error — a.go:4", "outline truncated"} {
		if !strings.Contains(outline, want) {
			t.Errorf("outline %q missing %q", outline, want)
		}
	}
	if strings.Contains(outline, "a.go:2:3") {
		t.Fatalf("outline exposed UTF-16 character as a human column: %q", outline)
	}

	symbols[0].Container = "pkg"
	workspace := renderWorkspaceSymbols(symbols[:1], true, resolver)
	for _, want := range []string{"struct Outer in pkg — a.go:2", "results truncated"} {
		if !strings.Contains(workspace, want) {
			t.Errorf("workspace symbols %q missing %q", workspace, want)
		}
	}
	if strings.Contains(workspace, "a.go:2:3") {
		t.Fatalf("workspace symbols exposed UTF-16 character as a human column: %q", workspace)
	}
}

type testSymbolResolver struct{ root string }

func (r testSymbolResolver) Resolve(path string) (string, error) {
	return filepath.Join(r.root, path), nil
}

func (r testSymbolResolver) Display(path string) string {
	relative, err := filepath.Rel(r.root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(relative)
}
