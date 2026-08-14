package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/cj-vana/switchboard/internal/provider"
)

func serve(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	c, err := New("ollama", WithBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func serveBody(t *testing.T, body string) *Client {
	t.Helper()
	return serve(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, body)
	})
}

func drain(t *testing.T, s provider.EventStream) ([]provider.Event, error) {
	t.Helper()
	var events []provider.Event
	for {
		ev, err := s.Next()
		if errors.Is(err, io.EOF) {
			return events, nil
		}
		if err != nil {
			return events, err
		}
		events = append(events, ev)
	}
}

func target(t *testing.T, model string) provider.RouteTarget {
	t.Helper()
	tgt, err := Target("ollama", model)
	if err != nil {
		t.Fatal(err)
	}
	return tgt
}

// The fixture is a verbatim capture from Ollama's /v1 endpoint, so the mapping
// is tested against what a real compatible server sends rather than against
// the format's documentation.
func TestRecordedStream(t *testing.T) {
	fixture, err := os.ReadFile("testdata/tool_call_stream.sse")
	if err != nil {
		t.Fatal(err)
	}
	c := serveBody(t, string(fixture))

	s, err := c.Stream(context.Background(), target(t, "qwen3.5:9b-mlx"), provider.Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	events, err := drain(t, s)
	if err != nil {
		t.Fatalf("draining: %v", err)
	}

	var reasoning strings.Builder
	var uses []provider.ToolUse
	var done *provider.Event
	for i, ev := range events {
		switch ev.Type {
		case provider.EventThinkingDelta:
			reasoning.WriteString(ev.Text)
		case provider.EventToolUse:
			uses = append(uses, *ev.ToolUse)
		case provider.EventDone:
			done = &events[i]
		}
	}

	if !strings.Contains(reasoning.String(), "main.go") {
		t.Errorf("reassembled reasoning looks wrong: %q", reasoning.String())
	}
	if len(uses) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(uses))
	}
	if uses[0].Name != "read" || uses[0].ID != "call_1lwhr0uw" {
		t.Errorf("tool call = %+v", uses[0])
	}

	// The wire carries arguments as an escaped string. The canonical type
	// carries JSON, so the conversion has to actually happen.
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(uses[0].Input, &args); err != nil {
		t.Fatalf("tool arguments did not become JSON: %v", err)
	}
	if args.Path != "main.go" {
		t.Errorf("args.path = %q", args.Path)
	}

	if done == nil {
		t.Fatal("no done event")
	}
	if done.StopReason != provider.StopToolUse {
		t.Errorf("StopReason = %q, want tool_use", done.StopReason)
	}
	if done.Usage.InputTokens != 274 || done.Usage.OutputTokens != 57 {
		t.Errorf("usage = %+v, want 274 in / 57 out", done.Usage)
	}
}

