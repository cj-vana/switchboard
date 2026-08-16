package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cj-vana/switchboard/internal/permission"
)

// fakeTransport is an in-memory wire, with a scripted server on the far end.
type fakeTransport struct {
	toServer   chan []byte
	fromServer chan []byte
	closed     chan struct{}
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{
		toServer:   make(chan []byte, 16),
		fromServer: make(chan []byte, 16),
		closed:     make(chan struct{}),
	}
}

func (f *fakeTransport) Send(msg []byte) error {
	select {
	case f.toServer <- append([]byte(nil), msg...):
		return nil
	case <-f.closed:
		return errors.New("closed")
	}
}

func (f *fakeTransport) Recv() ([]byte, error) {
	select {
	case msg := <-f.fromServer:
		return msg, nil
	case <-f.closed:
		return nil, errors.New("closed")
	}
}

func (f *fakeTransport) Close() error {
	select {
	case <-f.closed:
	default:
		close(f.closed)
	}
	return nil
}

// serveScript answers initialize and tools/list with canned data and
// tools/call through the supplied function. A nil onCall, or one returning
// the empty string, leaves the call unanswered, which is how a test models a
// server that hangs. It stops when the transport closes.
func serveScript(f *fakeTransport, tools []ToolInfo, onCall func(name string, args json.RawMessage) string) {
	for {
		var raw []byte
		select {
		case raw = <-f.toServer:
		case <-f.closed:
			return
		}
		var req struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(raw, &req) != nil || req.ID == nil {
			continue // notification
		}
		var result string
		switch req.Method {
		case "initialize":
			result = `{"protocolVersion":"2025-06-18","serverInfo":{"name":"fake","version":"1.0"}}`
		case "tools/list":
			b, _ := json.Marshal(struct {
				Tools []ToolInfo `json:"tools"`
			}{tools})
			result = string(b)
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			if onCall == nil {
				continue
			}
			if result = onCall(p.Name, p.Arguments); result == "" {
				continue
			}
		default:
			result = "{}"
		}
		msg := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, *req.ID, result)
		f.fromServer <- []byte(msg)
	}
}

func connectFake(t *testing.T, tools []ToolInfo, onCall func(string, json.RawMessage) string) (*Client, *fakeTransport) {
	t.Helper()
	f := newFakeTransport()
	go serveScript(f, tools, onCall)

	c := &Client{
		spec:      Spec{Name: "fake", Command: "unused"},
		logf:      func(string, string) {},
		transport: f,
		pending:   map[int64]chan rpcResponse{},
	}
	go c.readLoop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.listTools(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c, f
}

var echoTool = []ToolInfo{{
	Name:        "echo",
	Description: "echoes",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
}}

func TestConnectDiscoversTools(t *testing.T) {
	c, _ := connectFake(t, echoTool, nil)

	tools := c.Tools()
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools = %+v, want the served echo tool", tools)
	}
	if !strings.Contains(c.ServerLine(), "fake 1.0") || !strings.Contains(c.ServerLine(), "2025-06-18") {
		t.Errorf("ServerLine() = %q, want name, version, and protocol", c.ServerLine())
	}
}

