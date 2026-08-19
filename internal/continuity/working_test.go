package continuity

import "testing"

// The list changes far more often than the reason for it, so a call that says
// nothing about the objective must not erase it.
func TestObjectiveAndStopConditionSurviveAListUpdate(t *testing.T) {
	first := WithWorking(nil, []Task{{Text: "read the code", Status: TaskActive}}, Working{
		Objective:     "make the adapter speak both thinking dialects",
		StopCondition: "the offline tests pin both shapes",
		NextAction:    "read the adapter",
	})

	second := WithWorking(&first, []Task{{Text: "read the code", Status: TaskDone}}, Working{})
	if second.Objective != first.Objective {
		t.Errorf("objective = %q, want it kept across a list update", second.Objective)
	}
	if second.StopCondition != first.StopCondition {
		t.Errorf("stop condition = %q, want it kept across a list update", second.StopCondition)
	}
}

// The next action names the very next step, so the call that changed the list
// is the moment it stopped being true.
func TestTheNextActionDoesNotSurviveAListUpdate(t *testing.T) {
	first := WithWorking(nil, []Task{{Text: "one", Status: TaskActive}}, Working{NextAction: "read the adapter"})
	second := WithWorking(&first, []Task{{Text: "one", Status: TaskDone}}, Working{})
	if second.NextAction == "read the adapter" {
		t.Error("a stale next action survived the call that invalidated it")
	}
}

// A later call replaces what an earlier one said.
func TestANewObjectiveReplacesTheOldOne(t *testing.T) {
	first := WithWorking(nil, nil, Working{Objective: "the first thing"})
	second := WithWorking(&first, nil, Working{Objective: "the second thing"})
	if second.Objective != "the second thing" {
		t.Errorf("objective = %q, want the newer one", second.Objective)
	}
}

// WithTasks is the old signature and must behave exactly as it did, or every
// existing caller quietly changes meaning.
func TestWithTasksIsUnchanged(t *testing.T) {
	prior := Capsule{Objective: "kept", StopCondition: "also kept", NextAction: "dropped"}
	out := WithTasks(&prior, []Task{{Text: "a", Status: TaskPending}})
	if out.Objective != "kept" || out.StopCondition != "also kept" {
		t.Errorf("out = %+v, want the semantic fields carried", out)
	}
	if out.NextAction != "" {
		t.Errorf("next action = %q, want it cleared as it always was", out.NextAction)
	}
}

// What the model writes goes through the same redaction everything else does,
// because a capsule is durable and is rendered into a later context.
func TestWorkingFieldsAreRedactedLikeTheRest(t *testing.T) {
	const token = "sk-ant-api03-JZoUmalVvXBSXFuPPFAdMSFRLXMWZAAgvVPMNXHJIRVwvKAFFDTIJXPXBBRLDXNQ"
	capsule := WithWorking(nil, nil, Working{Objective: "use " + token + " for the calls"})
	capsule.Source = SourceTodo

	prepared, err := Prepare(capsule)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Objective == capsule.Objective {
		t.Errorf("objective = %q, which still carries the credential", prepared.Objective)
	}
}