// OpenAI and most compatible servers split tool arguments across chunks. Ollama
// does not, so this is the case the recorded fixture cannot cover.
func TestToolArgumentsSplitAcrossChunks(t *testing.T) {
	c := serveBody(t, strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read","arguments":"{\"pa"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"m"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ain.go\"}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n\n"))

	s, err := c.Stream(context.Background(), target(t, "m"), provider.Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	events, err := drain(t, s)
	if err != nil {
		t.Fatal(err)
	}

	var use *provider.ToolUse
	for _, ev := range events {
		if ev.Type == provider.EventToolUse {
			use = ev.ToolUse
		}
	}
	if use == nil {
		t.Fatal("the split call never completed")
	}
	if got := string(use.Input); got != `{"path":"main.go"}` {
		t.Errorf("reassembled arguments = %s", got)
	}
	if use.ID != "call_a" {
		t.Errorf("id = %q; the id arrives only in the first fragment", use.ID)
	}
}

func TestTwoParallelToolCalls(t *testing.T) {
	c := serveBody(t, strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[` +
			`{"index":0,"id":"a","function":{"name":"read","arguments":"{\"path\":\"one\"}"}},` +
			`{"index":1,"id":"b","function":{"name":"read","arguments":"{\"path\":\"two\"}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n\n"))

	s, _ := c.Stream(context.Background(), target(t, "m"), provider.Request{})
	defer s.Close()
	events, err := drain(t, s)
	if err != nil {
		t.Fatal(err)
	}

	var ids []string
	for _, ev := range events {
		if ev.Type == provider.EventToolUse {
			ids = append(ids, ev.ToolUse.ID)
		}
	}
	// Index order, not map order: the loop pairs results to calls positionally.
	if strings.Join(ids, ",") != "a,b" {
		t.Errorf("tool call order = %v, want a,b", ids)
	}
}

func TestMalformedArgumentsAreReported(t *testing.T) {
	c := serveBody(t, strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"content":"before"}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"read","arguments":"{\"path\":"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n\n"))

	s, _ := c.Stream(context.Background(), target(t, "m"), provider.Request{})
	defer s.Close()

	events, err := drain(t, s)
	var protoErr *provider.ProtocolError
	if !errors.As(err, &protoErr) {
		t.Fatalf("err = %v, want a ProtocolError for arguments that never completed", err)
	}
	// Text that arrived before the bad call is still real output.
	if len(events) == 0 || events[0].Text != "before" {
		t.Errorf("output before the malformed call was discarded: %+v", events)
	}
}

func TestTruncatedStreamIsDistinguishable(t *testing.T) {
	c := serveBody(t, `data: {"choices":[{"index":0,"delta":{"content":"half a th"}}]}`+"\n")

	s, _ := c.Stream(context.Background(), target(t, "m"), provider.Request{})
	defer s.Close()

	events, err := drain(t, s)
	if !errors.Is(err, provider.ErrStreamIncomplete) {
		t.Fatalf("err = %v, want ErrStreamIncomplete", err)
	}
	if len(events) != 1 || events[0].Text != "half a th" {
		t.Errorf("partial content was lost: %+v", events)
	}
}

func TestKeepAlivesAndUnknownFieldsAreIgnored(t *testing.T) {
	c := serveBody(t, strings.Join([]string{
		`: keep-alive`,
		`event: message`,
		`id: 42`,
		`data: {"choices":[{"index":0,"delta":{"content":"hello"}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}, "\n"))

	s, _ := c.Stream(context.Background(), target(t, "m"), provider.Request{})
	defer s.Close()
	events, err := drain(t, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Text != "hello" {
		t.Errorf("events = %+v", events)
	}
	if events[1].StopReason != provider.StopEndTurn {
		t.Errorf("StopReason = %q", events[1].StopReason)
	}
}

func TestCachedPromptTokensSplitOutOfInput(t *testing.T) {
	c := serveBody(t, strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":800}}}`,
		`data: [DONE]`,
	}, "\n\n"))

	s, _ := c.Stream(context.Background(), target(t, "m"), provider.Request{})
	defer s.Close()
	events, err := drain(t, s)
	if err != nil {
		t.Fatal(err)
	}

	// A usage chunk can arrive after finish_reason, so the terminal event has
	// to carry what was known by the end of the stream.
	var usage provider.Usage
	for _, ev := range events {
		if ev.Type == provider.EventDone {
			usage = ev.Usage
		}
	}
	if usage.CacheReadTokens != 800 {
		t.Errorf("cache reads = %d, want 800", usage.CacheReadTokens)
	}
	if usage.InputTokens != 200 {
		t.Errorf("uncached input = %d, want the 200 that were not served from cache", usage.InputTokens)
	}
}

func TestNestedErrorShapeIsUnwrapped(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":{"message":"model 'nope' not found","type":"not_found_error"}}`)
	})

	_, err := c.Stream(context.Background(), target(t, "nope"), provider.Request{})
	var apiErr *provider.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *provider.APIError", err)
	}
	if apiErr.Body != "model 'nope' not found" {
		t.Errorf("body = %q, want the message unwrapped from the nested object", apiErr.Body)
	}
}

