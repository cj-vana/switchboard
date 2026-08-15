package advisor

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cj-vana/switchboard/internal/agent"
	"github.com/cj-vana/switchboard/internal/permission"
	"github.com/cj-vana/switchboard/internal/provider"
	"github.com/cj-vana/switchboard/internal/tools"
)

// scriptedProvider answers every consult with a fixed text, and records what
// it was asked.
type scriptedProvider struct {
	mu      sync.Mutex
	answer  string
	prompts []string
}

func (p *scriptedProvider) Name() string { return "scripted" }

func (p *scriptedProvider) Stream(_ context.Context, _ provider.RouteTarget, req provider.Request) (provider.EventStream, error) {
	p.mu.Lock()
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if t, ok := b.(provider.Text); ok {
				p.prompts = append(p.prompts, t.Text)
			}
		}
	}
	answer := p.answer
	p.mu.Unlock()
	return &scriptedStream{events: []provider.Event{
		{Type: provider.EventTextDelta, Text: answer},
		{Type: provider.EventDone, StopReason: provider.StopEndTurn},
	}}, nil
}

func (p *scriptedProvider) CountTokens(context.Context, provider.RouteTarget, provider.Request) (provider.TokenEstimate, error) {
	return provider.TokenEstimate{}, nil
}

func (p *scriptedProvider) Probe(context.Context, provider.RouteTarget) (provider.ProbeResult, error) {
	return provider.ProbeResult{}, nil
}

type scriptedStream struct{ events []provider.Event }

func (s *scriptedStream) Next() (provider.Event, error) {
	if len(s.events) == 0 {
		return provider.Event{}, provider.ErrStreamIncomplete
	}
	ev := s.events[0]
	s.events = s.events[1:]
	return ev, nil
}
func (s *scriptedStream) Close() error { return nil }

func target() provider.RouteTarget {
	return provider.RouteTarget{Provider: "scripted", Surface: "test", ModelID: "adv"}
}

// repeatCall feeds the advisor the same failing command until the detector's
// loop trigger fires.
func repeatCall(a *Advisor, times int) {
	req := permission.Request{Tool: "exec", Argv: []string{"go", "test", "./..."}}
	for range times {
		a.ToolStart("exec", req)
		a.ToolEnd("exec", tools.Result{Content: "FAIL: TestX (0.01s)", IsError: true}, time.Second)
	}
}

func waitAdvice(t *testing.T, ch chan string) string {
	t.Helper()
	select {
	case advice := <-ch:
		return advice
	case <-time.After(5 * time.Second):
		t.Fatal("no advice arrived")
		return ""
	}
}

func TestTroubleTriggersAConsultAndAdviceQueues(t *testing.T) {
	p := &scriptedProvider{answer: "Stop rerunning the whole suite; run the one failing test and read its output."}
	got := make(chan string, 4)
	a := New(agent.NopObserver{}, p, target(), func(text string) { got <- text })

	a.StartTurn("fix the flaky test")
	repeatCall(a, 4)

	advice := waitAdvice(t, got)
	if !strings.Contains(advice, "one failing test") {
		t.Fatalf("unexpected advice: %q", advice)
	}

	msgs := a.Drain()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 queued injection, got %d", len(msgs))
	}
	text := msgs[0].Content[0].(provider.Text).Text
	if !strings.HasPrefix(text, "[advisor]") {
		t.Fatalf("injected advice must be labelled as advice, got %q", text)
	}
	if a.Drain() != nil {
		t.Fatal("Drain must clear the queue")
	}

	// The consult saw the task and the evidence, not nothing.
	p.mu.Lock()
	defer p.mu.Unlock()
	joined := strings.Join(p.prompts, "\n")
	for _, want := range []string{"fix the flaky test", "go test"} {
		if !strings.Contains(joined, want) {
			t.Errorf("consult prompt is missing %q", want)
		}
	}
}

func TestConsultBudgetHolds(t *testing.T) {
	p := &scriptedProvider{answer: "advice"}
	got := make(chan string, 16)
	a := New(agent.NopObserver{}, p, target(), func(text string) { got <- text },
		WithBounds(1, time.Nanosecond))

	a.StartTurn("task")
	repeatCall(a, 12)
	waitAdvice(t, got)

	select {
	case extra := <-got:
		t.Fatalf("the one-consult budget produced a second consult: %q", extra)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestNoneMeansSilence(t *testing.T) {
	p := &scriptedProvider{answer: "NONE"}
	got := make(chan string, 4)
	a := New(agent.NopObserver{}, p, target(), func(text string) { got <- text })

	a.StartTurn("task")
	repeatCall(a, 4)

	select {
	case advice := <-got:
		t.Fatalf("NONE should produce no advice, got %q", advice)
	case <-time.After(500 * time.Millisecond):
	}
	if a.Drain() != nil {
		t.Fatal("NONE queued an injection anyway")
	}
}
