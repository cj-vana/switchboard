package main

import (
	"testing"

	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

func delegateBudgetSessions(t *testing.T) (*session.Session, *session.Session) {
	t.Helper()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	primary, err := store.Create(workspace, provider.RouteTargetID("anthropic/first-party/primary"), "rev")
	if err != nil {
		t.Fatal(err)
	}
	sub, err := store.Create(workspace, provider.RouteTargetID("anthropic/first-party/sub"), "rev")
	if err != nil {
		primary.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = primary.Close()
		_ = sub.Close()
	})
	return primary, sub
}

func TestDelegateReconcileAddsOnlyMissingActualCost(t *testing.T) {
	primary, sub := delegateBudgetSessions(t)
	var tracker delegateLedgerTracker
	tracker.mark(primary, sub)

	if err := sub.AppendUsage(session.Usage{CostMicroUSD: 40_000}); err != nil {
		t.Fatal(err)
	}
	attempt, err := primary.BeginBudgetAttempt(100_000)
	if err != nil {
		t.Fatal(err)
	}
	if err := primary.SettleBudgetAttempt(attempt, session.BudgetOutcomeSucceeded, 40_000); err != nil {
		t.Fatal(err)
	}
	if err := tracker.reconcile(primary, sub); err != nil {
		t.Fatal(err)
	}
	state := primary.State()
	if state.ExternalCostMicroUSD != 40_000 || state.RetryReserveMicroUSD != 0 {
		t.Fatalf("delegate cost was double charged: %+v", state)
	}
	if state.Calls != 0 || state.Usage != (provider.Usage{}) {
		t.Fatalf("delegate charge became fake primary usage: %+v", state)
	}
}

func TestDelegateReconcilePreservesPendingDebtAndActualCost(t *testing.T) {
	primary, sub := delegateBudgetSessions(t)
	var tracker delegateLedgerTracker
	tracker.mark(primary, sub)

	if _, err := primary.BeginBudgetAttempt(100_000); err != nil {
		t.Fatal(err)
	}
	if err := sub.AppendUsage(session.Usage{CostMicroUSD: 25_000}); err != nil {
		t.Fatal(err)
	}
	if err := tracker.reconcile(primary, sub); err != nil {
		t.Fatal(err)
	}
	state := primary.State()
	if state.ExternalCostMicroUSD != 25_000 || state.RetryReserveMicroUSD != 100_000 {
		t.Fatalf("unsettled delegate accounting = %+v", state)
	}
}
