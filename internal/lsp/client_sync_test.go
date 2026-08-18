package lsp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestInitializeStoresDecodedCapabilities(t *testing.T) {
	c, server, _ := newScriptedServer(t)
	done := make(chan error, 1)
	go func() { done <- c.initialize(context.Background()) }()

	request := server.recv(t)
	assertMethod(t, request, "initialize")
	params := frameParams(t, request)
	capabilities := params["capabilities"].(map[string]any)
	general := capabilities["general"].(map[string]any)
	encodings := general["positionEncodings"].([]any)
	if len(encodings) != 1 || encodings[0] != "utf-16" {
		t.Fatalf("advertised position encodings = %#v, want UTF-16 only", encodings)
	}

	server.send(t, map[string]any{
		"jsonrpc": "2.0", "id": request["id"],
		"result": map[string]any{
			"serverInfo": map[string]any{"name": "fixture", "version": "9"},
			"capabilities": map[string]any{
				"positionEncoding":       "utf-16",
				"textDocumentSync":       map[string]any{"openClose": true, "change": 2, "save": true},
				"definitionProvider":     true,
				"referencesProvider":     false,
				"documentSymbolProvider": map[string]any{},
				"workspaceSymbolProvider": map[string]any{
					"resolveProvider": false,
				},
			},
		},
	})
	initialized := server.recv(t)
	assertMethod(t, initialized, "initialized")
	if err := awaitTestError(t, done); err != nil {
		t.Fatal(err)
	}

	got := c.Capabilities()
	if got.ServerName != "fixture" || got.ServerVersion != "9" || !got.Definition || got.References || !got.DocumentSymbols || !got.WorkspaceSymbols {
		t.Fatalf("stored capabilities = %#v", got)
	}
	if got.Sync != (SyncOptions{OpenClose: true, Change: SyncIncremental, Save: true}) {
		t.Fatalf("stored sync options = %#v", got.Sync)
	}
}

