package anthropic

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
	"time"

	"github.com/switchboard-code/switchboard/internal/provider"
)

func serve(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(WithBaseURL(srv.URL))
}

func serveFixture(t *testing.T, name string) *Client {
	t.Helper()
	body, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return serve(t, func(w http.ResponseWriter, _ *http.Request) { w.Write(body) })
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

func collect(t *testing.T, c *Client, req provider.Request) []provider.Event {
	t.Helper()
	s, err := c.Stream(context.Background(), Target("claude-haiku-4-5"), req)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	events, err := drain(t, s)
	if err != nil {
		t.Fatalf("draining: %v", err)
	}
	return events
}

func done(t *testing.T, events []provider.Event) provider.Event {
	t.Helper()
	for _, ev := range events {
		if ev.Type == provider.EventDone {
			return ev
		}
	}
	t.Fatal("no terminal event")
	return provider.Event{}
}

// The fixtures are verbatim captures of two identical requests, the second of
// which hit the cache the first wrote. That pair is the whole point of this
// adapter: §6.3 requires cache state be updated from observed usage rather than
// assumed from how a request was built, and no local target reports any.
func TestCacheWriteAndReadAreDistinctObservations(t *testing.T) {
	write := done(t, collect(t, serveFixture(t, "tool_call_cache_write.sse"), provider.Request{}))
	read := done(t, collect(t, serveFixture(t, "tool_call_cache_read.sse"), provider.Request{}))

	if write.Usage.CacheWriteTokens != 11153 {
		t.Errorf("write usage = %+v, want 11153 written", write.Usage)
	}
	if write.Usage.CacheReadTokens != 0 {
		t.Errorf("the first call read %d cached tokens; it had nothing to read", write.Usage.CacheReadTokens)
	}

	if read.Usage.CacheReadTokens != 11153 {
		t.Errorf("read usage = %+v, want 11153 read", read.Usage)
	}
	if read.Usage.CacheWriteTokens != 0 {
		t.Errorf("the second call wrote %d cached tokens; it hit the existing entry", read.Usage.CacheWriteTokens)
	}

	// A write and a read cost different amounts, so collapsing them would let
	// the estimator price a hit as a miss and never notice.
	if write.Usage.CacheWriteTokens == read.Usage.CacheWriteTokens {
		t.Error("a write observation and a read observation came out identical")
	}
}

// The three input counts are disjoint here, unlike the compatible format where
// cached tokens are a subset of the prompt count. Subtracting would lose a whole
// prefix; adding in the other adapter would double-count one.
func TestInputCountsAreDisjoint(t *testing.T) {
	ev := done(t, collect(t, serveFixture(t, "tool_call_cache_read.sse"), provider.Request{}))

	if ev.Usage.InputTokens != 337 {
		t.Errorf("input = %d, want the uncached remainder of 337", ev.Usage.InputTokens)
	}
	if total := ev.Usage.InputTokens + ev.Usage.CacheReadTokens + ev.Usage.CacheWriteTokens; total != 11490 {
		t.Errorf("the prompt totals %d, want 11490 as the sum of the three counts", total)
	}
}

// Usage arrives in two events. message_start carries the input and cache counts
// with a placeholder output count of 1; message_delta carries the final output
// count. Taking either alone reports the turn wrong.
func TestUsageIsMergedAcrossEvents(t *testing.T) {
	ev := done(t, collect(t, serveFixture(t, "tool_call_cache_write.sse"), provider.Request{}))

	if ev.Usage.OutputTokens != 65 {
		t.Errorf("output = %d, want the final 65 rather than the placeholder from message_start", ev.Usage.OutputTokens)
	}
	if ev.Usage.CacheWriteTokens != 11153 {
		t.Errorf("cache write = %d; message_delta must not zero what message_start reported", ev.Usage.CacheWriteTokens)
	}
}

func TestToolCallIsAssembledFromPartialJSON(t *testing.T) {
	events := collect(t, serveFixture(t, "tool_call_cache_write.sse"), provider.Request{})

	var uses []provider.ToolUse
	for _, ev := range events {
		if ev.Type == provider.EventToolUse {
			uses = append(uses, *ev.ToolUse)
		}
	}
	if len(uses) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(uses))
	}
	if uses[0].Name != "read" || !strings.HasPrefix(uses[0].ID, "toolu_") {
		t.Errorf("tool call = %+v", uses[0])
	}

	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(uses[0].Input, &args); err != nil {
		t.Fatalf("arguments did not reassemble into JSON: %v", err)
	}
	if args.Path != "main.go" {
		t.Errorf("args.path = %q", args.Path)
	}

	if got := done(t, events).StopReason; got != provider.StopToolUse {
		t.Errorf("StopReason = %q, want tool_use", got)
	}
}

