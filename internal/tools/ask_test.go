package tools

import (
	"context"
	"strings"
	"testing"
)

// stubQuestioner answers every question with a fixed Answer and records what
// it was asked, so a test can check both directions of the seam.
type stubQuestioner struct {
	answer Answer
	err    error
	asked  []Question
}

func (s *stubQuestioner) AskUser(_ context.Context, q Question) (Answer, error) {
	s.asked = append(s.asked, q)
	return s.answer, s.err
}

func askInputFor(multi bool, labels ...string) map[string]any {
	options := make([]map[string]any, 0, len(labels))
	for _, l := range labels {
		options = append(options, map[string]any{"label": l})
	}
	return map[string]any{"question": "which store?", "options": options, "multi": multi}
}

func TestAskDeliversTheQuestionAndRendersTheChoice(t *testing.T) {
	r, _ := newRegistry(t)
	q := &stubQuestioner{answer: Answer{Picked: []string{"sqlite"}}}
	r.SetQuestioner(q)

	res := run(t, r, "ask", map[string]any{
		"question": "which store should the cache use?",
		"options": []map[string]any{
			{"label": "sqlite", "detail": "one file, no server"},
			{"label": "bolt"},
		},
	})
	if res.IsError {
		t.Fatalf("ask failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "The user chose: sqlite") {
		t.Errorf("result must carry the choice: %q", res.Content)
	}
	if len(q.asked) != 1 {
		t.Fatalf("questioner heard %d questions, want 1", len(q.asked))
	}
	got := q.asked[0]
	if got.Question != "which store should the cache use?" || len(got.Options) != 2 {
		t.Errorf("question crossed the seam mangled: %+v", got)
	}
	if got.Options[0].Detail != "one file, no server" {
		t.Errorf("option detail dropped: %+v", got.Options[0])
	}
}

func TestAskRendersMultipleChoicesInOfferedOrder(t *testing.T) {
	r, _ := newRegistry(t)
	r.SetQuestioner(&stubQuestioner{answer: Answer{Picked: []string{"tests", "docs"}}})

	res := run(t, r, "ask", askInputFor(true, "tests", "docs", "bench"))
	if !strings.Contains(res.Content, "The user chose: tests, docs") {
		t.Errorf("multi answer must list every pick: %q", res.Content)
	}
}

func TestAskCarriesATypedAnswer(t *testing.T) {
	r, _ := newRegistry(t)
	r.SetQuestioner(&stubQuestioner{answer: Answer{Text: "neither; keep it in memory"}})

	res := run(t, r, "ask", askInputFor(false, "sqlite", "bolt"))
	if !strings.Contains(res.Content, "The user answered in their own words: neither; keep it in memory") {
		t.Errorf("typed answer must travel verbatim: %q", res.Content)
	}
}

// TestAskRedactsATypedSecret is the guarantee, not the comment above
// renderAnswer: a key pasted into an answer is recorded and sent with the
// result, so it redacts unconditionally, the injected-report posture.
func TestAskRedactsATypedSecret(t *testing.T) {
	key := "sk-ant-api03-" + strings.Repeat("x", 24)
	r, _ := newRegistry(t)
	r.SetQuestioner(&stubQuestioner{answer: Answer{Text: "use " + key + " for now"}})

	res := run(t, r, "ask", askInputFor(false, "keychain", "env var"))
	if strings.Contains(res.Content, key) {
		t.Fatalf("the key survived into the tool result: %q", res.Content)
	}
	if !strings.Contains(res.Content, "[redacted:") {
		t.Errorf("the redaction must say a credential stood there: %q", res.Content)
	}
}

func TestAskReportsADecline(t *testing.T) {
	r, _ := newRegistry(t)
	r.SetQuestioner(&stubQuestioner{answer: Answer{Declined: true}})

	res := run(t, r, "ask", askInputFor(false, "a", "b"))
	if res.IsError {
		t.Fatalf("a decline is an answer, not an error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "declined") {
		t.Errorf("the model must hear the decline: %q", res.Content)
	}
}

func TestAskRefusesWithNoOneListening(t *testing.T) {
	r, _ := newRegistry(t)

	res := run(t, r, "ask", askInputFor(false, "a", "b"))
	if !res.IsError {
		t.Fatal("an unset questioner must refuse, not hang or invent an answer")
	}
	if !strings.Contains(res.Content, "no one is listening") {
		t.Errorf("the refusal must say why and what to do instead: %q", res.Content)
	}
}

func TestAskCancellationIsAnErrorNotAnAnswer(t *testing.T) {
	r, _ := newRegistry(t)
	r.SetQuestioner(&stubQuestioner{err: context.Canceled})

	if _, err := tryRun(r, "ask", askInputFor(false, "a", "b")); err == nil {
		t.Fatal("a cancelled question must surface as the turn's error, never as a fabricated answer")
	}
}

func TestAskRejectsMalformedQuestions(t *testing.T) {
	r, _ := newRegistry(t)
	r.SetQuestioner(&stubQuestioner{})

	cases := []struct {
		name  string
		input map[string]any
		want  string
	}{
		{"no question", map[string]any{"options": []map[string]any{{"label": "a"}, {"label": "b"}}}, "empty"},
		{"one option", askInputFor(false, "only"), "at least"},
		{"nine options", askInputFor(false, "a", "b", "c", "d", "e", "f", "g", "h", "i"), "form"},
		{"blank label", map[string]any{"question": "q?", "options": []map[string]any{{"label": "a"}, {"label": "  "}}}, "no label"},
		{"duplicate labels", askInputFor(false, "same", "same"), "both"},
	}
	for _, tc := range cases {
		if _, err := tryRun(r, "ask", tc.input); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want mention of %q", tc.name, err, tc.want)
		}
	}
}

// TestBranchAskHasNoQuestioner pins the branch posture: a race arm keeps the
// ask schema for the frozen zone, and its call refuses because the branch
// registry never receives the questioner.
func TestBranchAskHasNoQuestioner(t *testing.T) {
	r, _ := newRegistry(t)
	r.SetQuestioner(&stubQuestioner{answer: Answer{Picked: []string{"a"}}})

	branch := r.Branch(nil)
	res, err := tryRun(branch, "ask", askInputFor(false, "a", "b"))
	if err != nil {
		t.Fatalf("ask in a branch: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "no one is listening") {
		t.Errorf("a branch question must refuse, got %+v", res)
	}

	// The refuse map names the better reason, at Plan, before the engine is
	// consulted.
	refused := r.Branch(map[string]string{"ask": "ask is unavailable in a race branch"})
	if _, err := tryRun(refused, "ask", askInputFor(false, "a", "b")); err == nil ||
		!strings.Contains(err.Error(), "race branch") {
		t.Errorf("the refuse map's reason must reach the model, got %v", err)
	}
}
