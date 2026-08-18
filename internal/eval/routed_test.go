package eval

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/prefix"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/router"
)

type captureRouter struct {
	input router.Input
}

func (r *captureRouter) Route(input router.Input) (router.Decision, error) {
	r.input = input
	chosen := input.Candidates[len(input.Candidates)-1]
	return router.Decision{
		Tier: chosen.Tier, Target: chosen.Target.ID(), EstimatedCost: chosen.Estimate,
	}, nil
}

type recordingProvider struct {
	mu       sync.Mutex
	probe    provider.ProbeResult
	probeErr error
	turns    [][]provider.Event
	requests []provider.Request
	probes   int
}

func (p *recordingProvider) Name() string { return "recording" }

func (p *recordingProvider) Probe(context.Context, provider.RouteTarget) (provider.ProbeResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.probes++
	return p.probe, p.probeErr
}

func (p *recordingProvider) CountTokens(context.Context, provider.RouteTarget, provider.Request) (provider.TokenEstimate, error) {
	return provider.TokenEstimate{}, nil
}

func (p *recordingProvider) Stream(_ context.Context, _ provider.RouteTarget, request provider.Request) (provider.EventStream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, request)
	if len(p.turns) == 0 {
		return nil, errors.New("recording provider ran out of turns")
	}
	events := p.turns[0]
	p.turns = p.turns[1:]
	return &recordingStream{events: events}, nil
}

type recordingStream struct {
	events []provider.Event
	index  int
}

func (s *recordingStream) Next() (provider.Event, error) {
	if s.index >= len(s.events) {
		return provider.Event{}, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}

func (*recordingStream) Close() error { return nil }

func liveProbe() provider.ProbeResult {
	return provider.ProbeResult{Reachable: true, ModelPresent: true, Tools: provider.ToolsParallel}
}

func completedTurn() []provider.Event {
	return []provider.Event{
		{Type: provider.EventTextDelta, Index: 0, Text: "done"},
		{Type: provider.EventDone, StopReason: provider.StopEndTurn, Usage: provider.Usage{InputTokens: 20, OutputTokens: 2}},
	}
}

func readTurn(id string) []provider.Event {
	call := provider.ToolUse{ID: id, Name: "read", Input: json.RawMessage(`{"path":"note.txt"}`)}
	return []provider.Event{
		{Type: provider.EventToolUse, Index: 0, ToolUse: &call},
		{Type: provider.EventDone, StopReason: provider.StopToolUse, Usage: provider.Usage{InputTokens: 20, OutputTokens: 2}},
	}
}

func evalCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	cat, err := catalog.LoadBundled()
	if err != nil {
		t.Fatal(err)
	}
	return cat
}

