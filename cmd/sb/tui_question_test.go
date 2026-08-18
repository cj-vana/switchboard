package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/tools"
)

func questionFixture(multi bool) tools.Question {
	return tools.Question{
		Question: "which store should the cache use?",
		Multi:    multi,
		Options: []tools.QuestionOption{
			{Label: "sqlite", Detail: "one file, no server"},
			{Label: "bolt"},
			{Label: "memory"},
		},
	}
}

func openQuestion(t *testing.T, multi bool) (*tuiModel, chan tools.Answer) {
	t.Helper()
	m := testModel(t)
	respond := make(chan tools.Answer, 1)
	m.dlg = newQuestionDialog(questionFixture(multi), respond)
	return m, respond
}

func answered(t *testing.T, respond chan tools.Answer) tools.Answer {
	t.Helper()
	select {
	case ans := <-respond:
		return ans
	default:
		t.Fatal("the dialog closed without resolving; the loop would hang on this channel")
		return tools.Answer{}
	}
}

func TestQuestionDialogSingleSelect(t *testing.T) {
	m, respond := openQuestion(t, false)

	view := m.dlg.view(80, m.th)
	if !strings.Contains(view, "which store should the cache use?") ||
		!strings.Contains(view, "sqlite") || !strings.Contains(view, "one file, no server") {
		t.Fatalf("the dialog must show the question, the options, and their details:\n%s", view)
	}

	m.dlg.update(tea.KeyMsg{Type: tea.KeyDown}, m.th)
	done, _ := m.dlg.update(tea.KeyMsg{Type: tea.KeyEnter}, m.th)
	if !done {
		t.Fatal("enter did not close the dialog")
	}
	if ans := answered(t, respond); len(ans.Picked) != 1 || ans.Picked[0] != "bolt" {
		t.Fatalf("answer = %+v, want the highlighted option", ans)
	}
}

func TestQuestionDialogDigitQuickPick(t *testing.T) {
	m, respond := openQuestion(t, false)

	done, _ := m.dlg.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'3'}}, m.th)
	if !done {
		t.Fatal("a digit on a single-select question must answer at once")
	}
	if ans := answered(t, respond); len(ans.Picked) != 1 || ans.Picked[0] != "memory" {
		t.Fatalf("answer = %+v, want option 3", ans)
	}
}

func TestQuestionDialogMultiMarksInOfferedOrder(t *testing.T) {
	m, respond := openQuestion(t, true)

	// Mark the third option first, then the first: the answer must come
	// back in offered order, the shape the model asked the question in.
	m.dlg.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'3'}}, m.th)
	m.dlg.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'1'}}, m.th)

	if view := m.dlg.view(80, m.th); !strings.Contains(view, "[x]") {
		t.Fatalf("a marked option must show its mark:\n%s", view)
	}

	done, _ := m.dlg.update(tea.KeyMsg{Type: tea.KeyEnter}, m.th)
	if !done {
		t.Fatal("enter did not close the dialog")
	}
	if ans := answered(t, respond); strings.Join(ans.Picked, ",") != "sqlite,memory" {
		t.Fatalf("answer = %+v, want sqlite,memory in offered order", ans)
	}
}

func TestQuestionDialogMultiEnterWithNoMarksPicksTheHighlighted(t *testing.T) {
	m, respond := openQuestion(t, true)

	m.dlg.update(tea.KeyMsg{Type: tea.KeyDown}, m.th)
	done, _ := m.dlg.update(tea.KeyMsg{Type: tea.KeyEnter}, m.th)
	if !done {
		t.Fatal("enter did not close the dialog")
	}
	if ans := answered(t, respond); strings.Join(ans.Picked, ",") != "bolt" {
		t.Fatalf("answer = %+v, want the highlighted option alone", ans)
	}
}

func TestQuestionDialogTypedAnswer(t *testing.T) {
	m, respond := openQuestion(t, false)

	// Down past the options lands on the type-your-own row.
	for range 3 {
		m.dlg.update(tea.KeyMsg{Type: tea.KeyDown}, m.th)
	}
	if done, _ := m.dlg.update(tea.KeyMsg{Type: tea.KeyEnter}, m.th); done {
		t.Fatal("arming the input must not close the dialog")
	}
	m.dlg.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("neither, keep it in memory")}, m.th)
	done, _ := m.dlg.update(tea.KeyMsg{Type: tea.KeyEnter}, m.th)
	if !done {
		t.Fatal("enter did not send the typed answer")
	}
	if ans := answered(t, respond); ans.Text != "neither, keep it in memory" {
		t.Fatalf("answer = %+v, want the typed text", ans)
	}
}

func TestQuestionDialogEscapeDeclines(t *testing.T) {
	m, respond := openQuestion(t, false)

	done, _ := m.dlg.update(tea.KeyMsg{Type: tea.KeyEscape}, m.th)
	if !done {
		t.Fatal("esc did not close the dialog")
	}
	if ans := answered(t, respond); !ans.Declined {
		t.Fatalf("answer = %+v, want a decline: the loop is blocked and must hear something", ans)
	}
}

func TestQuestionDialogEscapeWhileTypingReturnsToTheList(t *testing.T) {
	m, respond := openQuestion(t, false)

	for range 3 {
		m.dlg.update(tea.KeyMsg{Type: tea.KeyDown}, m.th)
	}
	m.dlg.update(tea.KeyMsg{Type: tea.KeyEnter}, m.th)
	if done, _ := m.dlg.update(tea.KeyMsg{Type: tea.KeyEscape}, m.th); done {
		t.Fatal("esc while typing must back out to the options, not decline")
	}
	select {
	case ans := <-respond:
		t.Fatalf("nothing should have resolved yet, got %+v", ans)
	default:
	}
}

func TestParseQuestionPicks(t *testing.T) {
	q := questionFixture(false)
	multi := questionFixture(true)

	cases := []struct {
		name   string
		answer string
		q      tools.Question
		want   string
		ok     bool
	}{
		{"single number", "2", q, "bolt", true},
		{"single refuses two numbers", "1 2", q, "", false},
		{"multi numbers", "3 1", multi, "sqlite,memory", true},
		{"multi with commas", "1,2", multi, "sqlite,bolt", true},
		{"out of range", "4", q, "", false},
		{"words are words", "just cache in memory", q, "", false},
		{"half-numeric is words", "1 maybe", multi, "", false},
	}
	for _, tc := range cases {
		got, ok := parseQuestionPicks(tc.answer, tc.q)
		if ok != tc.ok || strings.Join(got, ",") != tc.want {
			t.Errorf("%s: parseQuestionPicks(%q) = %v, %v; want %q, %v", tc.name, tc.answer, got, ok, tc.want, tc.ok)
		}
	}
}
