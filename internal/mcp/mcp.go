// Package mcp connects Model Context Protocol servers to the tool suite.
//
// The built-in suite stays small on purpose; MCP is the socket the long tail
// plugs into (design principle 5). This package speaks the protocol's client
// side over stdio and Streamable HTTP, and bridges each discovered tool into
// the registry under a namespaced name.
//
// Two constraints shape the code. Discovery happens once, at session
// assembly: tool definitions sit in the frozen zone of the context layout
// (§6.1), so a server that changes its tool list mid-session is deliberately
// not followed. And an MCP tool runs outside whatever sandbox this host
// verified — the server is a process this package started un-confined, acting
// wherever it acts — so every bridged call carries the external effect and
// the permission engine treats it accordingly.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
)

// protocolVersion is the revision this client implements. A server answering
// with a different one is accepted and recorded: tools/list and tools/call
// are stable across every published revision, and refusing a handshake over
// a date string would break servers that work.
const protocolVersion = "2025-06-18"

// Spec describes one configured server. Command starts a stdio server; URL
// reaches a Streamable HTTP one. Exactly one must be set.
type Spec struct {
	Name    string
	Command string
	Args    []string
	Env     map[string]string
	URL     string

	// Allow lists tool names (the server's own names, not the namespaced
	// form) the user pre-approved in config. Everything else asks.
	Allow []string
}

func (s Spec) validate() error {
	if s.Name == "" {
		return errors.New("mcp server has no name")
	}
	if (s.Command == "") == (s.URL == "") {
		return fmt.Errorf("mcp server %s: exactly one of command and url must be set", s.Name)
	}
	return nil
}

// ToolInfo is one tool as the server described it.
type ToolInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Result mirrors tools.Result without importing it: content for the model,
// and whether the tool itself reports failure.
type Result struct {
	Content string
	IsError bool
}

// transport carries newline-delimited JSON-RPC messages both ways. Close
// must unblock a pending Recv.
type transport interface {
	Send(msg []byte) error
	Recv() ([]byte, error)
	Close() error
}

// Client is one connected server. Methods are safe for concurrent use;
// requests are matched to responses by id, so calls can overlap even though
// the bridge serializes them today.
type Client struct {
	spec Spec

	// logf receives protocol-level notices: a crashed server, a log message
	// the server sent, a request it made that this client refuses. It is the
	// package's only output channel; nothing here writes to a terminal.
	logf func(level, text string)

	transport transport
	seq       atomic.Int64

	mu      sync.Mutex
	pending map[int64]chan rpcResponse
	dead    error // set once the read loop exits; sticky

	serverName    string
	serverVersion string
	protocol      string
	tools         []ToolInfo
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`

	// fatal marks a transport death rather than a server answer, so a dead
	// connection is never mistaken for a tool-level refusal the model could
	// correct.
	fatal error
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("%s (code %d)", e.Message, e.Code) }

// incoming is the shape every received line is first read into, enough to
// tell a response from a server-initiated request from a notification.
type incoming struct {
	ID     *int64          `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
	Params json.RawMessage `json:"params"`
}

// Connect starts (or reaches) the server, performs the initialize handshake,
// and lists its tools. The context bounds the whole sequence: a server that
// cannot say hello inside it is reported, not waited on.
func Connect(ctx context.Context, spec Spec, logf func(level, text string)) (*Client, error) {
	if err := spec.validate(); err != nil {
		return nil, err
	}
	if logf == nil {
		logf = func(string, string) {}
	}

	var tr transport
	var err error
	if spec.Command != "" {
		tr, err = startStdio(spec)
	} else {
		tr, err = startHTTP(spec)
	}
	if err != nil {
		return nil, err
	}

	c := &Client{
		spec:      spec,
		logf:      logf,
		transport: tr,
		pending:   map[int64]chan rpcResponse{},
	}
	go c.readLoop()

	if err := c.initialize(ctx); err != nil {
		c.Close()
		return nil, err
	}
	if err := c.listTools(ctx); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (c *Client) Name() string    { return c.spec.Name }
func (c *Client) Spec() Spec      { return c.spec }
func (c *Client) Tools() []ToolInfo {
	return append([]ToolInfo(nil), c.tools...)
}

// ServerLine describes the server for display: what it calls itself, and the
// protocol revision it answered with.
func (c *Client) ServerLine() string {
	name := c.serverName
	if name == "" {
		name = "unnamed server"
	}
	if c.serverVersion != "" {
		name += " " + c.serverVersion
	}
	return fmt.Sprintf("%s, protocol %s", name, c.protocol)
}

// Err reports why the connection died, or nil while it is alive.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dead
}

func (c *Client) initialize(ctx context.Context) error {
	params, _ := json.Marshal(map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "switchboard", "version": "dev"},
	})
	raw, err := c.call(ctx, "initialize", params)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("initialize: malformed result: %w", err)
	}
	c.protocol = res.ProtocolVersion
	c.serverName = res.ServerInfo.Name
	c.serverVersion = res.ServerInfo.Version
	if t, ok := c.transport.(interface{ setProtocol(string) }); ok && res.ProtocolVersion != "" {
		t.setProtocol(res.ProtocolVersion)
	}

	note, _ := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: "notifications/initialized"})
	return c.transport.Send(note)
}