func TestRoutedRunScoresTheExactRequestTheSelectedProviderReceives(t *testing.T) {
	cat := evalCatalog(t)
	selector := &captureRouter{}
	lowProvider := &recordingProvider{probe: liveProbe()}
	highProvider := &recordingProvider{probe: liveProbe(), turns: [][]provider.Event{completedTurn()}}
	arms := []Arm{
		{Name: "low", Target: provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-haiku-4-5"}, Provider: lowProvider},
		{Name: "high", Target: provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-opus-5"}, Provider: highProvider},
	}
	routed := RoutedArmFor{Catalog: cat, Router: selector, Ladder: arms}
	task := Task{
		ID: "assembled", Provenance: HandWritten, Prompt: "choose deliberately",
		Setup:  func(string) error { return nil },
		Verify: func(string) (bool, string, error) { return true, "", nil },
	}

	got := routed.Run(context.Background(), Runner{Catalog: cat}, task, 7)
	if !got.Solved || got.Failure != "" {
		t.Fatalf("run = %#v", got)
	}
	if got.Target != arms[1].Target.ID() || got.EstimatedTarget != got.Target {
		t.Fatalf("targets = actual %q estimated %q, want %q", got.Target, got.EstimatedTarget, arms[1].Target.ID())
	}
	if len(highProvider.requests) != 1 {
		t.Fatalf("selected provider requests = %d, want 1", len(highProvider.requests))
	}
	request := highProvider.requests[0]
	if len(request.System) == 0 || len(request.Tools) == 0 || len(request.Messages) != 1 {
		t.Fatalf("execution request was not fully assembled: %#v", request)
	}
	if request.Messages[0].Text() != task.Prompt {
		t.Fatalf("execution prompt = %q, want %q", request.Messages[0].Text(), task.Prompt)
	}
	wantPrompt := prefix.RequestTokens(request)
	wantContext := prefix.RequestTokenCeiling(request)
	if selector.input.Session.PromptTokens != wantPrompt || selector.input.Session.ContextTokens != wantContext {
		t.Fatalf("session tokens = (%d, %d), want (%d, %d)",
			selector.input.Session.PromptTokens, selector.input.Session.ContextTokens, wantPrompt, wantContext)
	}
	for _, candidate := range selector.input.Candidates {
		if candidate.PromptTokens != wantPrompt || candidate.ContextTokens != wantContext {
			t.Errorf("candidate %s tokens = (%d, %d), want (%d, %d)",
				candidate.Tier, candidate.PromptTokens, candidate.ContextTokens, wantPrompt, wantContext)
		}
		if candidate.ReservedOutputTokens != provider.EffectiveOutputTokenReserve(candidate.Target, candidate.Info.MaxOutput) {
			t.Errorf("candidate %s output reserve = %d", candidate.Tier, candidate.ReservedOutputTokens)
		}
	}
}

func TestUnsafePromptOnlyPickFailsClosed(t *testing.T) {
	_, _, err := (RoutedArmFor{}).Pick(Task{Prompt: "not an assembled request"})
	if !errors.Is(err, errRoutedFidelity) {
		t.Fatalf("Pick error = %v, want routed fidelity refusal", err)
	}
}

func TestRoutedPickUsesFirstLiveFeasibleFallback(t *testing.T) {
	cat := evalCatalog(t)
	primary := &recordingProvider{probe: provider.ProbeResult{Detail: "offline"}}
	fallback := &recordingProvider{probe: liveProbe()}
	primaryTarget := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-haiku-4-5"}
	fallbackTarget := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-opus-5"}
	routed := RoutedArmFor{Catalog: cat, Ladder: []Arm{{
		Name: "low", Target: primaryTarget, Provider: primary,
		Fallbacks: []Fallback{{Target: fallbackTarget, Provider: fallback}},
	}}}
	request := provider.Request{Messages: []provider.Message{provider.UserText("small fix")}}

	arm, decision, err := routed.PickRequest(context.Background(), Task{Prompt: "small fix"}, request)
	if err != nil {
		t.Fatal(err)
	}
	if arm.Target.ID() != fallbackTarget.ID() || decision.Target != fallbackTarget.ID() {
		t.Fatalf("fallback was not bound: arm=%s decision=%s", arm.Target.ID(), decision.Target)
	}
}

func TestRoutedPickEnforcesPolicyBudgetAndContextEnvelope(t *testing.T) {
	cat := evalCatalog(t)
	providerOK := &recordingProvider{probe: liveProbe()}
	target := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-haiku-4-5"}
	request := provider.Request{Messages: []provider.Message{provider.UserText("small fix")}}

	tests := []struct {
		name   string
		arm    Arm
		policy RoutedArmFor
	}{
		{
			name: "destination policy",
			arm:  Arm{Name: "only", Target: target, Provider: providerOK},
			policy: RoutedArmFor{Requirements: router.Requirements{
				ApprovedProviders: []string{"kimi"},
			}},
		},
		{
			name:   "hard budget",
			arm:    Arm{Name: "only", Target: target, Provider: providerOK},
			policy: RoutedArmFor{Budgets: router.Budgets{MaxCost: 1, MaxCostSet: true}},
		},
	}
	info, _, ok := cat.Lookup(target)
	if !ok {
		t.Fatal("test target is absent from catalog")
	}
	tight := target
	tight.Params.MaxOutputTokens = info.ContextWindow
	tests = append(tests, struct {
		name   string
		arm    Arm
		policy RoutedArmFor
	}{name: "input plus output context envelope", arm: Arm{Name: "only", Target: tight, Provider: providerOK}})

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.policy.Catalog = cat
			test.policy.Ladder = []Arm{test.arm}
			_, _, err := test.policy.PickRequest(context.Background(), Task{Prompt: "small fix"}, request)
			if !errors.Is(err, errRoutedFidelity) {
				t.Fatalf("error = %v, want routed fidelity refusal", err)
			}
		})
	}
}