// Replaying a thinking block without its signature is rejected by the server,
// so the signature has to survive the stream. Confirmed against the live API:
// a stripped signature comes back "Invalid `signature` in `thinking` block".
func TestThinkingSignatureReachesTheCaller(t *testing.T) {
	events := collect(t, serveFixture(t, "thinking.sse"), provider.Request{})

	var text, signature strings.Builder
	for _, ev := range events {
		if ev.Type == provider.EventThinkingDelta {
			text.WriteString(ev.Text)
			signature.WriteString(ev.Signature)
		}
	}
	if text.Len() == 0 {
		t.Error("no thinking text was emitted")
	}
	if signature.Len() == 0 {
		t.Fatal("the thinking block arrived unsigned; replaying it would be rejected")
	}
}

// An unsigned thinking block is dropped rather than sent. Dropping one is
// accepted by the server and replaying it unsigned is not, so sending it would
// turn a recoverable omission into a failed request.
func TestUnsignedThinkingIsDroppedNotSent(t *testing.T) {
	target := Target("claude-haiku-4-5")
	body, err := New().buildRequest(target, provider.Request{
		Messages: []provider.Message{{
			Role: provider.RoleAssistant,
			Content: []provider.Block{
				provider.Thinking{Text: "unsigned reasoning"},
				provider.Text{Text: "the answer"},
			},
		}},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "unsigned reasoning") {
		t.Errorf("an unsigned thinking block was sent and would be refused:\n%s", body)
	}
	if !strings.Contains(string(body), "the answer") {
		t.Error("dropping the thinking block took the text with it")
	}
}

func TestSignedThinkingIsReplayed(t *testing.T) {
	body, err := New().buildRequest(Target("claude-haiku-4-5"), provider.Request{
		Messages: []provider.Message{{
			Role:    provider.RoleAssistant,
			Content: []provider.Block{provider.Thinking{Text: "reasoning", Signature: "sig-abc"}},
		}},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "sig-abc") {
		t.Errorf("the signature did not survive into the request:\n%s", body)
	}
}

func TestCacheBreakpointsLandWhereTheyWereScored(t *testing.T) {
	target := Target("claude-haiku-4-5")
	req := provider.Request{
		System: []provider.Block{provider.Text{Text: "frozen"}},
		Tools:  []provider.ToolDefinition{{Name: "read", Schema: json.RawMessage(`{"type":"object"}`)}},
		Messages: []provider.Message{
			provider.UserText("first"),
			provider.UserText("second"),
		},
		CachePlan: &provider.CachePlan{Breakpoints: []provider.Breakpoint{
			{Position: provider.CachePosition{MessageIndex: provider.SystemBlocks, BlockIndex: 0}},
			{Position: provider.CachePosition{MessageIndex: provider.ToolDefinitions, BlockIndex: 0}, TTL: time.Hour},
			{Position: provider.CachePosition{MessageIndex: 1, BlockIndex: 0}},
		}},
	}

	body, err := New().buildRequest(target, req, true)
	if err != nil {
		t.Fatal(err)
	}

	var decoded struct {
		System []struct {
			Text         string          `json:"text"`
			CacheControl json.RawMessage `json:"cache_control"`
		} `json:"system"`
		Tools []struct {
			CacheControl struct {
				Type string `json:"type"`
				TTL  string `json:"ttl"`
			} `json:"cache_control"`
		} `json:"tools"`
		Messages []struct {
			Content []struct {
				Text         string          `json:"text"`
				CacheControl json.RawMessage `json:"cache_control"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}

	if len(decoded.System) == 0 || decoded.System[0].CacheControl == nil {
		t.Error("the system breakpoint did not render")
	}
	if decoded.Tools[0].CacheControl.TTL != "1h" {
		t.Errorf("tool breakpoint ttl = %q, want 1h", decoded.Tools[0].CacheControl.TTL)
	}
	// The marker has to sit on the second message, not the first: one position
	// off caches a different prefix than the one whose reuse was scored.
	if decoded.Messages[0].Content[0].CacheControl != nil {
		t.Error("a marker landed on the first message, which had no breakpoint")
	}
	if decoded.Messages[1].Content[0].CacheControl == nil {
		t.Error("the message breakpoint did not render")
	}
}

// There is no tool role in this API. A tool result is a user message carrying
// tool_result blocks, and sending "tool" is rejected with "Unexpected role",
// which ends the second turn of every tool-using conversation.
func TestToolResultsBecomeUserMessages(t *testing.T) {
	body, err := New().buildRequest(Target("claude-haiku-4-5"), provider.Request{
		Messages: []provider.Message{
			provider.UserText("read main.go"),
			{Role: provider.RoleAssistant, Content: []provider.Block{
				provider.ToolUse{ID: "toolu_1", Name: "read", Input: json.RawMessage(`{"path":"main.go"}`)},
			}},
			{Role: provider.RoleTool, Content: []provider.Block{
				provider.ToolResult{ToolUseID: "toolu_1", Name: "read", Content: "package main"},
			}},
		},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `"role":"tool"`) {
		t.Fatalf("a tool role reached the wire and would be rejected:\n%s", body)
	}

	var decoded struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string `json:"type"`
				ToolUseID string `json:"tool_use_id"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	last := decoded.Messages[len(decoded.Messages)-1]
	if last.Role != "user" {
		t.Errorf("the tool result went out as role %q, want user", last.Role)
	}
	if len(last.Content) != 1 || last.Content[0].Type != "tool_result" || last.Content[0].ToolUseID != "toolu_1" {
		t.Errorf("the tool result did not survive the role change: %+v", last.Content)
	}
}

func TestContinuityAndPromptRemainIsolatedTextBlocks(t *testing.T) {
	body, err := New().buildRequest(Target("claude-haiku-4-5"), provider.Request{Messages: []provider.Message{{
		Role: provider.RoleUser,
		Content: []provider.Block{
			provider.Text{Text: "[continuity capsule]\n\n"},
			provider.Text{Text: "continue with the fix"},
		},
		ContinuityRef: "0123456789abcdef0123456789abcdef",
	}}}, true)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Messages []struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Messages) != 1 || len(decoded.Messages[0].Content) != 2 ||
		decoded.Messages[0].Content[0].Text != "[continuity capsule]\n\n" ||
		decoded.Messages[0].Content[1].Text != "continue with the fix" {
		t.Fatalf("Anthropic continuity blocks = %+v", decoded.Messages)
	}
	if strings.Contains(string(body), "continuity_ref") {
		t.Fatal("session-only continuity metadata reached the Anthropic wire")
	}
}

func TestSystemRoleInMessagesIsRefused(t *testing.T) {
	_, err := New().buildRequest(Target("claude-haiku-4-5"), provider.Request{
		Messages: []provider.Message{{Role: provider.RoleSystem, Content: []provider.Block{provider.Text{Text: "hi"}}}},
	}, true)

	var capErr *provider.CapabilityError
	if !errors.As(err, &capErr) {
		t.Fatalf("err = %v, want a CapabilityError", err)
	}
}

func TestBreakpointOutOfRangeIsRefused(t *testing.T) {
	_, err := New().buildRequest(Target("claude-haiku-4-5"), provider.Request{
		Messages: []provider.Message{provider.UserText("only one")},
		CachePlan: &provider.CachePlan{Breakpoints: []provider.Breakpoint{
			{Position: provider.CachePosition{MessageIndex: 7, BlockIndex: 0}},
		}},
	}, true)

	var capErr *provider.CapabilityError
	if !errors.As(err, &capErr) {
		t.Fatalf("err = %v, want a CapabilityError rather than a silently dropped marker", err)
	}
}

func TestTooManyBreakpointsIsRefused(t *testing.T) {
	var bps []provider.Breakpoint
	for range maxBreakpoints + 1 {
		bps = append(bps, provider.Breakpoint{Position: provider.CachePosition{MessageIndex: 0, BlockIndex: 0}})
	}
	_, err := New().buildRequest(Target("claude-haiku-4-5"), provider.Request{
		Messages:  []provider.Message{provider.UserText("hi")},
		CachePlan: &provider.CachePlan{Breakpoints: bps},
	}, true)

	var capErr *provider.CapabilityError
	if !errors.As(err, &capErr) {
		t.Fatalf("err = %v; a dropped marker is a cache miss the caller was billed to discover", err)
	}
}

// The target sells two retentions. Rounding an unsupported one to the nearer
// value would bill a rate nobody chose.
func TestUnsupportedTTLIsRefused(t *testing.T) {
	_, err := New().buildRequest(Target("claude-haiku-4-5"), provider.Request{
		Messages: []provider.Message{provider.UserText("hi")},
		CachePlan: &provider.CachePlan{Breakpoints: []provider.Breakpoint{
			{Position: provider.CachePosition{MessageIndex: 0, BlockIndex: 0}, TTL: 30 * time.Minute},
		}},
	}, true)

	var capErr *provider.CapabilityError
	if !errors.As(err, &capErr) {
		t.Fatalf("err = %v, want a CapabilityError", err)
	}
}

// "adaptive" is rejected by this model, confirmed live. Effort therefore maps to
// a token budget, and max_tokens has to clear it or the request is refused.
func TestThinkingBudgetAndCeiling(t *testing.T) {
	target := Target("claude-haiku-4-5")
	target.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: "high"}

	body, err := New().buildRequest(target, provider.Request{
		Messages: []provider.Message{provider.UserText("hi")},
	}, true)
	if err != nil {
		t.Fatal(err)
	}

	var decoded struct {
		MaxTokens int `json:"max_tokens"`
		Thinking  struct {
			Type         string `json:"type"`
			BudgetTokens int    `json:"budget_tokens"`
		} `json:"thinking"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Thinking.Type != "enabled" || decoded.Thinking.BudgetTokens != effortBudgets["high"] {
		t.Errorf("thinking = %+v", decoded.Thinking)
	}
	if decoded.MaxTokens <= decoded.Thinking.BudgetTokens {
		t.Errorf("max_tokens %d does not clear the thinking budget %d, so the request would be refused",
			decoded.MaxTokens, decoded.Thinking.BudgetTokens)
	}
}

// The current Opus and Sonnet models invert what claude-haiku-4-5 does: a
// budget is a 400 and the effort is a word on output_config. The adapter's
// catalog already offered xhigh on these targets while the budget dialect had
// no number for it, so this is the request that used to be unsendable.
func TestAdaptiveModelsTakeAnEffortWordAndNoBudget(t *testing.T) {
	for _, model := range []string{"claude-fable-5", "claude-opus-5", "claude-opus-4-8", "claude-sonnet-5"} {
		target := Target(model)
		target.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: "xhigh"}

		body, err := New().buildRequest(target, provider.Request{
			Messages: []provider.Message{provider.UserText("hi")},
		}, true)
		if err != nil {
			t.Fatalf("%s: %v", model, err)
		}

		var decoded struct {
			Thinking struct {
				Type string `json:"type"`
			} `json:"thinking"`
			OutputConfig struct {
				Effort string `json:"effort"`
			} `json:"output_config"`
		}
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatalf("%s: %v", model, err)
		}
		if decoded.Thinking.Type != "adaptive" {
			t.Errorf("%s: thinking type = %q, want adaptive", model, decoded.Thinking.Type)
		}
		if decoded.OutputConfig.Effort != "xhigh" {
			t.Errorf("%s: output_config effort = %q, want xhigh", model, decoded.OutputConfig.Effort)
		}
		// omitempty is what keeps the budget out, so assert the bytes rather
		// than the decode: a zero budget renders as a present field the server
		// refuses, and a struct comparison would not notice.
		if strings.Contains(string(body), "budget_tokens") {
			t.Errorf("%s: the request carries a budget this model rejects: %s", model, body)
		}
	}
}

// Without an effort there is nothing to say, and naming a default here would
// freeze a choice the server is free to move.
func TestAdaptiveModelWithoutEffortSendsNoOutputConfig(t *testing.T) {
	target := Target("claude-opus-5")
	target.Params.Reasoning = &provider.Reasoning{Enabled: true}

	body, err := New().buildRequest(target, provider.Request{
		Messages: []provider.Message{provider.UserText("hi")},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "output_config") {
		t.Errorf("an unasked-for effort reached the wire: %s", body)
	}
}

// A model this adapter has no adaptive evidence for keeps the budget shape,
// which is the direction a wrong guess survives in.
func TestUnlistedModelsKeepTheBudgetShape(t *testing.T) {
	target := Target("kimi-k2-thinking")
	target.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: "high"}

	body, err := New().buildRequest(target, provider.Request{
		Messages: []provider.Message{provider.UserText("hi")},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"budget_tokens"`) {
		t.Errorf("the budget shape did not survive for an unlisted model: %s", body)
	}
}

// xhigh exists only in the adaptive dialect, so the budget shape has to refuse
// it by name rather than silently pick a neighbouring number.
func TestXHighIsRefusedOnTheBudgetShape(t *testing.T) {
	target := Target("claude-haiku-4-5")
	target.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: "xhigh"}

	_, err := New().buildRequest(target, provider.Request{
		Messages: []provider.Message{provider.UserText("hi")},
	}, true)

	var capErr *provider.CapabilityError
	if !errors.As(err, &capErr) {
		t.Fatalf("err = %v, want a CapabilityError", err)
	}
}

func TestUnknownEffortIsRefused(t *testing.T) {
	target := Target("claude-haiku-4-5")
	target.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: "ludicrous"}

	_, err := New().buildRequest(target, provider.Request{
		Messages: []provider.Message{provider.UserText("hi")},
	}, true)

	var capErr *provider.CapabilityError
	if !errors.As(err, &capErr) {
		t.Fatalf("err = %v, want a CapabilityError", err)
	}
}

// The nested error shape is what the API actually returns, captured live.
func TestErrorShapeIsUnwrapped(t *testing.T) {
	body, err := os.ReadFile("testdata/error.sse")
	if err != nil {
		t.Fatal(err)
	}
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write(body)
	})

	_, streamErr := c.Stream(context.Background(), Target("claude-haiku-4-5"), provider.Request{
		Messages: []provider.Message{provider.UserText("hi")},
	})
	var apiErr *provider.APIError
	if !errors.As(streamErr, &apiErr) {
		t.Fatalf("err = %v, want an APIError", streamErr)
	}
	if !strings.Contains(apiErr.Body, "adaptive thinking is not supported") {
		t.Errorf("the server's sentence did not survive: %q", apiErr.Body)
	}
	if strings.Contains(apiErr.Body, "{") {
		t.Errorf("the caller was handed a JSON document rather than a message: %q", apiErr.Body)
	}
}

