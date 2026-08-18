package main

import (
	"sync"
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
	tracker.mark(sub)

	if err := sub.AppendUsage(session.Usage{CostMicroUSD: 40_000}); err != nil {
		t.Fatal(err)
	}
	attempt, err := primary.BeginBudgetAttempt(100_000)
	if err != nil {
		t.Fatal(err)
	}
	if err := tracker.settle(primary, sub, attempt, session.BudgetOutcomeSucceeded, 40_000); err != nil {
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
	tracker.mark(sub)

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

func TestConcurrentDelegateReconcileAttributesEachSubsession(t *testing.T) {
	primary, first := delegateBudgetSessions(t)
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(t.TempDir(), provider.RouteTargetID("anthropic/first-party/second"), "rev")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })

	var tracker delegateLedgerTracker
	tracker.mark(first)
	tracker.mark(second)
	if err := first.AppendUsage(session.Usage{CostMicroUSD: 40_000}); err != nil {
		t.Fatal(err)
	}
	if err := second.AppendUsage(session.Usage{CostMicroUSD: 50_000}); err != nil {
		t.Fatal(err)
	}
	// Neither ordinary settlement made it to the primary. Reconciling one
	// task must not make the other's usage look paid.
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, sub := range []*session.Session{first, second} {
		sub := sub
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- tracker.reconcile(primary, sub)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := primary.State().ExternalCostMicroUSD; got != 90_000 {
		t.Fatalf("concurrent delegate reconciliation = %d, want 90000", got)
	}
}