func TestExplicitOutputAboveCatalogRaisesOpeningAndPerCallBudgetBound(t *testing.T) {
	cat := evalCatalog(t)
	baseTarget := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-haiku-4-5"}
	info, _, ok := cat.Lookup(baseTarget)
	if !ok {
		t.Fatal("test target is absent from catalog")
	}
	explicitTarget := baseTarget
	explicitTarget.Params.MaxOutputTokens = info.MaxOutput + 10_000
	server := &recordingProvider{probe: liveProbe()}
	request := provider.Request{Messages: []provider.Message{provider.UserText("small fix")}}
	promptTokens := prefix.RequestTokens(request)
	contextTokens := prefix.RequestTokenCeiling(request)
	oldBound := candidateForRequest(
		Arm{Name: "only", Target: baseTarget}, 0, info, promptTokens, contextTokens).CeilingCost
	explicitBound := candidateForRequest(
		Arm{Name: "only", Target: explicitTarget}, 0, info, promptTokens, contextTokens).CeilingCost
	if explicitBound <= oldBound {
		t.Fatalf("explicit output did not raise ceiling: old=%s explicit=%s", oldBound, explicitBound)
	}

	routed := RoutedArmFor{
		Catalog: cat,
		Ladder:  []Arm{{Name: "only", Target: explicitTarget, Provider: server}},
		Budgets: router.Budgets{MaxCost: oldBound, MaxCostSet: true},
	}
	if _, _, err := routed.PickRequest(context.Background(), Task{Prompt: "small fix"}, request); !errors.Is(err, errRoutedFidelity) {
		t.Fatalf("opening admitted explicit output above budget: %v", err)
	}

	// The loop rechecks the same concrete adapter allowance before every call,
	// not just at opening selection.
	withoutBudget := routed
	withoutBudget.Budgets = router.Budgets{}
	_, loop := escalationHarness(t, withoutBudget, provider.UserText("continue"))
	callRequest := provider.Request{
		System: loop.System, Tools: loop.Tools.Definitions(), Messages: loop.Session.State().Messages,
	}
	callContext := prefix.RequestTokenCeiling(callRequest)
	oldCallBound := candidateForRequest(
		Arm{Name: "only", Target: baseTarget}, 0, info, callContext, callContext).CeilingCost
	guard := newEvalBudget(
		router.Budgets{MaxCost: oldCallBound, MaxCostSet: true}, cat, loop)
	if err := guard.before(callContext, 1); !errors.Is(err, errRoutedFidelity) {
		t.Fatalf("per-call guard admitted explicit output above budget: %v", err)
	}
}

func TestRoutedRunEscalatesOnlyAfterPreparedMoveAndMarksOpeningEstimateUnavailable(t *testing.T) {
	cat := evalCatalog(t)
	lowTarget := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-haiku-4-5"}
	highTarget := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-opus-5"}
	low := &recordingProvider{
		probe: liveProbe(),
		turns: [][]provider.Event{readTurn("one"), readTurn("two"), readTurn("three")},
	}
	high := &recordingProvider{probe: liveProbe(), turns: [][]provider.Event{completedTurn()}}
	routed := RoutedArmFor{Catalog: cat, Ladder: []Arm{
		{Name: "low", Target: lowTarget, Provider: low},
		{Name: "high", Target: highTarget, Provider: high},
	}}
	task := Task{
		ID: "move", Provenance: HandWritten, Prompt: "make the small fix",
		Setup: func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "note.txt"), []byte("hello\n"), 0o600)
		},
		Verify: func(string) (bool, string, error) { return true, "", nil },
	}

	got := routed.Run(context.Background(), Runner{Catalog: cat, MaxRounds: 5}, task, 0)
	if !got.Solved || got.Failure != "" {
		t.Fatalf("escalated run = %#v", got)
	}
	if got.Target != highTarget.ID() || got.EstimatedTarget != lowTarget.ID() || got.Escalations != 1 {
		t.Fatalf("routing attribution = actual %s estimate %s moves %d",
			got.Target, got.EstimatedTarget, got.Escalations)
	}
	summary := Summarize(RoutedArm, []Run{got})
	if summary.EstimatesUnavailable != 1 || len(summary.EstimateError) != 0 {
		t.Fatalf("moved estimate was reconciled across targets: %#v", summary)
	}
}