func TestTruncatedStreamIsDistinguishable(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"half a th"}}`+"\n")
	})

	s, err := c.Stream(context.Background(), Target("claude-haiku-4-5"), provider.Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	events, drainErr := drain(t, s)
	if !errors.Is(drainErr, provider.ErrStreamIncomplete) {
		t.Fatalf("err = %v, want ErrStreamIncomplete", drainErr)
	}
	if len(events) != 1 || events[0].Text != "half a th" {
		t.Errorf("partial content was lost: %+v", events)
	}
}

func TestMalformedToolArgumentsAreReported(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, strings.Join([]string{
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"before"}}`,
			`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"read","input":{}}}`,
			`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`,
			`data: {"type":"content_block_stop","index":1}`,
			`data: {"type":"message_stop"}`,
		}, "\n\n"))
	})

	s, err := c.Stream(context.Background(), Target("claude-haiku-4-5"), provider.Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	events, drainErr := drain(t, s)
	var protoErr *provider.ProtocolError
	if !errors.As(drainErr, &protoErr) {
		t.Fatalf("err = %v, want a ProtocolError", drainErr)
	}
	if len(events) == 0 || events[0].Text != "before" {
		t.Errorf("output before the malformed call was discarded: %+v", events)
	}
}

func TestTemperatureWithThinkingIsRefused(t *testing.T) {
	target := Target("claude-haiku-4-5")
	target.Params.Reasoning = &provider.Reasoning{Enabled: true}
	temp := 0.5
	target.Params.Temperature = &temp

	_, err := New().buildRequest(target, provider.Request{
		Messages: []provider.Message{provider.UserText("hi")},
	}, true)

	var capErr *provider.CapabilityError
	if !errors.As(err, &capErr) {
		t.Fatalf("err = %v; picking one for the caller would silently change what they asked for", err)
	}
}

