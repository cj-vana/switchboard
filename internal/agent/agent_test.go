package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cj-vana/switchboard/internal/execution"
	"github.com/cj-vana/switchboard/internal/permission"
	"github.com/cj-vana/switchboard/internal/provider"
	"github.com/cj-vana/switchboard/internal/session"
	"github.com/cj-vana/switchboard/internal/tools"
)

// scriptTurn is one canned model call.
type scriptTurn struct {
	startErr error
	events   []provider.Event
	endErr   error // returned in place of io.EOF once events run out
}

type scriptedProvider struct {
	turns    []scriptTurn
	calls    int
	requests []provider.Request
}

func (p *scriptedProvider) Name() string { return "scripted" }

func (p *scriptedProvider) Stream(_ context.Context, _ provider.RouteTarget, req provider.Request) (provider.EventStream, error) {
	p.requests = append(p.requests, req)
	if p.calls >= len(p.turns) {
		return nil, errors.New("scripted provider ran out of turns")
	}
	turn := p.turns[p.calls]
	p.calls++
	if turn.startErr != nil {
		return nil, turn.startErr
	}
	return &scriptedStream{turn: turn}, nil
}

func (p *scriptedProvider) CountTokens(context.Context, provider.RouteTarget, provider.Request) (provider.TokenEstimate, error) {
	return provider.TokenEstimate{}, nil
}

func (p *scriptedProvider) Probe(context.Context, provider.RouteTarget) (provider.ProbeResult, error) {
	return provider.ProbeResult{Reachable: true, ModelPresent: true}, nil
}

type scriptedStream struct {
	turn scriptTurn
	i    int
}

func (s *scriptedStream) Next() (provider.Event, error) {
	if s.i < len(s.turn.events) {
		ev := s.turn.events[s.i]
		s.i++
		return ev, nil
	}
	if s.turn.endErr != nil {
		return provider.Event{}, s.turn.endErr
	}
	return provider.Event{}, io.EOF
}

func (s *scriptedStream) Close() error { return nil }

func textTurn(text string) scriptTurn {
	return scriptTurn{events: []provider.Event{
		{Type: provider.EventTextDelta, Index: 0, Text: text},
		{Type: provider.EventDone, StopReason: provider.StopEndTurn, Usage: provider.Usage{InputTokens: 10, OutputTokens: 5}},
	}}
}

func toolTurn(uses ...provider.ToolUse) scriptTurn {
	turn := scriptTurn{}
	for i, use := range uses {
		turn.events = append(turn.events, provider.Event{
			Type: provider.EventToolUse, Index: i, ToolUse: &use,
		})
	}
	turn.events = append(turn.events, provider.Event{
		Type: provider.EventDone, StopReason: provider.StopToolUse,
		Usage: provider.Usage{InputTokens: 20, OutputTokens: 8},
	})
	return turn
}

func use(id, name string, input string) provider.ToolUse {
	return provider.ToolUse{ID: id, Name: name, Input: json.RawMessage(input)}
}

type recordingObserver struct {
	text     strings.Builder
	thinking strings.Builder
	notices  []string
	toolEnds []string

	// The loop runs a turn's tool calls in parallel, so an observer that
	// appends without guarding races. This one did, which is the same mistake
	// the production detector made and the reason -race is worth running.
	mu sync.Mutex
}

func (o *recordingObserver) ThinkingDelta(s string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.thinking.WriteString(s)
}

func (o *recordingObserver) TextDelta(s string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.text.WriteString(s)
}

func (o *recordingObserver) ToolStart(string, permission.Request) {}

func (o *recordingObserver) ToolEnd(name string, res tools.Result, _ time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.toolEnds = append(o.toolEnds, name)
}

func (o *recordingObserver) Notice(level, text string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.notices = append(o.notices, level+": "+text)
}
func (o *recordingObserver) TurnUsage(session.Usage) {}