func TestUnpreparableEscalationStaysAndLetsTheRoutedRunContinue(t *testing.T) {
	cat := evalCatalog(t)
	lowTarget := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-haiku-4-5"}
	highTarget := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-opus-5"}
	low := &recordingProvider{
		probe: liveProbe(),
		turns: [][]provider.Event{readTurn("one"), readTurn("two"), readTurn("three"), completedTurn()},
	}
	high := &recordingProvider{probe: liveProbe()}
	routed := RoutedArmFor{Catalog: cat, Ladder: []Arm{
		{Name: "low", Target: lowTarget, Provider: low},
		{Name: "high", Target: highTarget, Provider: high},
	}}
	verified := false
	task := Task{
		ID: "blocked-move", Provenance: HandWritten, Prompt: "make the small fix",
		Setup: func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "note.txt"), []byte("hello\n"), 0o600)
		},
		Verify: func(string) (bool, string, error) {
			verified = true
			return true, "", nil
		},
	}

	// Opening selection sees the high rung as live. The move's second live
	// probe then fails, proving that the prepared destination—not stale opening
	// evidence—governs the atomic bind.
	high.probe = provider.ProbeResult{Detail: "went offline before the move"}
	selector := &captureRouter{}
	selectorRouteLow := router.Router(routerFunc(func(input router.Input) (router.Decision, error) {
		selector.input = input
		chosen := input.Candidates[0]
		// Make high live for opening resolution, then unavailable for the move.
		high.mu.Lock()
		high.probe = provider.ProbeResult{Detail: "went offline before the move"}
		high.mu.Unlock()
		return router.Decision{Tier: chosen.Tier, Target: chosen.Target.ID(), EstimatedCost: chosen.Estimate}, nil
	}))
	high.probe = liveProbe()
	routed.Router = selectorRouteLow

	got := routed.Run(context.Background(), Runner{Catalog: cat, MaxRounds: 5}, task, 0)
	if !got.Solved || got.Failure != "" || !verified {
		t.Fatalf("production-equivalent stay did not continue cleanly: run=%#v verified=%v", got, verified)
	}
	if got.Target != lowTarget.ID() || got.Escalations != 0 {
		t.Fatalf("rejected move changed target/rank: target=%s moves=%d", got.Target, got.Escalations)
	}
}

func TestFixedArmIgnoresRoutedFallbacksAndDoesNotProbe(t *testing.T) {
	cat := evalCatalog(t)
	primary := &recordingProvider{probe: liveProbe(), turns: [][]provider.Event{completedTurn()}}
	fallback := &recordingProvider{probe: liveProbe(), turns: [][]provider.Event{completedTurn()}}
	arm := Arm{
		Name:     "fixed",
		Target:   provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-haiku-4-5"},
		Provider: primary,
		Fallbacks: []Fallback{{
			Target:   provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-opus-5"},
			Provider: fallback,
		}},
	}
	task := Task{
		ID: "fixed", Provenance: HandWritten, Prompt: "answer",
		Setup:  func(string) error { return nil },
		Verify: func(string) (bool, string, error) { return true, "", nil },
	}

	got := (Runner{Catalog: cat}).Run(context.Background(), task, arm, 0)
	if !got.Solved || got.Target != arm.Target.ID() {
		t.Fatalf("fixed run = %#v", got)
	}
	if primary.probes != 0 || fallback.probes != 0 || len(fallback.requests) != 0 {
		t.Fatalf("fixed arm used routed resolution: primary probes=%d fallback probes=%d requests=%d",
			primary.probes, fallback.probes, len(fallback.requests))
	}
}

type routerFunc func(router.Input) (router.Decision, error)

func (f routerFunc) Route(input router.Input) (router.Decision, error) { return f(input) }

var _ router.Router = (*captureRouter)(nil)
var _ provider.Provider = (*recordingProvider)(nil)
