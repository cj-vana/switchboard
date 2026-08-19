package delegate

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/provider"
)

// recordingProvider answers every turn with a fixed line and keeps what it was
// asked, so a test can prove a stage actually saw the previous stage's output
// rather than trusting the plumbing to have carried it.
type recordingProvider struct {
	mu     sync.Mutex
	answer string
	seen   []string
}

func (p *recordingProvider) Name() string { return "recording" }

func (p *recordingProvider) Stream(_ context.Context, _ provider.RouteTarget, req provider.Request) (provider.EventStream, error) {
	p.mu.Lock()
	for _, m := range req.Messages {
		p.seen = append(p.seen, m.Text())
	}
	answer := p.answer
	p.mu.Unlock()
	return &oneTurnStream{events: []provider.Event{
		{Type: provider.EventTextDelta, Text: answer},
		{Type: provider.EventDone, StopReason: provider.StopEndTurn},
	}}, nil
}

func (p *recordingProvider) CountTokens(context.Context, provider.RouteTarget, provider.Request) (provider.TokenEstimate, error) {
	return provider.TokenEstimate{}, nil
}

func (p *recordingProvider) Probe(context.Context, provider.RouteTarget) (provider.ProbeResult, error) {
	return provider.ProbeResult{Reachable: true, ModelPresent: true, Tools: provider.ToolsParallel}, nil
}

func workflowRunner(t *testing.T, p provider.Provider) *Runner {
	t.Helper()
	cfg := testConfig(t, "unused")
	cfg.Tasks = NewTaskManager(4)
	cfg.Probe = func(_ context.Context, tierID string) (config.Tier, provider.Provider, string, error) {
		for _, tier := range ladder() {
			if tier.ID == tierID {
				return tier, p, "", nil
			}
		}
		t.Fatalf("probe asked for unknown tier %s", tierID)
		return config.Tier{}, nil, "", nil
	}
	return NewRunner(cfg)
}

// Stages run in order and a carrying stage is handed what the last one said.
// That ordering is the whole reason a workflow exists rather than a handful of
// delegate calls.
func TestAWorkflowRunsStagesInOrderAndCarriesAnswers(t *testing.T) {
	p := &recordingProvider{answer: "ANSWER-FROM-STAGE"}
	runner := workflowRunner(t, p)

	wf := Workflow{
		Name: "survey",
		Stages: []Stage{
			{Name: "survey", Tasks: []WorkflowTask{
				{Task: "list the files in $ARGUMENTS"},
				{Task: "list the tests"},
			}},
			{Name: "propose", Carry: true, Tasks: []WorkflowTask{{Task: "propose an edit"}}},
		},
	}

	var progress []string
	result := runner.RunWorkflow(context.Background(), wf, "internal/agent", func(text string) {
		progress = append(progress, text)
	})

	if result.Err != nil || result.Canceled {
		t.Fatalf("workflow failed: err=%v canceled=%v", result.Err, result.Canceled)
	}
	if len(result.Stages) != 2 {
		t.Fatalf("ran %d stages, want 2", len(result.Stages))
	}
	if len(result.Stages[0].Answers) != 2 {
		t.Fatalf("the fan-out stage produced %d answers, want 2", len(result.Stages[0].Answers))
	}
	if len(progress) != 2 {
		t.Errorf("progress was reported %d times, want once per stage: %v", len(progress), progress)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	joined := strings.Join(p.seen, "\n")
	// $ARGUMENTS reached the subagent.
	if !strings.Contains(joined, "internal/agent") {
		t.Error("the workflow's arguments never reached a task")
	}
	// The carrying stage saw the previous stage's output, not just its own task.
	var carried bool
	for _, seen := range p.seen {
		if strings.Contains(seen, "propose an edit") && strings.Contains(seen, "ANSWER-FROM-STAGE") {
			carried = true
		}
	}
	if !carried {
		t.Errorf("the carrying stage never saw the previous answers:\n%s", joined)
	}
}

// A cancelled run keeps the stages that finished. The work was done and paid
// for, and discarding it would make cancelling more expensive than waiting.
func TestACancelledWorkflowKeepsFinishedStages(t *testing.T) {
	runner := workflowRunner(t, &recordingProvider{answer: "done"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := runner.RunWorkflow(ctx, Workflow{
		Name:   "survey",
		Stages: []Stage{{Name: "one", Tasks: []WorkflowTask{{Task: "x"}}}},
	}, "", nil)

	if !result.Canceled {
		t.Fatal("a cancelled run did not say so")
	}
}