type autoAsker struct {
	approve bool
	calls   int
}

func (a *autoAsker) Ask(context.Context, permission.Request, permission.Outcome) (permission.Response, error) {
	a.calls++
	return permission.Response{Approved: a.approve}, nil
}

type harness struct {
	loop     *Loop
	provider *scriptedProvider
	obs      *recordingObserver
	asker    *autoAsker
	sess     *session.Session
	root     string
}

func newHarness(t *testing.T, mode permission.Mode, turns ...scriptTurn) *harness {
	t.Helper()

	root := t.TempDir()
	registry, err := tools.NewRegistry(root, execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.Create(root, "scripted/local/test", "test-revision")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })

	p := &scriptedProvider{turns: turns}
	obs := &recordingObserver{}
	asker := &autoAsker{approve: true}

	return &harness{
		loop: &Loop{
			Provider: p,
			Target:   provider.RouteTarget{Provider: "scripted", Surface: "local", ModelID: "test"},
			Tools:    registry,
			Perms:    permission.NewEngine(mode, execution.Capability{}),
			Asker:    asker,
			Session:  sess,
			Observer: obs,
		},
		provider: p,
		obs:      obs,
		asker:    asker,
		sess:     sess,
		root:     registry.Root(),
	}
}

func (h *harness) messages() []provider.Message { return h.sess.State().Messages }

func TestTurnWithoutTools(t *testing.T) {
	h := newHarness(t, permission.ModeDefault, textTurn("all done"))

	if err := h.loop.Turn(context.Background(), "say something"); err != nil {
		t.Fatal(err)
	}

	msgs := h.messages()
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want user + assistant", len(msgs))
	}
	if msgs[0].Role != provider.RoleUser || msgs[1].Role != provider.RoleAssistant {
		t.Errorf("roles = %s, %s", msgs[0].Role, msgs[1].Role)
	}
	if msgs[1].Text() != "all done" {
		t.Errorf("assistant text = %q", msgs[1].Text())
	}
	if h.obs.text.String() != "all done" {
		t.Errorf("observer saw %q", h.obs.text.String())
	}
	if got := h.sess.State().Usage; got.InputTokens != 10 || got.OutputTokens != 5 {
		t.Errorf("usage = %+v", got)
	}
}

