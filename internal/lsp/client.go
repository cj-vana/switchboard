// Package lsp speaks the Language Server Protocol to one server over stdio,
// narrowly: initialize, open a document, ask where a symbol is defined and
// where it is referenced. A language server answers those in milliseconds
// from a live syntax model, where the search tools answer with candidates —
// precision the model would otherwise spend tool rounds approximating.
//
// The narrowness is deliberate. Diagnostics, completion, formatting, and
// the rest of the protocol serve an editor's cursor, not an agent's
// questions, and every additional capability is surface the client must
// keep honest against real servers. What ships is what was verified.
package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Client is one running server. Methods are safe to call concurrently;
// responses are routed to callers by request id.
type Client struct {
	writeMu sync.Mutex
	in      io.WriteCloser

	mu      sync.Mutex
	pending map[int64]chan *response
	nextID  int64
	opened  map[string]bool
	closed  bool

	cmd  *exec.Cmd
	root string
}

type response struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("server error %d: %s", e.Code, e.Message) }

// Start spawns the server and completes the initialize handshake. The
// context bounds the handshake only; the server then lives until Close.
func Start(ctx context.Context, argv []string, root string) (*Client, error) {
	cmd := exec.CommandContext(context.Background(), argv[0], argv[1:]...)
	cmd.Dir = root
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// Stderr is the server's diagnostic chatter, and it must be drained:
	// a full pipe would block the server mid-answer.
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w", argv[0], err)
	}

	c := newClient(stdin, stdout, root)
	c.cmd = cmd
	if err := c.initialize(ctx); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// newClient wires the protocol over any pipe pair, which is what lets the
// tests speak to a scripted server without a subprocess.
func newClient(in io.WriteCloser, out io.Reader, root string) *Client {
	c := &Client{
		in:      in,
		pending: map[int64]chan *response{},
		opened:  map[string]bool{},
		root:    root,
	}
	go c.read(out)
	return c
}

func (c *Client) initialize(ctx context.Context) error {
	uri := pathToURI(c.root)
	var result json.RawMessage
	err := c.call(ctx, "initialize", map[string]any{
		"processId": os.Getpid(),
		"rootUri":   uri,
		"workspaceFolders": []map[string]any{
			{"uri": uri, "name": filepath.Base(c.root)},
		},
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"definition": map[string]any{},
				"references": map[string]any{},
			},
		},
	}, &result)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	return c.notify("initialized", map[string]any{})
}

// read is the single consumer of the server's stdout. Responses route to
// their callers; server-initiated requests are answered with null, because
// this client offers no configuration and no UI; notifications are
// dropped. A server that cannot live with those answers is a server this
// narrow client should not be speaking to, and the failure will be legible
// at the call that never completes.
func (c *Client) read(out io.Reader) {
	r := bufio.NewReader(out)
	for {
		msg, err := readMessage(r)
		if err != nil {
			c.failAll(err)
			return
		}
		var frame struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *rpcError       `json:"error"`
		}
		if err := json.Unmarshal(msg, &frame); err != nil {
			continue
		}
		switch {
		case frame.Method != "" && frame.ID != nil:
			// A server request. Reply null so the server is never left
			// waiting on a client that has no answer to give.
			c.reply(frame.ID)
		case frame.Method != "":
			// A notification; nothing here consumes them.
		case frame.ID != nil:
			var id int64
			if err := json.Unmarshal(frame.ID, &id); err != nil {
				continue
			}
			c.mu.Lock()
			ch := c.pending[id]
			delete(c.pending, id)
			c.mu.Unlock()
			if ch != nil {
				ch <- &response{Result: frame.Result, Error: frame.Error}
			}
		}
	}
}

func (c *Client) failAll(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for id, ch := range c.pending {
		delete(c.pending, id)
		ch <- &response{Error: &rpcError{Message: fmt.Sprintf("server stream ended: %v", err)}}
	}
}

func (c *Client) reply(id json.RawMessage) {
	payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": nil})
	c.write(payload)
}

func (c *Client) call(ctx context.Context, method string, params any, result any) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("the language server is gone")
	}
	c.nextID++
	id := c.nextID
	ch := make(chan *response, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return err
	}
	if err := c.write(payload); err != nil {
		return err
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return resp.Error
		}
		if result != nil && resp.Result != nil {
			return json.Unmarshal(resp.Result, result)
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	}
}

func (c *Client) notify(method string, params any) error {
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	if err != nil {
		return err
	}
	return c.write(payload)
}

