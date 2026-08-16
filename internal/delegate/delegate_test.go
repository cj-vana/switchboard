package delegate

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/cj-vana/switchboard/internal/agent"
	"github.com/cj-vana/switchboard/internal/config"
	"github.com/cj-vana/switchboard/internal/execution"
	"github.com/cj-vana/switchboard/internal/permission"
	"github.com/cj-vana/switchboard/internal/provider"
	"github.com/cj-vana/switchboard/internal/session"
	"github.com/cj-vana/switchboard/internal/tools"
)

// oneTurnProvider streams a single text answer and stops.
type oneTurnProvider struct{ text string }

func (p *oneTurnProvider) Name() string { return "scripted" }

func (p *oneTurnProvider) Stream(context.Context, provider.RouteTarget, provider.Request) (provider.EventStream, error) {
	return &oneTurnStream{events: []provider.Event{
		{Type: provider.EventTextDelta, Index: 0, Text: p.text},
		{Type: provider.EventDone, StopReason: provider.StopEndTurn, Usage: provider.Usage{InputTokens: 10, OutputTokens: 5}},
	}}, nil
}

func (p *oneTurnProvider) CountTokens(context.Context, provider.RouteTarget, provider.Request) (provider.TokenEstimate, error) {
	return provider.TokenEstimate{}, nil
}

func (p *oneTurnProvider) Probe(context.Context, provider.RouteTarget) (provider.ProbeResult, error) {
	return provider.ProbeResult{Reachable: true, ModelPresent: true}, nil
}

type oneTurnStream struct {
	events []provider.Event
	i      int
}

func (s *oneTurnStream) Next() (provider.Event, error) {
	if s.i < len(s.events) {
		ev := s.events[s.i]
		s.i++
		return ev, nil
	}
	return provider.Event{}, io.EOF
}

func (s *oneTurnStream) Close() error { return nil }

func ladder() []config.Tier {
	return []config.Tier{
		{ID: "t1", Label: "light", Target: provider.RouteTarget{Provider: "scripted", Surface: "local", ModelID: "small"}},
		{ID: "t2", Label: "deep", Target: provider.RouteTarget{Provider: "scripted", Surface: "local", ModelID: "big"}},
	}
}

func testConfig(t *testing.T, answer string) Config {
	t.Helper()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()

	return Config{
		Tiers: ladder(),
		Probe: func(_ context.Context, tierID string) (config.Tier, provider.Provider, error) {
			for _, tier := range ladder() {
				if tier.ID == tierID {
					return tier, &oneTurnProvider{text: answer}, nil
				}
			}
			t.Fatalf("probe asked for unknown tier %s", tierID)
			return config.Tier{}, nil, nil
		},
		NewSession: func(target provider.RouteTargetID) (*session.Session, error) {
			return store.Create(workspace, target, "test-revision")
		},
		NewLoop: func(tier config.Tier, client provider.Provider, sess *session.Session, obs agent.Observer, named *Agent) (*agent.Loop, error) {
			registry, err := tools.NewRegistry(workspace, execution.Capability{})
			if err != nil {
				return nil, err
			}
			if named != nil && len(named.Tools) > 0 {
				if err := registry.Restrict(named.Tools); err != nil {
					return nil, err
				}
			}
			return &agent.Loop{
				Provider:      client,
				Target:        tier.Target,
				Tools:         registry,
				Perms:         permission.NewEngine(permission.ModeDefault, execution.Capability{}),
				Session:       sess,
				System:        []provider.Block{provider.Text{Text: Preamble}},
				Observer:      obs,
				MaxToolRounds: MaxRounds,
			}, nil
		},
	}
}

func plan(t *testing.T, tool tools.Tool, input string) tools.Plan {
	t.Helper()
	p, err := tool.Plan(json.RawMessage(input))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNewRequiresALadderAndWiring(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("an empty ladder must be an error")
	}
	if _, err := New(Config{Tiers: ladder()}); err == nil {
		t.Error("missing wiring must be an error")
	}
}

