package eval

import (
	"encoding/json"
	"testing"
)

func TestFailureKindBackfillsOldJournalRows(t *testing.T) {
	var run Run
	if err := json.Unmarshal([]byte(`{
		"Solved": false,
		"Detail": "the turn failed: context deadline exceeded"
	}`), &run); err != nil {
		t.Fatal(err)
	}
	if got := run.FailureKind(); got != FailureTimeout {
		t.Fatalf("failure = %q, want %q", got, FailureTimeout)
	}
}

func TestRecordedFailureKindWinsOverLegacyText(t *testing.T) {
	run := Run{
		Detail:  "the turn failed: context deadline exceeded",
		Failure: FailureSetup,
	}
	if got := run.FailureKind(); got != FailureSetup {
		t.Fatalf("failure = %q, want recorded %q", got, FailureSetup)
	}
}

func TestSolvedRunCannotHideARecordedInfrastructureFailure(t *testing.T) {
	run := Run{Solved: true, Failure: FailureTurn}
	if got := run.FailureKind(); got != FailureTurn {
		t.Fatalf("failure = %q, want recorded %q", got, FailureTurn)
	}
}

func TestLegacyVerifierFailureIsNotMisclassifiedByNestedTimeout(t *testing.T) {
	run := Run{Detail: "the verifier failed to run: context deadline exceeded"}
	if got := run.FailureKind(); got != FailureVerifier {
		t.Fatalf("failure = %q, want %q", got, FailureVerifier)
	}
}

func TestLegacyUnclassifiedDetailStaysUnknownRatherThanBecomingQualityEvidence(t *testing.T) {
	run := Run{Detail: "expected context deadline exceeded, got nil"}
	if got := run.FailureKind(); got != FailureUnknown {
		t.Fatalf("failure = %q, want %q", got, FailureUnknown)
	}
}

func TestOnlySolvedAndExplicitVerificationOutcomesCountAsModelQuality(t *testing.T) {
	tests := []struct {
		name string
		run  Run
		want bool
	}{
		{name: "solved", run: Run{Solved: true}, want: true},
		{name: "verification loss", run: Run{Failure: FailureVerification}, want: true},
		{name: "legacy unclassified loss", run: Run{Detail: "tests still fail"}},
		{name: "round limit", run: Run{Failure: FailureRoundLimit}},
		{name: "timeout", run: Run{Failure: FailureTimeout}},
		{name: "provider turn", run: Run{Failure: FailureTurn}},
		{name: "verifier crashed", run: Run{Failure: FailureVerifier}},
		{name: "routing fidelity", run: Run{Failure: FailureFidelity}},
		{name: "contradictory solved infrastructure", run: Run{Solved: true, Failure: FailureSetup}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := modelQualityOutcome(test.run); got != test.want {
				t.Fatalf("modelQualityOutcome(%#v) = %v, want %v", test.run, got, test.want)
			}
		})
	}
}