func (c *Client) write(payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := fmt.Fprintf(c.in, "Content-Length: %d\r\n\r\n", len(payload)); err != nil {
		return err
	}
	_, err := c.in.Write(payload)
	return err
}

func readMessage(r *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if v, ok := strings.CutPrefix(line, "Content-Length: "); ok {
			length, err = strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return nil, fmt.Errorf("bad Content-Length %q", v)
			}
		}
	}
	if length < 0 {
		return nil, fmt.Errorf("message without Content-Length")
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// ensureOpen sends the file's current bytes once per session. The server
// answers position queries against what was opened, so open-from-disk at
// query time is what keeps answers about the code as it is now.
func (c *Client) ensureOpen(path string) error {
	c.mu.Lock()
	already := c.opened[path]
	if !already {
		c.opened[path] = true
	}
	c.mu.Unlock()
	if already {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return c.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{
			"uri":        pathToURI(path),
			"languageId": languageOf(path),
			"version":    1,
			"text":       string(data),
		},
	})
}

// Location is one answer: a file and a 1-based line.
type Location struct {
	Path string
	Line int
}

type wireLocation struct {
	URI   string `json:"uri"`
	Range struct {
		Start struct {
			Line int `json:"line"`
		} `json:"start"`
	} `json:"range"`
}

// Definition asks where the symbol at a position is defined.
func (c *Client) Definition(ctx context.Context, path string, line, character int) ([]Location, error) {
	return c.locate(ctx, "textDocument/definition", path, line, character, nil)
}

// References asks where the symbol at a position is used, declaration
// included, because the model asking usually wants the whole picture.
func (c *Client) References(ctx context.Context, path string, line, character int) ([]Location, error) {
	return c.locate(ctx, "textDocument/references", path, line, character,
		map[string]any{"includeDeclaration": true})
}

func (c *Client) locate(ctx context.Context, method, path string, line, character int, extra map[string]any) ([]Location, error) {
	if err := c.ensureOpen(path); err != nil {
		return nil, err
	}
	params := map[string]any{
		"textDocument": map[string]any{"uri": pathToURI(path)},
		// The wire is zero-based; every path through this package speaks
		// 1-based lines, because that is what file:line means everywhere
		// else in the product.
		"position": map[string]any{"line": line - 1, "character": character},
	}
	if extra != nil {
		params["context"] = extra
	}

	var raw json.RawMessage
	if err := c.call(ctx, method, params, &raw); err != nil {
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	// The wire allows Location, []Location, and []LocationLink; parsing the
	// array shapes covers every server this client speaks to, and a single
	// object is wrapped to join them.
	var wire []wireLocation
	if err := json.Unmarshal(raw, &wire); err != nil {
		var one wireLocation
		if err := json.Unmarshal(raw, &one); err != nil {
			var links []struct {
				Target string `json:"targetUri"`
				Range  struct {
					Start struct {
						Line int `json:"line"`
					} `json:"start"`
				} `json:"targetRange"`
			}
			if err := json.Unmarshal(raw, &links); err != nil {
				return nil, fmt.Errorf("unreadable %s answer: %s", method, raw)
			}
			for _, l := range links {
				wire = append(wire, wireLocation{URI: l.Target, Range: struct {
					Start struct {
						Line int `json:"line"`
					} `json:"start"`
				}(l.Range)})
			}
		} else {
			wire = []wireLocation{one}
		}
	}

	out := make([]Location, 0, len(wire))
	for _, w := range wire {
		out = append(out, Location{Path: uriToPath(w.URI), Line: w.Range.Start.Line + 1})
	}
	return out, nil
}

// Close shuts the server down politely and reaps it.
func (c *Client) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	_ = c.call(ctx, "shutdown", nil, nil)
	_ = c.notify("exit", nil)
	c.in.Close()
	if c.cmd != nil {
		_ = c.cmd.Wait()
	}
}

func pathToURI(path string) string { return "file://" + filepath.ToSlash(path) }

func uriToPath(uri string) string {
	path := strings.TrimPrefix(uri, "file://")
	return filepath.FromSlash(path)
}

func languageOf(path string) string {
	switch filepath.Ext(path) {
	case ".go":
		return "go"
	case ".ts", ".tsx":
		return "typescript"
	case ".js", ".jsx":
		return "javascript"
	case ".py":
		return "python"
	case ".rs":
		return "rust"
	default:
		return strings.TrimPrefix(filepath.Ext(path), ".")
	}
}