func TestPlanValidatesTaskAndTier(t *testing.T) {
	tool, err := New(testConfig(t, "ok"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := tool.Plan(json.RawMessage(`{"task":"  "}`)); err == nil {
		t.Error("an empty task must fail at Plan time")
	}
	if _, err := tool.Plan(json.RawMessage(`{"task":"x","tier":"t9"}`)); err == nil {
		t.Error("a tier outside the ladder must fail at Plan time")
	}

	p := plan(t, tool, `{"task":"find the flaky test"}`)
	if p.Request.Effect != permission.EffectRead {
		t.Errorf("spawning carries effect %q, want read: each sub call is gated on its own", p.Request.Effect)
	}
	if !strings.HasPrefix(p.Request.Detail, "t1 → ") {
		t.Errorf("Detail = %q, want the default bottom rung named", p.Request.Detail)
	}
}

func TestRunReturnsTheFinalAnswerWithATrailer(t *testing.T) {
	tool, err := New(testConfig(t, "The flaky test is TestRetry; it races on port 8080."))
	if err != nil {
		t.Fatal(err)
	}

	p := plan(t, tool, `{"task":"find the flaky test","tier":"t2"}`)
	res, err := p.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("run failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "TestRetry") {
		t.Errorf("content = %q, want the subagent's answer", res.Content)
	}
	if !strings.Contains(res.Content, "[delegate on t2:") {
		t.Errorf("content = %q, want the trailer naming the rung it ran on", res.Content)
	}
}

func TestForwardingFiltersWhatWouldMislead(t *testing.T) {
	rec := &recorder{}
	f := &forwarding{parent: rec}

	f.TextDelta("streamed text")
	f.ThinkingDelta("thoughts")
	f.TurnUsage(session.Usage{})
	f.ToolStart("todo", permission.Request{Tool: "todo"})
	f.ToolEnd("todo", tools.Result{}, time.Millisecond)
	f.ToolStart("grep", permission.Request{Tool: "grep"})
	f.ToolEnd("grep", tools.Result{Content: "hit"}, time.Millisecond)
	f.Notice("warn", "retrying")

	if len(rec.starts) != 1 || rec.starts[0] != "grep" {
		t.Errorf("starts = %v, want grep only: a sub todo would collide with the primary's", rec.starts)
	}
	if len(rec.ends) != 1 || rec.ends[0] != "grep" {
		t.Errorf("ends = %v", rec.ends)
	}
	if len(rec.notices) != 1 {
		t.Errorf("notices = %v, want the retry surfaced", rec.notices)
	}
}

func TestFinalTextSkipsIncompleteMessages(t *testing.T) {
	state := session.State{Messages: []provider.Message{
		{Role: provider.RoleAssistant, Content: []provider.Block{provider.Text{Text: "the real answer"}}},
		{Role: provider.RoleAssistant, Content: []provider.Block{provider.Text{Text: "interrupted"}}, Incomplete: true},
	}}
	if got := finalText(state); got != "the real answer" {
		t.Errorf("finalText = %q", got)
	}
}

// TestNoAgentsLeavesTheToolByteIdentical guards the frozen zone: a session
// with no definitions must render the same schema and description it always
// has, or every existing session's cached prefix breaks on upgrade.
func TestNoAgentsLeavesTheToolByteIdentical(t *testing.T) {
	tool, err := New(testConfig(t, "ok"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(tool.Schema()), `"agent"`) {
		t.Error("the bare schema must not carry an agent property")
	}
	if strings.Contains(tool.Description(), "Named agents") {
		t.Error("the bare description must not enumerate agents")
	}
}

func agentConfig(t *testing.T, answer string) Config {
	t.Helper()
	c := testConfig(t, answer)
	c.Agents = []Agent{
		{Name: "reviewer", Description: "reviews a diff", Tier: "t2", Tools: []string{"read", "grep"}, Prompt: "You review changes."},
		{Name: "scout", Description: "finds things", Prompt: "You search."},
	}
	return c
}

func TestAgentsAppearInSchemaAndDescription(t *testing.T) {
	tool, err := New(agentConfig(t, "ok"))
	if err != nil {
		t.Fatal(err)
	}
	schema := string(tool.Schema())
	if !strings.Contains(schema, `"enum": ["reviewer","scout"]`) {
		t.Errorf("schema = %s, want the agent names enumerated", schema)
	}
	desc := tool.Description()
	if !strings.Contains(desc, "reviewer: reviews a diff (runs on t2)") {
		t.Errorf("description = %q, want each agent's charter and rung", desc)
	}
}

func TestPlanResolvesTheAgentAndItsDefaultRung(t *testing.T) {
	tool, err := New(agentConfig(t, "ok"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := tool.Plan(json.RawMessage(`{"task":"x","agent":"nobody"}`)); err == nil {
		t.Error("an undefined agent must fail at Plan time")
	}

	p := plan(t, tool, `{"task":"check the diff","agent":"reviewer"}`)
	if !strings.HasPrefix(p.Request.Detail, "reviewer on t2 → ") {
		t.Errorf("Detail = %q, want the agent's default rung", p.Request.Detail)
	}

	p = plan(t, tool, `{"task":"check the diff","agent":"reviewer","tier":"t1"}`)
	if !strings.HasPrefix(p.Request.Detail, "reviewer on t1 → ") {
		t.Errorf("Detail = %q, want the explicit tier to win", p.Request.Detail)
	}

	p = plan(t, tool, `{"task":"look around","agent":"scout"}`)
	if !strings.HasPrefix(p.Request.Detail, "scout on t1 → ") {
		t.Errorf("Detail = %q, want a rungless agent on the bottom", p.Request.Detail)
	}
}

func TestRunNamesTheAgentInTheTrailer(t *testing.T) {
	tool, err := New(agentConfig(t, "Looks correct."))
	if err != nil {
		t.Fatal(err)
	}
	p := plan(t, tool, `{"task":"check the diff","agent":"reviewer"}`)
	res, err := p.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("run failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "[delegate reviewer on t2:") {
		t.Errorf("content = %q, want the trailer naming who ran", res.Content)
	}
}

type recorder struct {
	starts, ends, notices []string
}

func (r *recorder) ThinkingDelta(string) {}
func (r *recorder) TextDelta(string)     {}
func (r *recorder) ToolStart(name string, _ permission.Request) {
	r.starts = append(r.starts, name)
}
func (r *recorder) ToolEnd(name string, _ tools.Result, _ time.Duration) {
	r.ends = append(r.ends, name)
}
func (r *recorder) Notice(_, text string)   { r.notices = append(r.notices, text) }
func (r *recorder) TurnUsage(session.Usage) {}
