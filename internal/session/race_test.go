package session

import (
	"testing"

	"github.com/cj-vana/switchboard/internal/provider"
)

// A race record is audit, not conversation: replay must carry it without it
// touching state, and a reader from before the record type existed treats
// it as padding rather than corruption.
func TestRaceRecordSurvivesReplayWithoutTouchingState(t *testing.T) {
	store, sess := forkFixture(t)
	before := sess.State()

	err := sess.AppendRace(Race{
		Prompt:  "which answer is better",
		A:       RaceArm{Tier: "t1", Target: "scripted/local/small", SessionID: "a1", Status: "completed", CostMicroUSD: 0},
		B:       RaceArm{Tier: "t3", Target: "scripted/local/large", SessionID: "b1", Status: "completed", CostMicroUSD: 4200},
		Outcome: "tie",
		Kept:    "t1",
	})
	if err != nil {
		t.Fatal(err)
	}
	sess.Close()

	reopened, err := store.Open(before.ID)
	if err != nil {
		t.Fatalf("a log holding a race record failed to replay: %v", err)
	}
	defer reopened.Close()
	after := reopened.State()
	if len(after.Messages) != len(before.Messages) {
		t.Errorf("replaying past a race record changed the conversation: %d messages, want %d",
			len(after.Messages), len(before.Messages))
	}
	if after.CostMicroUSD != before.CostMicroUSD {
		t.Errorf("a race record changed the session's own cost: %d, want %d", after.CostMicroUSD, before.CostMicroUSD)
	}
}

// A race arm runs on the rung under trial, and its log has to say so:
// /resume binds the target from the start record, so a branch recorded
// against the source's target would resume the wrong model.
func TestForkOntoRecordsTheBranchTarget(t *testing.T) {
	store, sess := forkFixture(t)
	src := sess.State()

	branch, err := store.ForkOnto(src.ID, len(src.Messages), provider.RouteTargetID("scripted/local/other"))
	if err != nil {
		t.Fatal(err)
	}
	defer branch.Close()
	if got := branch.State().Target; got != "scripted/local/other" {
		t.Errorf("branch recorded target %q, want the rung it will run on", got)
	}
	// The messages are still the source's prefix, byte for byte — the
	// target names what serves the branch, not where its history came from.
	if len(branch.State().Messages) != len(src.Messages) {
		t.Errorf("branch holds %d messages, want the full prefix of %d", len(branch.State().Messages), len(src.Messages))
	}
}
