package main

// The part of compaction that is not a surface.
//
// Summarizing a session and seeding a fresh one from the summary is the same
// operation wherever it is invoked; what differs is who reports progress and
// who owns the swap. The TUI wraps this in its exclusive operation lane and an
// advisor ledger pause; the REPL, which has no advisor and no lane, calls it
// directly. Keeping the middle here means a fix to how the budget is carried
// or how the seed is stamped cannot land on one surface and miss the other.

import (
	"context"
	"errors"
	"fmt"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

// compactInputs is everything the operation needs, named rather than reached
// for through a surface's state, so a second caller cannot quietly supply a
// different budget or a different store.
type compactInputs struct {
	Source       *session.Session
	Store        *session.Store
	Workspace    string
	Catalog      *catalog.Catalog
	Budget       *budgetState
	Client       provider.Provider
	Target       provider.RouteTarget
	Instructions string
}

// compactSession summarizes the source and returns a fresh session seeded with
// that summary. The source is left untouched on every failure path, which is
// what lets a caller report "session unchanged" and mean it: the new session is
// created only after the summary is in hand and metered.
func compactSession(ctx context.Context, in compactInputs) (*session.Session, error) {
	state := in.Source.State()
	if len(state.Messages) == 0 {
		return nil, errors.New("nothing to compact yet")
	}

	req := summarizeRequest(state.Messages, in.Instructions)
	finish, err := beginMeteredCall(in.Budget, in.Catalog, in.Source, in.Target, req, session.UsagePurposeCompact)
	if err != nil {
		return nil, fmt.Errorf("stopped before summarizing: %w", err)
	}
	summary, usage, providerDone, callErr := summarizeRequestCall(ctx, in.Client, in.Target, req)
	// A provider that finished owes its usage even when the call then errored,
	// or the ceiling forgets tokens that were really spent.
	meterOutcome := callErr
	if providerDone {
		meterOutcome = nil
	}
	if err := errors.Join(callErr, finish(usage, meterOutcome)); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	sess, err := in.Store.Create(in.Workspace, in.Target.ID(), in.Catalog.Revision)
	if err != nil {
		return nil, err
	}
	accounting := in.Source.State()
	if err := sess.AppendBudgetTransfer("compact:"+accounting.ID,
		accounting.AccountedCostMicroUSD(), accounting.RetryReserveMicroUSD); err != nil {
		sess.Close()
		return nil, fmt.Errorf("could not carry the session budget: %w", err)
	}

	seed := compactSeedHead + state.ID + "). What follows is a summary of that conversation; treat it as established context.\n\n" + summary
	seedOpening := provider.UserText(seed)
	if capsule, ok := compactContinuity(accounting); ok {
		if _, err = sess.AppendContinuity(capsule); err == nil {
			seedOpening, err = stampTurnOpening(sess, seedOpening)
		}
	}
	// The acknowledgment keeps the log strictly alternating, which every
	// adapter renders correctly; a seed followed directly by the user's next
	// prompt would put two user messages back to back.
	if err == nil {
		err = sess.AppendMessage(seedOpening)
	}
	if err == nil {
		err = sess.AppendMessage(provider.Message{
			Role:    provider.RoleAssistant,
			Content: []provider.Block{provider.Text{Text: "Understood. Continuing from there."}},
		})
	}
	if err != nil {
		sess.Close()
		return nil, err
	}
	return sess, nil
}