func TestBuildRequestShape(t *testing.T) {
	c, err := New("ollama")
	if err != nil {
		t.Fatal(err)
	}

	req := provider.Request{
		System: []provider.Block{provider.Text{Text: "be terse"}},
		Tools: []provider.ToolDefinition{{
			Name: "read", Description: "Read a file", Schema: json.RawMessage(`{"type":"object"}`),
		}},
		Messages: []provider.Message{
			provider.UserText("read main.go"),
			{Role: provider.RoleAssistant, Content: []provider.Block{
				provider.Thinking{Text: "need the file"},
				provider.ToolUse{ID: "call_1", Name: "read", Input: json.RawMessage(`{"path":"main.go"}`)},
			}},
			{Role: provider.RoleTool, Content: []provider.Block{
				provider.ToolResult{ToolUseID: "call_1", Name: "read", Content: "package main"},
				provider.ToolResult{ToolUseID: "call_2", Name: "exec", Content: "boom", IsError: true},
			}},
			{Role: provider.RoleAssistant, Incomplete: true, Content: []provider.Block{
				provider.Text{Text: "cut off"},
			}},
		},
	}

	raw, err := c.buildRequest(target(t, "m"), req)
	if err != nil {
		t.Fatal(err)
	}
	var got chatRequest
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	roles := make([]string, len(got.Messages))
	for i, m := range got.Messages {
		roles[i] = m.Role
	}
	if strings.Join(roles, ",") != "system,user,assistant,tool,tool" {
		t.Fatalf("roles = %v", roles)
	}
	if got.Messages[3].ToolCallID != "call_1" || got.Messages[4].ToolCallID != "call_2" {
		t.Errorf("results not correlated by id: %q, %q", got.Messages[3].ToolCallID, got.Messages[4].ToolCallID)
	}

	// The canonical type holds JSON; the wire wants a string. Getting this
	// backwards is the most likely way a round trip through two adapters
	// stops meaning the same thing.
	if n := len(got.Messages[2].ToolCalls); n != 1 {
		t.Fatalf("assistant tool calls = %d", n)
	}
	if args := got.Messages[2].ToolCalls[0].Function.Arguments; args != `{"path":"main.go"}` {
		t.Errorf("arguments = %q, want a JSON string", args)
	}
	if got.Messages[2].Reasoning != "need the file" {
		t.Errorf("reasoning = %q, want it echoed for replay", got.Messages[2].Reasoning)
	}
	if got.StreamOptions == nil || !got.StreamOptions.IncludeUsage {
		t.Error("the ollama profile reports stream usage, so it must be requested")
	}
}

func TestUnsupportedEffortIsACapabilityError(t *testing.T) {
	c, _ := New("ollama")
	tgt := target(t, "m")
	tgt.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: "ludicrous"}

	_, err := c.buildRequest(tgt, provider.Request{})
	var capErr *provider.CapabilityError
	if !errors.As(err, &capErr) {
		t.Fatalf("err = %v, want *provider.CapabilityError", err)
	}
}

// The generic profile is the floor for an endpoint nobody has characterized,
// so it must not claim capabilities it has not been shown to have.
func TestGenericProfileDoesNotClaimUntestedCapabilities(t *testing.T) {
	c, err := New("generic", WithBaseURL("http://example.invalid/v1"))
	if err != nil {
		t.Fatal(err)
	}
	if c.profile.StreamUsage {
		t.Error("the generic profile must not assume stream_options support")
	}
	if len(c.profile.EffortLevels) != 0 {
		t.Error("the generic profile must not assume reasoning_effort support")
	}

	tgt := provider.RouteTarget{Provider: Name, Surface: "generic", ModelID: "m"}
	tgt.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: "high"}
	if _, err := c.buildRequest(tgt, provider.Request{}); err == nil {
		t.Error("requesting effort on an uncharacterized endpoint must be an error, not a silent drop")
	}
}

func TestUnknownProfileIsRejected(t *testing.T) {
	if _, err := New("definitely-not-tested"); err == nil {
		t.Error("an unknown profile must be an error rather than a silent fall back to the floor")
	}
}

func TestCachePlanIsRejected(t *testing.T) {
	c, _ := New("ollama")
	req := provider.Request{CachePlan: &provider.CachePlan{Breakpoints: []provider.Breakpoint{{}}}}
	_, err := c.buildRequest(target(t, "m"), req)
	var capErr *provider.CapabilityError
	if !errors.As(err, &capErr) {
		t.Fatalf("err = %v, want *provider.CapabilityError", err)
	}
}

func TestContextCancellationStopsStream(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"one\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})

	ctx, cancel := context.WithCancel(context.Background())
	s, err := c.Stream(ctx, target(t, "m"), provider.Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.Next(); err != nil {
		t.Fatalf("first event: %v", err)
	}
	cancel()
	if _, err := s.Next(); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}
