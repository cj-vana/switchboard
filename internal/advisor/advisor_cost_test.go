package advisor

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/tools"
)

// The advisor is a second model on a second bill, so what one consult can cost
// has to be a bound rather than a hope. Its evidence is a bounded tail and its
// budget is a count per turn; this pins the product of the two, and fails if a
// later change lets either grow quietly.
func TestOneConsultStaysWithinItsStatedBound(t *testing.T) {
	p := &scriptedProvider{answer: "NONE"}
	a := New(agent.NopObserver{}, p, target(), nil)

	// Worst case: a turn that fills the tail with the longest lines the
	// recorder keeps, and a task string no one would write but nothing stops.
	a.StartTurn(strings.Repeat("fix the flaky integration test in the payments package ", 40))
	req := permission.Request{Tool: "exec", Argv: []string{"exec", strings.Repeat("x", 900)}}
	for i := 0; i < maxEvidenceLines*3; i++ {
		call := provider.ToolUse{
			ID:    fmt.Sprintf("bulk-%d", i),
			Name:  "exec",
			Input: json.RawMessage(`{"command":["` + strings.Repeat("x", 900) + `"]}`),
		}
		a.ToolStart(call, req)
		a.ToolEnd(call, req, tools.Result{Content: strings.Repeat("y", 900), IsError: true}, time.Second)
	}
	repeatCall(a, 4)

	// The consult runs off the caller's goroutine; wait for the request to
	// reach the provider rather than for advice, since "NONE" is a valid
	// answer that produces none.
	deadline := time.Now().Add(5 * time.Second)
	for {
		p.mu.Lock()
		made := len(p.prompts)
		p.mu.Unlock()
		if made > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no consult was made")
		}
		time.Sleep(10 * time.Millisecond)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.prompts) == 0 {
		t.Fatal("no consult was made")
	}
	worst := 0
	for _, prompt := range p.prompts {
		if n := len(prompt) + len(systemPrompt); n > worst {
			worst = n
		}
	}
	// Four bytes per token is the estimator's own floor, used here for the
	// same reason it is used there: it is the conservative direction.
	tokens := worst / 4
	t.Logf("worst consult: %d bytes, about %d tokens, from a %d-line tail; %d per turn, %v cooldown; "+
		"so at most about %d tokens per turn",
		worst, tokens, len(a.events), DefaultMaxConsultsPerTurn, DefaultCooldown,
		tokens*DefaultMaxConsultsPerTurn)

	const ceiling = 12000
	if tokens > ceiling {
		t.Fatalf("one consult reaches about %d tokens, past the %d this is willing to spend "+
			"on a second model per wake-up", tokens, ceiling)
	}
	if got := len(a.events); got > maxEvidenceLines {
		t.Fatalf("the evidence tail holds %d lines, past its %d bound", got, maxEvidenceLines)
	}
}