func TestCallFlattensContentAndPassesErrors(t *testing.T) {
	c, _ := connectFake(t, echoTool, func(name string, args json.RawMessage) string {
		switch name {
		case "mixed":
			return `{"content":[{"type":"text","text":"hello "},{"type":"image","data":"..."},{"type":"text","text":"world"}]}`
		case "failing":
			return `{"content":[{"type":"text","text":"it broke"}],"isError":true}`
		}
		return `{"content":[]}`
	})

	ctx := context.Background()
	res, err := c.Call(ctx, "mixed", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "hello [image content omitted]world" || res.IsError {
		t.Errorf("mixed call = %+v", res)
	}

	res, err = c.Call(ctx, "failing", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || res.Content != "it broke" {
		t.Errorf("failing call = %+v, want the server's error text with IsError", res)
	}
}

func TestProtocolRefusalIsAToolError(t *testing.T) {
	f := newFakeTransport()
	go func() {
		for {
			var raw []byte
			select {
			case raw = <-f.toServer:
			case <-f.closed:
				return
			}
			var req struct {
				ID     *int64 `json:"id"`
				Method string `json:"method"`
			}
			if json.Unmarshal(raw, &req) != nil || req.ID == nil {
				continue
			}
			switch req.Method {
			case "initialize":
				f.fromServer <- []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-06-18","serverInfo":{"name":"fake"}}}`, *req.ID))
			case "tools/list":
				f.fromServer <- []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"tools":[]}}`, *req.ID))
			default:
				f.fromServer <- []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32602,"message":"unknown tool"}}`, *req.ID))
			}
		}
	}()

	c := &Client{spec: Spec{Name: "fake"}, logf: func(string, string) {}, transport: f, pending: map[int64]chan rpcResponse{}}
	go c.readLoop()
	t.Cleanup(func() { c.Close() })

	ctx := context.Background()
	if err := c.initialize(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := c.Call(ctx, "nope", nil)
	if err != nil {
		t.Fatalf("a protocol refusal must not be a transport error: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "unknown tool") {
		t.Errorf("res = %+v, want the refusal as a tool error the model can read", res)
	}
}

func TestServerRequestsAreAnswered(t *testing.T) {
	f := newFakeTransport()
	// Serve exactly the handshake, then stop reading: after this the test
	// owns both channels, so nothing races it for the client's replies.
	served := make(chan struct{})
	go func() {
		defer close(served)
		answered := 0
		for answered < 2 {
			raw := <-f.toServer
			var req struct {
				ID     *int64 `json:"id"`
				Method string `json:"method"`
			}
			if json.Unmarshal(raw, &req) != nil || req.ID == nil {
				continue
			}
			switch req.Method {
			case "initialize":
				f.fromServer <- []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-06-18","serverInfo":{"name":"fake"}}}`, *req.ID))
			case "tools/list":
				f.fromServer <- []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"tools":[]}}`, *req.ID))
			}
			answered++
		}
	}()

	c := &Client{spec: Spec{Name: "fake"}, logf: func(string, string) {}, transport: f, pending: map[int64]chan rpcResponse{}}
	go c.readLoop()
	t.Cleanup(func() { c.Close() })

	ctx := context.Background()
	if err := c.initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.listTools(ctx); err != nil {
		t.Fatal(err)
	}
	<-served

	// A ping is answered with an empty result.
	f.fromServer <- []byte(`{"jsonrpc":"2.0","id":900,"method":"ping"}`)
	reply := <-f.toServer
	if !strings.Contains(string(reply), `"id":900`) || !strings.Contains(string(reply), `"result"`) {
		t.Errorf("ping reply = %s, want an empty result", reply)
	}

	// Sampling would spend the user's model budget on the server's behalf;
	// it is refused, not ignored, so the server is not left hanging.
	f.fromServer <- []byte(`{"jsonrpc":"2.0","id":901,"method":"sampling/createMessage","params":{}}`)
	reply = <-f.toServer
	if !strings.Contains(string(reply), `"id":901`) || !strings.Contains(string(reply), "-32601") {
		t.Errorf("sampling reply = %s, want method-not-found", reply)
	}
}

func TestDeadTransportFailsPendingAndFutureCalls(t *testing.T) {
	c, f := connectFake(t, echoTool, nil)

	done := make(chan error, 1)
	go func() {
		_, err := c.Call(context.Background(), "echo", json.RawMessage(`{"text":"hi"}`))
		done <- err
	}()
	// Give the call a moment to register as pending, then kill the wire.
	time.Sleep(50 * time.Millisecond)
	f.Close()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a call pending on a dead transport must fail")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pending call never failed after transport death")
	}

	if c.Err() == nil {
		t.Error("Err() must report the death")
	}
	if _, err := c.Call(context.Background(), "echo", nil); err == nil {
		t.Error("calls after death must fail immediately")
	}
}

func TestBridgedToolCarriesTheExternalEffect(t *testing.T) {
	c, _ := connectFake(t, echoTool, func(name string, args json.RawMessage) string {
		return `{"content":[{"type":"text","text":"echoed"}]}`
	})

	bridged := c.BridgedTools()
	if len(bridged) != 1 {
		t.Fatalf("bridged = %d tools, want 1", len(bridged))
	}
	tool := bridged[0]
	if tool.Name() != "mcp__fake__echo" {
		t.Errorf("Name() = %q, want the namespaced form", tool.Name())
	}
	if tool.ParallelSafe() {
		t.Error("an opaque external effect must not be parallel-safe")
	}
	if !strings.Contains(tool.Description(), "[fake MCP]") {
		t.Errorf("Description() = %q, want the provenance prefix", tool.Description())
	}

	plan, err := tool.Plan(json.RawMessage(`{"text":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Request.Effect != permission.EffectExternal {
		t.Errorf("Effect = %q, want external", plan.Request.Effect)
	}
	if plan.Request.Tool != "mcp__fake__echo" {
		t.Errorf("Request.Tool = %q", plan.Request.Tool)
	}
	if !strings.Contains(plan.Request.Detail, `"text":"hi"`) || !strings.Contains(plan.Request.Detail, "fake server") {
		t.Errorf("Detail = %q, want the arguments and the server", plan.Request.Detail)
	}

	res, err := plan.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "echoed" || res.IsError {
		t.Errorf("run = %+v", res)
	}
}

func TestNamespacedSanitizes(t *testing.T) {
	if got := Namespaced("my server", "read.file"); got != "mcp__my_server__read_file" {
		t.Errorf("Namespaced = %q", got)
	}
}

func TestAllowRulesNameTheNamespacedTool(t *testing.T) {
	c := &Client{spec: Spec{Name: "gh", Allow: []string{"search"}}}
	rules := c.AllowRules()
	if len(rules) != 1 {
		t.Fatalf("rules = %+v", rules)
	}
	r := rules[0]
	if r.Decision != permission.Allow || r.Tool != "mcp__gh__search" || r.Effect != permission.EffectExternal {
		t.Errorf("rule = %+v", r)
	}
}

func TestBridgeSchemaFallsBackToAnObject(t *testing.T) {
	c := &Client{spec: Spec{Name: "s"}, logf: func(string, string) {}, tools: []ToolInfo{{Name: "bare"}}}
	bridged := c.BridgedTools()
	if len(bridged) != 1 {
		t.Fatal("want the bare tool bridged")
	}
	var schema struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(bridged[0].Schema(), &schema); err != nil || schema.Type != "object" {
		t.Errorf("schema = %s, want an object schema", bridged[0].Schema())
	}
}