// The counting endpoint rejects max_tokens and stream, so the counted document
// is the generated one minus those fields and has to stay that way.
func TestCountRequestOmitsGenerationFields(t *testing.T) {
	body, err := New().buildRequest(Target("claude-haiku-4-5"), provider.Request{
		Messages: []provider.Message{provider.UserText("hi")},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "max_tokens") || strings.Contains(string(body), "stream") {
		t.Errorf("the counting request carries fields the endpoint refuses:\n%s", body)
	}
}

func TestCountTokensIsExact(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"input_tokens":1234}`)
	})

	est, err := c.CountTokens(context.Background(), Target("claude-haiku-4-5"), provider.Request{
		Messages: []provider.Message{provider.UserText("hi")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if est.InputTokens != 1234 {
		t.Errorf("count = %d", est.InputTokens)
	}
	if !est.Exact {
		t.Error("a count from the server is exact; reporting it as an estimate would make a budget check widen a margin it does not need")
	}
}

func TestContextCancellationStopsStream(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"one"}}`+"\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(2 * time.Second)
	})

	ctx, cancel := context.WithCancel(context.Background())
	s, err := c.Stream(ctx, Target("claude-haiku-4-5"), provider.Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.Next(); err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := s.Next(); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// Models is what a picker offers for a surface whose model ids cannot be
// guessed — a plan endpoint's above all, where the names are the vendor's and
// appear in no catalog.
func TestModelsListsTheAccountsModels(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/models") {
			t.Errorf("asked for %s, want /v1/models", r.URL.Path)
		}
		io.WriteString(w, `{"data":[{"id":"k3-256k"},{"id":"k3-turbo"}]}`)
	})
	names, err := c.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "k3-256k" {
		t.Fatalf("Models() = %v, want the two ids the server listed", names)
	}
}
