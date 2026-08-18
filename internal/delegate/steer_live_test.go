package delegate

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/provider"
)

// steerableProvider answers with a tool call on the first round so the loop
// comes back for a second, which is where an injected message can land. It
// records every request it is given, so the test can see what the subagent's
// model actually received rather than what the queue believed it sent.
type steerableProvider struct {
	mu       sync.Mutex
	requests []provider.Request
	round    int
}

func (p *steerableProvider) Name() string { return "steerable" }

func (p *steerableProvider) Stream(_ context.Context, _ provider.RouteTarget, req provider.Request) (provider.EventStream, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.round++
	round := p.round
	p.mu.Unlock()

	if round == 1 {
		// One tool call, so the loop takes another round.
		return &oneTurnStream{events: []provider.Event{
			{Type: provider.EventToolUse, ToolUse: &provider.ToolUse{
				ID: "c1", Name: "glob", Input: json.RawMessage(`{"pattern":"*.go"}`)}},
			{Type: provider.EventDone, StopReason: provider.StopToolUse},
		}}, nil
	}
	return &oneTurnStream{events: []provider.Event{
		{Type: provider.EventTextDelta, Text: "done"},
		{Type: provider.EventDone, StopReason: provider.StopEndTurn},
	}}, nil
}

func (p *steerableProvider) CountTokens(context.Context, provider.RouteTarget, provider.Request) (provider.TokenEstimate, error) {
	return provider.TokenEstimate{}, nil
}

func (p *steerableProvider) Probe(context.Context, provider.RouteTarget) (provider.ProbeResult, error) {
	return provider.ProbeResult{Reachable: true, ModelPresent: true, Tools: provider.ToolsParallel}, nil
}

// The channel is only real if the message reaches the subagent's model. This
// runs a delegation to completion and reads what the provider was handed on
// the round after the steer was queued.
func TestASteerReachesTheSubagentsNextRequest(t *testing.T) {
	scripted := &steerableProvider{}
	cfg := testConfig(t, "unused")
	cfg.Probe = func(_ context.Context, tierID string) (config.Tier, provider.Provider, string, error) {
		for _, tier := range ladder() {
			if tier.ID == tierID {
				return tier, scripted, "", nil
			}
		}
		t.Fatalf("probe asked for unknown tier %s", tierID)
		return config.Tier{}, nil, "", nil
	}

	// The manager has to be the test's, or the tool builds its own and the
	// steer goes to a registry nothing is running in.
	manager := NewTaskManager(2)
	cfg.Tasks = manager

	// Queue the steer as soon as the task is registered, which is the race a
	// real person creates by typing while the first round is in flight.
	steered := make(chan struct{})
	go func() {
		defer close(steered)
		for {
			for _, task := range manager.List() {
				if task.Status.terminal() {
					return
				}
				if manager.Steer(task.ID, "look at cmd/sb, not internal") == nil {
					return
				}
			}
		}
	}()

	tool, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	res, err := plan(t, tool, `{"task":"survey the repository","tier":"t1"}`).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("delegation failed: %s", res.Content)
	}

	<-steered
	scripted.mu.Lock()
	defer scripted.mu.Unlock()
	if len(scripted.requests) < 2 {
		t.Fatalf("the subagent took %d rounds; a steer needs a second one to land in", len(scripted.requests))
	}
	last := scripted.requests[len(scripted.requests)-1]
	var found string
	for _, m := range last.Messages {
		for _, block := range m.Content {
			if text, ok := block.(provider.Text); ok && strings.Contains(text.Text, steerPrefix) {
				found = text.Text
			}
		}
	}
	if found == "" {
		t.Fatal("the steer never reached the subagent's request")
	}
	if !strings.Contains(found, "cmd/sb") {
		t.Fatalf("the steer arrived garbled: %q", found)
	}
}
