package advisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/tools"
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

type blockingProvider struct {
	entered chan struct{}
	release chan struct{}
}

func (p *blockingProvider) Name() string { return "blocking" }
func (p *blockingProvider) Stream(context.Context, provider.RouteTarget, provider.Request) (provider.EventStream, error) {
	close(p.entered)
	<-p.release
	return &scriptedStream{events: []provider.Event{
		{Type: provider.EventTextDelta, Text: "advice"},
		{Type: provider.EventDone},
	}}, nil
}
func (p *blockingProvider) CountTokens(context.Context, provider.RouteTarget, provider.Request) (provider.TokenEstimate, error) {
	return provider.TokenEstimate{}, nil
}
func (p *blockingProvider) Probe(context.Context, provider.RouteTarget) (provider.ProbeResult, error) {
	return provider.ProbeResult{}, nil
}

type meterOrderProvider struct {
	beforeStream func() error
	usage        provider.Usage
	streamErr    error
}

func (p *meterOrderProvider) Name() string { return "meter-order" }
func (p *meterOrderProvider) Stream(context.Context, provider.RouteTarget, provider.Request) (provider.EventStream, error) {
	if p.beforeStream != nil {
		if err := p.beforeStream(); err != nil {
			return nil, err
		}
	}
	if p.streamErr != nil {
		return nil, p.streamErr
	}
	return &scriptedStream{events: []provider.Event{
		{Type: provider.EventTextDelta, Text: "use the focused test"},
		{Type: provider.EventDone, Usage: p.usage},
	}}, nil
}
func (p *meterOrderProvider) CountTokens(context.Context, provider.RouteTarget, provider.Request) (provider.TokenEstimate, error) {
	return provider.TokenEstimate{}, nil
}
func (p *meterOrderProvider) Probe(context.Context, provider.RouteTarget) (provider.ProbeResult, error) {
	return provider.ProbeResult{}, nil
}

func target() provider.RouteTarget {
	return provider.RouteTarget{Provider: "scripted", Surface: "test", ModelID: "adv"}
}

// repeatCall feeds the advisor the same failing command until the detector's
// loop trigger fires.
func repeatCall(a *Advisor, times int) {
	req := permission.Request{Tool: "exec", Argv: []string{"go", "test", "./..."}}
	for i := range times {
		call := provider.ToolUse{ID: fmt.Sprintf("call-%d", i), Name: "exec", Input: json.RawMessage(`{"argv":["go","test","./..."]}`)}
		a.ToolStart(call, req)
		a.ToolEnd(call, req, tools.Result{Content: "FAIL: TestX (0.01s)", IsError: true}, time.Second)
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

func TestPauseWaitsForInflightConsultBeforeSessionSnapshot(t *testing.T) {
	p := &blockingProvider{entered: make(chan struct{}), release: make(chan struct{})}
	a := New(agent.NopObserver{}, p, target(), nil, WithBounds(2, time.Nanosecond))
	a.StartTurn("task")
	a.maybeConsult("failure spike")
	select {
	case <-p.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("advisor provider call did not start")
	}

	waited := make(chan error, 1)
	go func() { waited <- a.PauseAndWait(context.Background()) }()
	select {
	case err := <-waited:
		t.Fatalf("PauseAndWait returned before admitted consult settled: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(p.release)
	select {
	case err := <-waited:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PauseAndWait did not unblock after consult settled")
	}

	// While paused, even a fresh trigger cannot enter a provider call.
	a.mu.Lock()
	a.lastConsult = time.Time{}
	a.mu.Unlock()
	a.maybeConsult("another failure")
	time.Sleep(30 * time.Millisecond)
	a.mu.Lock()
	inflight := a.inflight
	a.mu.Unlock()
	if inflight {
		t.Fatal("paused advisor admitted a new consult")
	}
	a.Resume()
}

func TestConsultMetersBeforeProviderAndSettlesDoneUsage(t *testing.T) {
	began := false
	settled := false
	wantUsage := provider.Usage{InputTokens: 12, OutputTokens: 3}
	p := &meterOrderProvider{
		usage: wantUsage,
		beforeStream: func() error {
			if !began {
				return errors.New("provider reached before meter admission")
			}
			return nil
		},
	}
	a := New(agent.NopObserver{}, p, target(), nil, WithMeter(func(provider.Request) (AttemptFinish, error) {
		began = true
		return func(got provider.Usage, err error) error {
			if err != nil {
				t.Fatalf("successful consult settled with error: %v", err)
			}
			if got != wantUsage {
				t.Fatalf("settled usage = %+v, want %+v", got, wantUsage)
			}
			settled = true
			return nil
		}, nil
	}))
	if _, err := a.consult(context.Background(), "task", "evidence", "trigger"); err != nil {
		t.Fatal(err)
	}
	if !settled {
		t.Fatal("advisor did not settle EventDone usage")
	}
}

func TestConsultSettlesProviderFailureConservatively(t *testing.T) {
	wantErr := errors.New("provider unavailable")
	p := &meterOrderProvider{streamErr: wantErr}
	var settledErr error
	a := New(agent.NopObserver{}, p, target(), nil, WithMeter(func(provider.Request) (AttemptFinish, error) {
		return func(_ provider.Usage, err error) error {
			settledErr = err
			return nil
		}, nil
	}))
	if _, err := a.consult(context.Background(), "task", "evidence", "trigger"); !errors.Is(err, wantErr) {
		t.Fatalf("consult err = %v, want provider failure", err)
	}
	if !errors.Is(settledErr, wantErr) {
		t.Fatalf("meter settlement err = %v, want provider failure", settledErr)
	}
}
