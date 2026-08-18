package main

import (
	"bufio"
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/credential"
)

const testGitHubToken = "ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789"

// A key-shaped prompt does not start a turn; it opens the gate, and until
// the user answers, nothing has left the machine.
func TestStartTurnHoldsAKeyBehindTheGate(t *testing.T) {
	m := testModel(t)
	cmd := m.startTurn("review this: "+testGitHubToken, "")
	if cmd != nil {
		t.Error("a gated turn still returned a command")
	}
	if m.dlg == nil {
		t.Fatal("a prompt carrying a token opened no gate")
	}
	if m.busy {
		t.Error("the turn began before the gate was answered")
	}
	view := m.dlg.view(90, m.th)
	if !strings.Contains(view, "GitHub token") {
		t.Errorf("the gate does not name what it found:\n%s", view)
	}
	if strings.Contains(view, testGitHubToken) {
		t.Errorf("the gate shows the secret it exists to hold back:\n%s", view)
	}
}

// The three answers: redact rewrites the outbound copy, send passes it as
// typed, and esc drops it — the safe direction is the default.
func TestSecretGateAnswers(t *testing.T) {
	m := testModel(t)
	prompt := "use " + testGitHubToken + " for the API"
	leaks := credential.ScanPrompt(prompt)
	if len(leaks) == 0 {
		t.Fatal("fixture token was not detected")
	}

	var sent string
	m.openSecretGate(leaks, prompt, func(p string) tea.Cmd { sent = p; return nil })
	done, _ := m.dlg.update(tea.KeyMsg{Type: tea.KeyEnter}, m.th) // first item: redact
	if !done {
		t.Fatal("the gate did not resolve on enter")
	}
	if strings.Contains(sent, testGitHubToken) {
		t.Errorf("redact sent the secret: %q", sent)
	}
	if !strings.Contains(sent, "[redacted: a GitHub token]") {
		t.Errorf("redact does not say what stood there: %q", sent)
	}

	sent = ""
	m.openSecretGate(leaks, prompt, func(p string) tea.Cmd { sent = p; return nil })
	if done, _ = m.dlg.update(tea.KeyMsg{Type: tea.KeyEscape}, m.th); !done {
		t.Fatal("esc did not resolve the gate")
	}
	if sent != "" {
		t.Errorf("esc sent the prompt anyway: %q", sent)
	}
}

// The REPL's form of the gate: same three answers, asked in line, and the
// question itself never quotes the key.
func TestReplSecretGateAnswers(t *testing.T) {
	prompt := "use " + testGitHubToken + " for the API"
	leaks := credential.ScanPrompt(prompt)
	if len(leaks) == 0 {
		t.Fatal("fixture token was not detected")
	}
	ask := func(answer string) (string, string) {
		t.Helper()
		out, err := os.CreateTemp(t.TempDir(), "repl")
		if err != nil {
			t.Fatal(err)
		}
		defer out.Close()
		r := &repl{in: bufio.NewReader(strings.NewReader(answer)), out: newRenderer(out)}
		sent := r.secretGate(prompt, leaks)
		r.out.flush()
		printed, err := os.ReadFile(out.Name())
		if err != nil {
			t.Fatal(err)
		}
		return sent, string(printed)
	}

	if sent, printed := ask("r\n"); strings.Contains(sent, testGitHubToken) ||
		!strings.Contains(sent, "[redacted: a GitHub token]") || strings.Contains(printed, testGitHubToken) {
		t.Errorf("redact answer: sent %q, printed %q", sent, printed)
	}
	if sent, _ := ask("s\n"); sent != prompt {
		t.Errorf("send answer did not pass the prompt as typed: %q", sent)
	}
	if sent, printed := ask("\n"); sent != "" || strings.Contains(printed, testGitHubToken) {
		t.Errorf("an empty answer sent anyway: %q (printed %q)", sent, printed)
	}
}

// The race record is a summary, not the transcript: a key typed into the
// /race prompt must not ride the record into the log after the gate
// scrubbed it from what was sent.
func TestFinishRaceRedactsTheRecordedPrompt(t *testing.T) {
	m := raceModel(t)
	armA, err := assembleRaceArm(m.app, m.app.config.Tiers[0], &racedProvider{}, agent.NopObserver{})
	if err != nil {
		t.Fatal(err)
	}
	armB, err := assembleRaceArm(m.app, m.app.config.Tiers[1], &racedProvider{}, agent.NopObserver{})
	if err != nil {
		t.Fatal(err)
	}
	armA.status, armB.status = "completed", "completed"
	run := &raceRun{typed: "compare with " + testGitHubToken, arms: [2]*raceArm{armA, armB}}
	m.race = run
	m.busy = true

	winnerPath := armA.sess.Path()
	m.finishRace(run, "a", "a")
	log, err := os.ReadFile(winnerPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(log), testGitHubToken) {
		t.Error("the race record carried the raw key into the log")
	}
	if !strings.Contains(string(log), "[redacted: a GitHub token]") {
		t.Error("the record does not say a key stood in the prompt")
	}
}

// The headless surface has no one to ask, so the answer is no, with the
// widening flag named — and the refusal itself never quotes the key.
func TestRefuseLeakedSecrets(t *testing.T) {
	if err := refuseLeakedSecrets("a clean prompt", false); err != nil {
		t.Errorf("a clean prompt was refused: %v", err)
	}
	if err := refuseLeakedSecrets("key: "+testGitHubToken, true); err != nil {
		t.Errorf("-allow-secrets did not widen the gate: %v", err)
	}
	err := refuseLeakedSecrets("key: "+testGitHubToken, false)
	if err == nil {
		t.Fatal("a token rode through a -p prompt unchallenged")
	}
	if !strings.Contains(err.Error(), "-allow-secrets") {
		t.Errorf("the refusal does not name the deliberate widening: %v", err)
	}
	if strings.Contains(err.Error(), testGitHubToken) {
		t.Errorf("the refusal quotes the secret: %v", err)
	}
}