func (c *Client) listTools(ctx context.Context) error {
	cursor := ""
	for {
		p := map[string]any{}
		if cursor != "" {
			p["cursor"] = cursor
		}
		params, _ := json.Marshal(p)
		raw, err := c.call(ctx, "tools/list", params)
		if err != nil {
			return fmt.Errorf("tools/list: %w", err)
		}
		var res struct {
			Tools      []ToolInfo `json:"tools"`
			NextCursor string     `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return fmt.Errorf("tools/list: malformed result: %w", err)
		}
		c.tools = append(c.tools, res.Tools...)
		if res.NextCursor == "" {
			return nil
		}
		cursor = res.NextCursor
	}
}

// Call invokes one tool. A tool-level failure comes back as a Result with
// IsError set, exactly as the built-in suite reports one, so the model can
// read it and correct itself; only a transport-level failure is an error.
func (c *Client) Call(ctx context.Context, tool string, args json.RawMessage) (Result, error) {
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	params, _ := json.Marshal(struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}{tool, args})

	raw, err := c.call(ctx, "tools/call", params)
	if err != nil {
		var rpcErr *rpcError
		if errors.As(err, &rpcErr) {
			// A protocol-level refusal (unknown tool, invalid params) is
			// something the model can act on; the connection is fine.
			return Result{Content: rpcErr.Message, IsError: true}, nil
		}
		return Result{}, err
	}

	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return Result{}, fmt.Errorf("tools/call: malformed result: %w", err)
	}

	var b strings.Builder
	for _, block := range res.Content {
		if block.Type == "text" {
			b.WriteString(block.Text)
			continue
		}
		fmt.Fprintf(&b, "[%s content omitted]", block.Type)
	}
	return Result{Content: b.String(), IsError: res.IsError}, nil
}

// call sends one request and waits for its response or the context.
func (c *Client) call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	id := c.seq.Add(1)
	ch := make(chan rpcResponse, 1)

	c.mu.Lock()
	if c.dead != nil {
		err := c.dead
		c.mu.Unlock()
		return nil, err
	}
	c.pending[id] = ch
	c.mu.Unlock()

	msg, _ := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params})
	if err := c.transport.Send(msg); err != nil {
		c.forget(id)
		return nil, err
	}

	select {
	case resp := <-ch:
		if resp.fatal != nil {
			return nil, resp.fatal
		}
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	case <-ctx.Done():
		c.forget(id)
		return nil, ctx.Err()
	}
}

func (c *Client) forget(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// readLoop is the single reader. It dispatches responses by id, answers the
// pings a server is entitled to send, refuses the requests this client does
// not implement, and surfaces the server's log notifications. When the
// transport ends, every pending call fails with the reason.
func (c *Client) readLoop() {
	for {
		line, err := c.transport.Recv()
		if err != nil {
			c.fail(fmt.Errorf("mcp server %s: connection closed: %w", c.spec.Name, err))
			return
		}
		var msg incoming
		if err := json.Unmarshal(line, &msg); err != nil {
			c.logf("warn", fmt.Sprintf("mcp %s sent unparseable output; ignoring", c.spec.Name))
			continue
		}

		switch {
		case msg.ID != nil && msg.Method == "": // response
			c.mu.Lock()
			ch, ok := c.pending[*msg.ID]
			delete(c.pending, *msg.ID)
			c.mu.Unlock()
			if ok {
				ch <- rpcResponse{Result: msg.Result, Error: msg.Error}
			}
		case msg.ID != nil: // server-initiated request
			c.answer(*msg.ID, msg.Method)
		default: // notification
			c.notified(msg.Method, msg.Params)
		}
	}
}

// answer replies to a server-initiated request. Ping gets its empty result;
// everything else — sampling, roots, elicitation — is declined with
// method-not-found, because each would put this client in a role the user
// never granted it (a sampling request is the server spending the user's
// model budget).
func (c *Client) answer(id int64, method string) {
	type errBody struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	var msg []byte
	if method == "ping" {
		msg, _ = json.Marshal(struct {
			JSONRPC string         `json:"jsonrpc"`
			ID      int64          `json:"id"`
			Result  map[string]any `json:"result"`
		}{"2.0", id, map[string]any{}})
	} else {
		msg, _ = json.Marshal(struct {
			JSONRPC string  `json:"jsonrpc"`
			ID      int64   `json:"id"`
			Error   errBody `json:"error"`
		}{"2.0", id, errBody{-32601, "switchboard does not serve " + method}})
	}
	if err := c.transport.Send(msg); err != nil {
		c.logf("warn", fmt.Sprintf("mcp %s: failed to answer %s: %v", c.spec.Name, method, err))
	}
}

func (c *Client) notified(method string, params json.RawMessage) {
	switch method {
	case "notifications/message":
		var p struct {
			Level string `json:"level"`
			Data  any    `json:"data"`
		}
		_ = json.Unmarshal(params, &p)
		c.logf("info", fmt.Sprintf("mcp %s: %v", c.spec.Name, p.Data))
	case "notifications/tools/list_changed":
		// Deliberately not followed mid-session: the definitions are in the
		// frozen zone (§6.1). The next session will list again.
		c.logf("warn", fmt.Sprintf("mcp %s changed its tool list; the new set applies from the next session", c.spec.Name))
	}
}

// fail marks the client dead and drains every waiter.
func (c *Client) fail(err error) {
	c.mu.Lock()
	if c.dead == nil {
		c.dead = err
	}
	waiters := c.pending
	c.pending = map[int64]chan rpcResponse{}
	c.mu.Unlock()

	for _, ch := range waiters {
		ch <- rpcResponse{fatal: err}
	}
}

func (c *Client) Close() error {
	err := c.transport.Close()
	c.fail(errors.New("client closed"))
	return err
}