func TestDefinitionAtSymbolSynchronizesTheExactUTF16Snapshot(t *testing.T) {
	c, server, root := newScriptedServer(t)
	path := filepath.Join(root, "a.go")
	content := "package a\n\nvar 😀Thing = 1\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := c.DefinitionAtSymbol(context.Background(), path, 3, "Thing")
		done <- err
	}()

	open := server.recv(t)
	assertMethod(t, open, "textDocument/didOpen")
	openDocument := frameParams(t, open)["textDocument"].(map[string]any)
	if got := openDocument["text"]; got != content {
		t.Fatalf("didOpen text = %q, want exact disk snapshot %q", got, content)
	}
	request := server.recv(t)
	assertMethod(t, request, "textDocument/definition")
	position := frameParams(t, request)["position"].(map[string]any)
	if position["line"] != float64(2) || position["character"] != float64(6) {
		t.Fatalf("wire position = %#v, want line 2, UTF-16 character 6", position)
	}
	server.send(t, map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": nil})
	if err := awaitTestError(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestDiskEditsSendFullChangesSavesAndMonotonicVersions(t *testing.T) {
	c, server, root := newScriptedServer(t)
	path := filepath.Join(root, "a.go")
	c.setCapabilities(Capabilities{
		PositionEncoding: PositionEncodingUTF16,
		Sync: SyncOptions{
			OpenClose: true, Change: SyncFull, Save: true, SaveIncludeText: true,
		},
		Definition: true,
	})

	query := func() chan error {
		done := make(chan error, 1)
		go func() {
			_, err := c.Definition(context.Background(), path, 1, 0)
			done <- err
		}()
		return done
	}

	done := query()
	open := server.recv(t)
	assertMethod(t, open, "textDocument/didOpen")
	if got := frameParams(t, open)["textDocument"].(map[string]any)["version"]; got != float64(1) {
		t.Fatalf("didOpen version = %v, want 1", got)
	}
	request := server.recv(t)
	assertMethod(t, request, "textDocument/definition")
	server.send(t, map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": nil})
	if err := awaitTestError(t, done); err != nil {
		t.Fatal(err)
	}

	for version, content := range []string{"package a\nvar One = 1\n", "package a\nvar Two = 2\n"} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		done = query()
		change := server.recv(t)
		assertMethod(t, change, "textDocument/didChange")
		params := frameParams(t, change)
		wireVersion := params["textDocument"].(map[string]any)["version"]
		if wireVersion != float64(version+2) {
			t.Fatalf("didChange version = %v, want %d", wireVersion, version+2)
		}
		changes := params["contentChanges"].([]any)
		changeItem := changes[0].(map[string]any)
		if changeItem["text"] != content {
			t.Fatalf("didChange text = %q, want %q", changeItem["text"], content)
		}
		if _, incremental := changeItem["range"]; incremental {
			t.Fatal("full synchronization must not send an incremental range")
		}

		save := server.recv(t)
		assertMethod(t, save, "textDocument/didSave")
		if got := frameParams(t, save)["text"]; got != content {
			t.Fatalf("didSave text = %q, want %q", got, content)
		}
		request = server.recv(t)
		assertMethod(t, request, "textDocument/definition")
		server.send(t, map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": nil})
		if err := awaitTestError(t, done); err != nil {
			t.Fatal(err)
		}
	}

	// A query over unchanged bytes emits only its request: no redundant
	// didChange or didSave is allowed to consume another version.
	done = query()
	request = server.recv(t)
	assertMethod(t, request, "textDocument/definition")
	server.send(t, map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": nil})
	if err := awaitTestError(t, done); err != nil {
		t.Fatal(err)
	}
	c.documentsMu.Lock()
	version := c.documents[path].version
	c.documentsMu.Unlock()
	if version != 3 {
		t.Fatalf("unchanged query advanced document version to %d, want 3", version)
	}
}

func TestIncrementalSyncReplacesTheWholeOldUTF16Range(t *testing.T) {
	c, server, root := newScriptedServer(t)
	path := filepath.Join(root, "a.go")
	if err := os.WriteFile(path, []byte("a😀"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.setCapabilities(Capabilities{
		PositionEncoding: PositionEncodingUTF16,
		Sync:             SyncOptions{OpenClose: true, Change: SyncIncremental},
		Definition:       true,
	})

	first := asyncDefinition(c, path)
	server.recv(t) // didOpen
	request := server.recv(t)
	server.send(t, map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": nil})
	if err := awaitTestError(t, first); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte("replacement"), 0o644); err != nil {
		t.Fatal(err)
	}
	second := asyncDefinition(c, path)
	change := server.recv(t)
	assertMethod(t, change, "textDocument/didChange")
	item := frameParams(t, change)["contentChanges"].([]any)[0].(map[string]any)
	wireRange := item["range"].(map[string]any)
	start := wireRange["start"].(map[string]any)
	end := wireRange["end"].(map[string]any)
	if start["line"] != float64(0) || start["character"] != float64(0) || end["line"] != float64(0) || end["character"] != float64(3) {
		t.Fatalf("incremental replacement range = %#v, want 0:0 through UTF-16 0:3", wireRange)
	}
	request = server.recv(t)
	server.send(t, map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": nil})
	if err := awaitTestError(t, second); err != nil {
		t.Fatal(err)
	}
}

func TestDidOpenStateIsCommittedOnlyAfterSuccessfulNotification(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.go")
	writer := &failFirstWriteCloser{fail: true}
	c := &Client{
		in: writer, pending: map[int64]chan *response{}, documents: map[string]*documentState{},
		problems: NewProblemStore(root), root: root,
	}
	capabilities := Capabilities{Sync: SyncOptions{OpenClose: true, Change: SyncFull}}
	data := []byte("package a\n")
	if err := c.syncDocumentLocked(path, data, capabilities); err == nil {
		t.Fatal("first didOpen unexpectedly succeeded")
	}
	if c.documents[path] != nil {
		t.Fatalf("failed didOpen committed state: %+v", c.documents[path])
	}
	if err := c.syncDocumentLocked(path, data, capabilities); err != nil {
		t.Fatalf("retrying didOpen: %v", err)
	}
	if got := c.documents[path]; got == nil || !got.open || got.version != 1 || !bytes.Equal(got.text, data) {
		t.Fatalf("retried didOpen state = %+v", got)
	}
}

func TestDeletingAndRecreatingAnOpenFileClosesThenReopens(t *testing.T) {
	c, server, root := newScriptedServer(t)
	path := filepath.Join(root, "a.go")

	first := asyncDefinition(c, path)
	server.recv(t) // didOpen
	request := server.recv(t)
	server.send(t, map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": nil})
	if err := awaitTestError(t, first); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	deleted := asyncDefinition(c, path)
	closeFrame := server.recv(t)
	assertMethod(t, closeFrame, "textDocument/didClose")
	if err := awaitTestError(t, deleted); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("query after delete error = %v, want not-exist", err)
	}
	c.documentsMu.Lock()
	_, retained := c.documents[path]
	c.documentsMu.Unlock()
	if retained {
		t.Fatal("deleted document remained in synchronization state")
	}

	if err := os.WriteFile(path, []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	recreated := asyncDefinition(c, path)
	open := server.recv(t)
	assertMethod(t, open, "textDocument/didOpen")
	if got := frameParams(t, open)["textDocument"].(map[string]any)["version"]; got != float64(1) {
		t.Fatalf("recreated didOpen version = %v, want 1", got)
	}
	request = server.recv(t)
	server.send(t, map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": nil})
	if err := awaitTestError(t, recreated); err != nil {
		t.Fatal(err)
	}
}

func TestCanceledCallSendsCancelRequest(t *testing.T) {
	c, server, root := newScriptedServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.Definition(ctx, filepath.Join(root, "a.go"), 1, 0)
		done <- err
	}()
	server.recv(t) // didOpen
	request := server.recv(t)
	cancel()
	cancellation := server.recv(t)
	assertMethod(t, cancellation, "$/cancelRequest")
	if got := frameParams(t, cancellation)["id"]; got != request["id"] {
		t.Fatalf("cancel id = %v, want request id %v", got, request["id"])
	}
	if err := awaitTestError(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled call error = %v", err)
	}
}

func TestPreCanceledCallDoesNotReadOpenOrRequest(t *testing.T) {
	c, _, root := newScriptedServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Definition(ctx, filepath.Join(root, "a.go"), 1, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled call error = %v", err)
	}
	c.documentsMu.Lock()
	documents := len(c.documents)
	c.documentsMu.Unlock()
	c.mu.Lock()
	pending, nextID := len(c.pending), c.nextID
	c.mu.Unlock()
	if documents != 0 || pending != 0 || nextID != 0 {
		t.Fatalf("pre-canceled call mutated state: documents=%d pending=%d nextID=%d", documents, pending, nextID)
	}
}

func TestCloseSendsDocumentCloseBeforeShutdownAndExit(t *testing.T) {
	c, server, root := newScriptedServer(t)
	path := filepath.Join(root, "a.go")
	query := asyncDefinition(c, path)
	server.recv(t) // didOpen
	request := server.recv(t)
	server.send(t, map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": nil})
	if err := awaitTestError(t, query); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		c.Close()
		close(done)
	}()
	assertMethod(t, server.recv(t), "textDocument/didClose")
	shutdown := server.recv(t)
	assertMethod(t, shutdown, "shutdown")
	server.send(t, map[string]any{"jsonrpc": "2.0", "id": shutdown["id"], "result": nil})
	assertMethod(t, server.recv(t), "exit")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not complete")
	}
	if c.Problems().Snapshot(ProblemFilter{}).Available {
		t.Fatal("diagnostics store remained available after shutdown")
	}
}

func TestCloseBreaksAWriterBlockedBehindAnUnresponsiveServer(t *testing.T) {
	root := t.TempDir()
	writer := newBlockingWriteCloser()
	c := &Client{
		in: writer, pending: map[int64]chan *response{}, documents: map[string]*documentState{},
		problems: NewProblemStore(root), root: root, closeGrace: 20 * time.Millisecond,
		capabilities: Capabilities{PositionEncoding: PositionEncodingUTF16},
	}
	writeDone := make(chan error, 1)
	go func() { writeDone <- c.notify("fixture/blocked", nil) }()
	select {
	case <-writer.started:
	case <-time.After(time.Second):
		t.Fatal("fixture write never blocked")
	}

	closed := make(chan struct{})
	go func() {
		c.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close remained blocked behind the server writer")
	}
	select {
	case err := <-writeDone:
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("blocked write error = %v, want closed pipe", err)
		}
	case <-time.After(time.Second):
		t.Fatal("closing stdin did not release the blocked writer")
	}
}

func TestUnsupportedCapabilityDoesNotOpenOrRequest(t *testing.T) {
	c, _, root := newScriptedServer(t)
	c.setCapabilities(Capabilities{PositionEncoding: PositionEncodingUTF16})
	_, err := c.Definition(context.Background(), filepath.Join(root, "a.go"), 1, 0)
	var unsupported *UnsupportedCapabilityError
	if !errors.As(err, &unsupported) || unsupported.Feature != FeatureDefinition {
		t.Fatalf("error = %v, want UnsupportedCapabilityError for definition", err)
	}
	c.documentsMu.Lock()
	documents := len(c.documents)
	c.documentsMu.Unlock()
	c.mu.Lock()
	pending := len(c.pending)
	c.mu.Unlock()
	if documents != 0 || pending != 0 {
		t.Fatalf("unsupported request mutated state: documents=%d pending=%d", documents, pending)
	}
}

func TestDefinitionLocationLinkUsesSelectionRange(t *testing.T) {
	raw := json.RawMessage(`[{
  "targetUri":"file:///tmp/lib%20one.go",
  "targetRange":{"start":{"line":3,"character":1},"end":{"line":8,"character":0}},
  "targetSelectionRange":{"start":{"line":5,"character":7},"end":{"line":5,"character":10}}
}]`)
	locations, err := decodeLocations("textDocument/definition", raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(locations) != 1 || locations[0].Path != filepath.FromSlash("/tmp/lib one.go") || locations[0].Line != 6 || locations[0].Character != 8 {
		t.Fatalf("LocationLink decoded as %+v", locations)
	}
}

func TestDefinitionLocationsRejectMissingOrInvalidRanges(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		part string
	}{
		{name: "location missing range", raw: `{"uri":"file:///tmp/a.go"}`, part: "omitted range"},
		{name: "location negative", raw: `{"uri":"file:///tmp/a.go","range":{"start":{"line":-1,"character":0},"end":{"line":0,"character":0}}}`, part: "non-negative"},
		{name: "location backwards", raw: `{"uri":"file:///tmp/a.go","range":{"start":{"line":2,"character":0},"end":{"line":1,"character":0}}}`, part: "precedes"},
		{name: "link missing selection", raw: `{"targetUri":"file:///tmp/a.go","targetRange":{"start":{"line":0,"character":0},"end":{"line":1,"character":0}}}`, part: "omitted targetRange"},
		{name: "link selection outside", raw: `{"targetUri":"file:///tmp/a.go","targetRange":{"start":{"line":1,"character":0},"end":{"line":1,"character":5}},"targetSelectionRange":{"start":{"line":2,"character":0},"end":{"line":2,"character":1}}}`, part: "outside"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeLocations("textDocument/definition", json.RawMessage(tt.raw))
			if err == nil || !strings.Contains(err.Error(), tt.part) {
				t.Fatalf("error = %v, want one containing %q", err, tt.part)
			}
		})
	}

	// A legitimate selection at 0:0 must not be mistaken for an omitted
	// zero-value range.
	zero := json.RawMessage(`{
  "targetUri":"file:///tmp/a.go",
  "targetRange":{"start":{"line":0,"character":0},"end":{"line":1,"character":0}},
  "targetSelectionRange":{"start":{"line":0,"character":0},"end":{"line":0,"character":0}}
}`)
	locations, err := decodeLocations("textDocument/definition", zero)
	if err != nil || len(locations) != 1 || locations[0].Line != 1 || locations[0].Character != 1 {
		t.Fatalf("zero selection location = %+v, error %v", locations, err)
	}
}

func TestServerRequestsUseProtocolShapedReplies(t *testing.T) {
	_, server, root := newScriptedServer(t)
	server.send(t, map[string]any{
		"jsonrpc": "2.0", "id": 71, "method": "workspace/workspaceFolders", "params": nil,
	})
	reply := server.recv(t)
	folders, ok := reply["result"].([]any)
	if !ok || len(folders) != 1 {
		t.Fatalf("workspace folder reply = %#v", reply)
	}
	folder := folders[0].(map[string]any)
	if folder["uri"] != pathToURI(root) || folder["name"] != filepath.Base(root) {
		t.Fatalf("workspace folder = %#v", folder)
	}

	server.send(t, map[string]any{
		"jsonrpc": "2.0", "id": 72, "method": "fixture/unsupported", "params": map[string]any{},
	})
	reply = server.recv(t)
	wireError, ok := reply["error"].(map[string]any)
	if !ok || wireError["code"] != float64(-32601) {
		t.Fatalf("unknown request reply = %#v, want MethodNotFound", reply)
	}
}

func TestPublishDiagnosticsTracksSyncedVersionsAndDiskInvalidation(t *testing.T) {
	c, server, root := newScriptedServer(t)
	path := filepath.Join(root, "a.go")
	uri := pathToURI(path)

	first := asyncDefinition(c, path)
	server.recv(t) // didOpen version 1
	request := server.recv(t)
	server.send(t, map[string]any{
		"jsonrpc": "2.0", "method": "textDocument/publishDiagnostics",
		"params": map[string]any{
			"uri": uri, "version": 1,
			"diagnostics": []map[string]any{{
				"range": map[string]any{
					"start": map[string]any{"line": 2, "character": 4},
					"end":   map[string]any{"line": 2, "character": 9},
				},
				"severity": 1, "code": 27, "source": "fixture", "message": "first",
			}},
		},
	})
	server.send(t, map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": nil})
	if err := awaitTestError(t, first); err != nil {
		t.Fatal(err)
	}
	fresh := waitProblemSnapshot(t, c.Problems(), func(snapshot ProblemSnapshot) bool {
		return snapshot.Total == 1 && snapshot.Documents[0].Freshness == Fresh
	})
	problem := fresh.Documents[0].Problems[0]
	if problem.Line != 3 || problem.Column != 5 || problem.EndLine != 3 || problem.EndColumn != 10 || problem.Code != "27" || problem.Source != "fixture" {
		t.Fatalf("published diagnostic = %+v", problem)
	}

	if err := os.WriteFile(path, []byte("package a\nvar Changed = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second := asyncDefinition(c, path)
	change := server.recv(t)
	assertMethod(t, change, "textDocument/didChange")
	request = server.recv(t)
	assertMethod(t, request, "textDocument/definition")
	pending := waitProblemSnapshot(t, c.Problems(), func(snapshot ProblemSnapshot) bool {
		return snapshot.Total == 1 && snapshot.Documents[0].Freshness == Pending
	})
	if pending.Documents[0].CurrentVersion == nil || *pending.Documents[0].CurrentVersion != 2 {
		t.Fatalf("pending document version = %+v", pending.Documents[0])
	}

	server.send(t, map[string]any{
		"jsonrpc": "2.0", "method": "textDocument/publishDiagnostics",
		"params": map[string]any{
			"uri": uri, "version": 2,
			"diagnostics": []map[string]any{{
				"range":    map[string]any{"start": map[string]any{"line": 1}, "end": map[string]any{"line": 1, "character": 1}},
				"severity": 2, "message": "replacement",
			}},
		},
	})
	server.send(t, map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": nil})
	if err := awaitTestError(t, second); err != nil {
		t.Fatal(err)
	}
	replacement := waitProblemSnapshot(t, c.Problems(), func(snapshot ProblemSnapshot) bool {
		return snapshot.Total == 1 && snapshot.Documents[0].Freshness == Fresh && snapshot.Documents[0].Problems[0].Message == "replacement"
	})
	if replacement.Documents[0].Version == nil || *replacement.Documents[0].Version != 2 {
		t.Fatalf("replacement document version = %+v", replacement.Documents[0])
	}
}

type failFirstWriteCloser struct {
	bytes.Buffer
	fail bool
}

func (w *failFirstWriteCloser) Write(p []byte) (int, error) {
	if w.fail {
		w.fail = false
		return 0, fmt.Errorf("injected write failure")
	}
	return w.Buffer.Write(p)
}

func (w *failFirstWriteCloser) Close() error { return nil }

type blockingWriteCloser struct {
	started chan struct{}
	closed  chan struct{}
	start   sync.Once
	close   sync.Once
}

func newBlockingWriteCloser() *blockingWriteCloser {
	return &blockingWriteCloser{started: make(chan struct{}), closed: make(chan struct{})}
}

func (w *blockingWriteCloser) Write([]byte) (int, error) {
	w.start.Do(func() { close(w.started) })
	<-w.closed
	return 0, io.ErrClosedPipe
}

func (w *blockingWriteCloser) Close() error {
	w.close.Do(func() { close(w.closed) })
	return nil
}

func asyncDefinition(c *Client, path string) chan error {
	done := make(chan error, 1)
	go func() {
		_, err := c.Definition(context.Background(), path, 1, 0)
		done <- err
	}()
	return done
}

func frameParams(t *testing.T, frame map[string]any) map[string]any {
	t.Helper()
	params, ok := frame["params"].(map[string]any)
	if !ok {
		t.Fatalf("frame params = %#v, want object", frame["params"])
	}
	return params
}

func assertMethod(t *testing.T, frame map[string]any, want string) {
	t.Helper()
	if got := frame["method"]; got != want {
		t.Fatalf("method = %v, want %s: %#v", got, want, frame)
	}
}

func awaitTestError(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("operation did not complete")
		return nil
	}
}

func waitProblemSnapshot(t *testing.T, store *ProblemStore, ready func(ProblemSnapshot) bool) ProblemSnapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		snapshot := store.Snapshot(ProblemFilter{})
		if ready(snapshot) {
			return snapshot
		}
		if time.Now().After(deadline) {
			t.Fatalf("diagnostics state did not converge: %+v", snapshot)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestDiagnosticCodeRejectsUnsupportedJSONShapes(t *testing.T) {
	for _, raw := range []string{"null", "true", `{}`, `[]`} {
		if got := diagnosticCode(json.RawMessage(raw)); got != "" {
			t.Errorf("diagnosticCode(%s) = %q, want empty", strings.TrimSpace(raw), got)
		}
	}
}

func TestMalformedPublishedDiagnosticRecordsProtocolIssueAtomically(t *testing.T) {
	root := t.TempDir()
	c := &Client{documents: map[string]*documentState{}, problems: NewProblemStore(root), root: root}
	c.publishDiagnostics(json.RawMessage(`{
  "uri":"file:///tmp/a.go",
  "diagnostics":[{"message":"missing range"}]
}`))
	snapshot := c.Problems().Snapshot(ProblemFilter{})
	if snapshot.ProtocolIssues != 1 || snapshot.Total != 0 || !strings.Contains(snapshot.LastProtocolIssue, "omitted range") {
		t.Fatalf("malformed diagnostic snapshot = %+v", snapshot)
	}
}