func TestToolRoundTrip(t *testing.T) {
	h := newHarness(t, permission.ModeDefault,
		toolTurn(use("call_1", "read", `{"path":"hello.txt"}`)),
		textTurn("the file says hi"),
	)
	if err := os.WriteFile(filepath.Join(h.root, "hello.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := h.loop.Turn(context.Background(), "read hello.txt"); err != nil {
		t.Fatal(err)
	}

	msgs := h.messages()
	if len(msgs) != 4 {
		t.Fatalf("got %d messages, want user, assistant, tool, assistant", len(msgs))
	}
	if msgs[2].Role != provider.RoleTool {
		t.Fatalf("message 2 role = %s, want tool", msgs[2].Role)
	}
	result, ok := msgs[2].Content[0].(provider.ToolResult)
	if !ok {
		t.Fatalf("tool message holds a %s block", msgs[2].Content[0].Kind())
	}
	if result.ToolUseID != "call_1" || result.Content != "hi" || result.IsError {
		t.Errorf("result = %+v", result)
	}

	// The second request must carry the whole conversation so far.
	if len(h.provider.requests) != 2 {
		t.Fatalf("provider called %d times, want 2", len(h.provider.requests))
	}
	if n := len(h.provider.requests[1].Messages); n != 3 {
		t.Errorf("second request carried %d messages, want 3", n)
	}
}

// Every tool call needs exactly one result, in call order. A conversation where
// they do not line up is malformed, and every later request inherits it.
func TestEveryCallGetsExactlyOneResultInOrder(t *testing.T) {
	h := newHarness(t, permission.ModeDefault,
		toolTurn(
			use("call_1", "read", `{"path":"a.txt"}`),
			use("call_2", "read", `{"path":"missing.txt"}`),
			use("call_3", "nosuchtool", `{}`),
			use("call_4", "read", `{"path":"b.txt"}`),
		),
		textTurn("done"),
	)
	os.WriteFile(filepath.Join(h.root, "a.txt"), []byte("alpha"), 0o644)
	os.WriteFile(filepath.Join(h.root, "b.txt"), []byte("bravo"), 0o644)

	if err := h.loop.Turn(context.Background(), "read some files"); err != nil {
		t.Fatal(err)
	}

	toolMsg := h.messages()[2]
	if len(toolMsg.Content) != 4 {
		t.Fatalf("got %d results for 4 calls", len(toolMsg.Content))
	}
	for i, want := range []string{"call_1", "call_2", "call_3", "call_4"} {
		got := toolMsg.Content[i].(provider.ToolResult)
		if got.ToolUseID != want {
			t.Errorf("result %d is for %s, want %s", i, got.ToolUseID, want)
		}
	}
	if r := toolMsg.Content[0].(provider.ToolResult); r.Content != "alpha" || r.IsError {
		t.Errorf("call_1 = %+v", r)
	}
	if r := toolMsg.Content[1].(provider.ToolResult); !r.IsError {
		t.Error("a missing file should come back as a tool error")
	}
	if r := toolMsg.Content[2].(provider.ToolResult); !r.IsError || !strings.Contains(r.Content, "nosuchtool") {
		t.Errorf("unknown tool = %+v", r)
	}
}

func TestMalformedToolArgumentsGoBackToTheModel(t *testing.T) {
	h := newHarness(t, permission.ModeDefault,
		toolTurn(use("call_1", "read", `{"path": 12345}`)),
		textTurn("let me try again"),
	)

	// A bad argument blob is the model's mistake to fix, not a reason to throw
	// away the turn (§10.3).
	if err := h.loop.Turn(context.Background(), "read something"); err != nil {
		t.Fatalf("a malformed argument must not abort the turn: %v", err)
	}
	res := h.messages()[2].Content[0].(provider.ToolResult)
	if !res.IsError {
		t.Error("the model should have been told its arguments were wrong")
	}
	if h.provider.calls != 2 {
		t.Errorf("provider called %d times; the model never got a chance to correct itself", h.provider.calls)
	}
}

func TestDeniedCallIsReportedNotFatal(t *testing.T) {
	h := newHarness(t, permission.ModeDefault,
		toolTurn(use("call_1", "exec", `{"command":["rm","-rf","/"]}`)),
		textTurn("understood, I will not"),
	)
	h.asker.approve = false

	if err := h.loop.Turn(context.Background(), "clean up"); err != nil {
		t.Fatal(err)
	}
	res := h.messages()[2].Content[0].(provider.ToolResult)
	if !res.IsError || !strings.Contains(res.Content, "did not approve") {
		t.Errorf("result = %+v", res)
	}
	if h.asker.calls != 1 {
		t.Errorf("asker called %d times, want 1", h.asker.calls)
	}
}

func TestPlanModeRefusesWithoutPrompting(t *testing.T) {
	h := newHarness(t, permission.ModePlan,
		toolTurn(use("call_1", "write", `{"path":"new.txt","content":"x"}`)),
		textTurn("I cannot write in plan mode"),
	)

	if err := h.loop.Turn(context.Background(), "make a file"); err != nil {
		t.Fatal(err)
	}
	if h.asker.calls != 0 {
		t.Error("plan mode denies outright; it must not prompt")
	}
	if _, err := os.Stat(filepath.Join(h.root, "new.txt")); err == nil {
		t.Error("plan mode wrote a file")
	}
}

func TestRetryOnTransientFailure(t *testing.T) {
	h := newHarness(t, permission.ModeDefault,
		scriptTurn{startErr: &provider.APIError{Provider: "scripted", StatusCode: 503}},
		textTurn("recovered"),
	)
	h.loop.MaxAttempts = 3

	if err := h.loop.Turn(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if h.provider.calls != 2 {
		t.Errorf("provider called %d times, want a retry", h.provider.calls)
	}
	if len(h.obs.notices) == 0 {
		t.Error("a retry must be visible to the user, not silent")
	}
}

func TestPermanentFailureIsNotRetried(t *testing.T) {
	h := newHarness(t, permission.ModeDefault,
		scriptTurn{startErr: &provider.APIError{Provider: "scripted", StatusCode: 404, Body: "model not found"}},
	)
	h.loop.MaxAttempts = 3

	err := h.loop.Turn(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected the turn to fail")
	}
	if h.provider.calls != 1 {
		t.Errorf("provider called %d times; a 404 will not fix itself", h.provider.calls)
	}
}

func TestMalformedStreamAbortsWithoutRetrying(t *testing.T) {
	h := newHarness(t, permission.ModeDefault, scriptTurn{
		events: []provider.Event{{Type: provider.EventTextDelta, Text: "partial"}},
		endErr: &provider.ProtocolError{Provider: "scripted", Detail: "unknown block"},
	})
	h.loop.MaxAttempts = 3

	err := h.loop.Turn(context.Background(), "hello")
	var protoErr *provider.ProtocolError
	if !errors.As(err, &protoErr) {
		t.Fatalf("err = %v, want a ProtocolError", err)
	}
	if h.provider.calls != 1 {
		t.Errorf("provider called %d times; re-issuing produces the same malformed shape", h.provider.calls)
	}
}

// A stream that drops leaves real output. It is kept, marked incomplete, and
// never replayed as a finished turn.
func TestExhaustedRetriesRecordAnIncompleteMessage(t *testing.T) {
	dropped := scriptTurn{
		events: []provider.Event{{Type: provider.EventTextDelta, Text: "half a thou"}},
		endErr: provider.ErrStreamIncomplete,
	}
	h := newHarness(t, permission.ModeDefault, dropped, dropped)
	h.loop.MaxAttempts = 2

	err := h.loop.Turn(context.Background(), "hello")
	if !errors.Is(err, provider.ErrStreamIncomplete) {
		t.Fatalf("err = %v", err)
	}
	if h.provider.calls != 2 {
		t.Errorf("provider called %d times, want 2 attempts", h.provider.calls)
	}

	msgs := h.messages()
	last := msgs[len(msgs)-1]
	if !last.Incomplete {
		t.Fatalf("the partial turn was not marked incomplete: %+v", last)
	}
	if last.Text() != "half a thou" {
		t.Errorf("partial content = %q, want what actually arrived", last.Text())
	}
}

func TestRoundLimit(t *testing.T) {
	var turns []scriptTurn
	for range 5 {
		turns = append(turns, toolTurn(use("call_x", "read", `{"path":"loop.txt"}`)))
	}
	h := newHarness(t, permission.ModeDefault, turns...)
	os.WriteFile(filepath.Join(h.root, "loop.txt"), []byte("x"), 0o644)
	h.loop.MaxToolRounds = 3

	err := h.loop.Turn(context.Background(), "go forever")
	if !errors.Is(err, ErrRoundLimit) {
		t.Fatalf("err = %v, want ErrRoundLimit", err)
	}
	if h.provider.calls != 3 {
		t.Errorf("provider called %d times, want the 3 round limit", h.provider.calls)
	}
}

func TestCancellationStillPairsResultsWithCalls(t *testing.T) {
	h := newHarness(t, permission.ModeDefault,
		toolTurn(
			use("call_1", "exec", `{"command":["sleep","10"],"timeout_seconds":30}`),
			use("call_2", "exec", `{"command":["echo","never"]}`),
		),
	)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	err := h.loop.Turn(ctx, "run things")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	msgs := h.messages()
	toolMsg := msgs[len(msgs)-1]
	if toolMsg.Role != provider.RoleTool {
		t.Fatalf("last message role = %s, want the tool results", toolMsg.Role)
	}
	if len(toolMsg.Content) != 2 {
		t.Fatalf("got %d results for 2 calls; a cancelled turn must still pair them", len(toolMsg.Content))
	}
	second := toolMsg.Content[1].(provider.ToolResult)
	if !second.IsError || !strings.Contains(second.Content, "cancelled") {
		t.Errorf("the unrun call should say it never ran: %+v", second)
	}
}

func TestSessionResumesAfterAFailedTurn(t *testing.T) {
	h := newHarness(t, permission.ModeDefault,
		scriptTurn{startErr: &provider.APIError{StatusCode: 500}},
		scriptTurn{startErr: &provider.APIError{StatusCode: 500}},
		scriptTurn{startErr: &provider.APIError{StatusCode: 500}},
	)
	h.loop.MaxAttempts = 3

	if err := h.loop.Turn(context.Background(), "hello"); err == nil {
		t.Fatal("expected failure")
	}
	// The user's message survives, so resuming shows what was asked.
	msgs := h.messages()
	if len(msgs) != 1 || msgs[0].Text() != "hello" {
		t.Errorf("messages = %+v", msgs)
	}
}

func TestSystemPromptIsStableWithinASession(t *testing.T) {
	capability := execution.Capability{Platform: "darwin"}
	first := SystemPrompt("/work", permission.ModeDefault, capability)
	second := SystemPrompt("/work", permission.ModeDefault, capability)

	// The system prompt sits in the frozen zone. If it varied per call, every
	// request would invalidate the cached prefix (§6.1).
	if first[0].(provider.Text).Text != second[0].(provider.Text).Text {
		t.Error("the system prompt is not stable between calls")
	}
	if !strings.Contains(first[0].(provider.Text).Text, "no verified sandbox") {
		t.Error("the model should be told that each command needs approval")
	}
}

// TestInjectLandsBetweenRoundsOnly pins the injection seam: nothing on the
// opening round, where the previous message is the user's own prompt and a
// second user message would be adjacent to it; delivery after tool results,
// where a user-role message is legal everywhere.
func TestInjectLandsBetweenRoundsOnly(t *testing.T) {
	h := newHarness(t, permission.ModeDefault,
		toolTurn(use("call_1", "read", `{"path":"hello.txt"}`)),
		textTurn("done"),
	)
	if err := os.WriteFile(filepath.Join(h.root, "hello.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	injections := 0
	pending := []provider.Message{provider.UserText("[advisor] check the error message first")}
	h.loop.Inject = func() []provider.Message {
		injections++
		out := pending
		pending = nil
		return out
	}

	if err := h.loop.Turn(context.Background(), "read hello.txt"); err != nil {
		t.Fatal(err)
	}

	// Round 0 must not have drained: the first drain happens on round 1, so
	// Inject was consulted exactly once for a two-round turn.
	if injections != 1 {
		t.Fatalf("Inject consulted %d times over two rounds, want 1 (never on the opening round)", injections)
	}

	msgs := h.messages()
	// user, assistant(tool use), tool results, injected user, assistant.
	if len(msgs) != 5 {
		t.Fatalf("got %d messages, want 5: %+v", len(msgs), msgs)
	}
	if msgs[2].Role != provider.RoleTool {
		t.Fatalf("message 2 is %s, want the tool results", msgs[2].Role)
	}
	if msgs[3].Role != provider.RoleUser || !strings.Contains(msgs[3].Text(), "[advisor]") {
		t.Fatalf("message 3 should be the injected advice, got %s %q", msgs[3].Role, msgs[3].Text())
	}
	if msgs[0].Role != provider.RoleUser || msgs[1].Role != provider.RoleAssistant {
		t.Fatal("the opening round's shape changed")
	}
}
