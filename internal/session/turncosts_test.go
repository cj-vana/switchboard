package session

import (
	"math"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/provider"
)

func TestTurnCostsIncludeExternalChargesWithoutFakeCalls(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "anthropic/first-party/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.AppendMessage(provider.UserText("delegate this")); err != nil {
		t.Fatal(err)
	}
	attempt, err := sess.BeginBudgetAttempt(100_000)
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.SettleBudgetAttempt(attempt, BudgetOutcomeSucceeded, 25_000); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendBudgetTransfer("race:loser", 15_000, 0); err != nil {
		t.Fatal(err)
	}

	turns, err := ReadTurnCosts(sess.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].CostMicroUSD != 40_000 || turns[0].Calls != 0 || turns[0].Usage != (provider.Usage{}) {
		t.Fatalf("external per-turn accounting = %+v", turns)
	}
}

func TestTurnCostsKeepBackgroundModelWorkOutOfThePreviousTurn(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "anthropic/first-party/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.AppendMessage(provider.UserText("implement the feature")); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendUsage(Usage{Purpose: UsagePurposeTurn, Usage: provider.Usage{InputTokens: 10}, CostMicroUSD: 100}); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendUsage(Usage{Purpose: UsagePurposeAdvisor, Usage: provider.Usage{InputTokens: 20}, CostMicroUSD: 200}); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendUsage(Usage{Purpose: UsagePurposeCompact, Usage: provider.Usage{InputTokens: 30}, CostMicroUSD: 300}); err != nil {
		t.Fatal(err)
	}

	got, err := ReadTurnCosts(sess.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("turn/background buckets = %+v", got)
	}
	if got[0].Turn != 1 || got[0].Purpose != UsagePurposeTurn || got[0].CostMicroUSD != 100 {
		t.Fatalf("user turn absorbed background work: %+v", got[0])
	}
	if got[1].Purpose != UsagePurposeAdvisor || got[1].Turn != 0 || got[1].CostMicroUSD != 200 {
		t.Fatalf("advisor bucket = %+v", got[1])
	}
	if got[2].Purpose != UsagePurposeCompact || got[2].Turn != 0 || got[2].CostMicroUSD != 300 {
		t.Fatalf("compact bucket = %+v", got[2])
	}
}

func TestTurnCostReaderRejectsTokenOverflow(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "anthropic/first-party/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.UserText("overflow")); err != nil {
		t.Fatal(err)
	}
	// Write raw records so the reader itself is tested; AppendUsage correctly
	// rejects the second one at the session-state boundary.
	for i, count := range []int{math.MaxInt, 1} {
		sess.mu.Lock()
		err = sess.append(RecordUsage, Usage{CallID: string(rune('a' + i)), Usage: provider.Usage{InputTokens: count}})
		sess.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ReadTurnCosts(sess.Path()); err == nil || !strings.Contains(err.Error(), "overflow") {
		t.Fatalf("turn token overflow was accepted: %v", err)
	}
}
