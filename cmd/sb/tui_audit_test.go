package main

import (
	"crypto/sha256"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/checkpoint"
	"github.com/switchboard-code/switchboard/internal/provider"
)

func assistantCall(id, name, input string) provider.Message {
	return provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{
		provider.ToolUse{ID: id, Name: name, Input: json.RawMessage(input)},
	}}
}

func toolResult(id, name, content string, failed bool) provider.Message {
	return provider.Message{Role: provider.RoleUser, Content: []provider.Block{
		provider.ToolResult{ToolUseID: id, Name: name, Content: content, IsError: failed},
	}}
}

func assistantText(text string) provider.Message {
	return provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{provider.Text{Text: text}}}
}

// The claim under audit is this turn's, so evidence from the turn before it
// would put the agent's earlier work in the dock for what it said now.
func TestAuditReadsTheLastTurnAndNotTheOneBeforeIt(t *testing.T) {
	messages := []provider.Message{
		provider.UserText("first ask"),
		assistantCall("c1", "exec", `{"command":["go","build"]}`),
		toolResult("c1", "exec", "ok", false),
		assistantText("built it"),
		provider.UserText("second ask"),
		assistantCall("c2", "edit", `{"path":"main.go"}`),
		toolResult("c2", "edit", "edited", false),
		assistantText("edited main.go"),
	}

	e, err := gatherAuditEvidence(messages, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.calls) != 1 || e.calls[0].name != "edit" {
		t.Fatalf("calls = %+v, want only this turn's edit", e.calls)
	}
	if e.closing != "edited main.go" {
		t.Errorf("closing = %q, want this turn's closing message", e.closing)
	}
	if e.opening != "second ask" {
		t.Errorf("opening = %q, want the message that opened this turn", e.opening)
	}
}

// A tool call that failed is the evidence a claim of success fails against, so
// losing the error would make the audit agree with exactly the wrong turns.
func TestAuditKeepsAFailedResultAsFailed(t *testing.T) {
	messages := []provider.Message{
		provider.UserText("run the tests"),
		assistantCall("c1", "exec", `{"command":["go","test","./..."]}`),
		toolResult("c1", "exec", "FAIL\tpkg/thing\t0.2s", true),
		assistantText("tests pass"),
	}

	e, err := gatherAuditEvidence(messages, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.calls) != 1 || !e.calls[0].failed {
		t.Fatalf("calls = %+v, want one call recorded as failed", e.calls)
	}

	packet, _ := renderAuditEvidence(e)
	if !strings.Contains(packet, "ERROR") {
		t.Errorf("the packet does not mark the failure:\n%s", packet)
	}
	if !strings.Contains(packet, "tests pass") {
		t.Errorf("the packet does not carry the claim:\n%s", packet)
	}
}

// A turn that was interrupted leaves a call with no result. That is not a
// silent success, and calling it one would invent the evidence.
func TestAuditNamesACallThatNeverReturned(t *testing.T) {
	messages := []provider.Message{
		provider.UserText("do it"),
		assistantCall("c1", "exec", `{"command":["sleep","60"]}`),
	}

	e, err := gatherAuditEvidence(messages, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.calls) != 1 || !e.calls[0].unknown {
		t.Fatalf("calls = %+v, want the call marked as having no recorded result", e.calls)
	}
	if packet, _ := renderAuditEvidence(e); !strings.Contains(packet, "none recorded") {
		t.Errorf("the packet does not say the result is missing:\n%s", packet)
	}
}

// The recorder is the second ledger, and a created file and a modified one are
// different facts about what the turn did.
func TestAuditCarriesTheRecordersCaptures(t *testing.T) {
	dir := t.TempDir()
	created := filepath.Join(dir, "new.go")
	modified := filepath.Join(dir, "old.go")

	rec := checkpoint.NewRecorder()
	rec.Begin("turn")
	rec.RecordState(created, false, 0, nil)
	rec.Commit(created, true, 0o644, sha256.Sum256([]byte("fresh")))
	rec.RecordState(modified, true, 0o644, []byte("before"))
	rec.Commit(modified, true, 0o644, sha256.Sum256([]byte("after")))

	messages := []provider.Message{provider.UserText("write it"), assistantText("wrote it")}
	e, err := gatherAuditEvidence(messages, rec)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.mutations) != 2 {
		t.Fatalf("mutations = %+v, want both captures", e.mutations)
	}
	byPath := map[string]auditMutation{}
	for _, m := range e.mutations {
		byPath[m.path] = m
	}
	if !byPath[created].created {
		t.Errorf("%s was not reported as created", created)
	}
	if byPath[modified].created {
		t.Errorf("%s was reported as created but existed before the turn", modified)
	}
}

// The packet is machine-composed and leaves for a target this turn may never
// have reached, with nobody watching it go.
func TestAuditRedactsBeforeTheEvidenceLeaves(t *testing.T) {
	const token = "sk-ant-api03-JZoUmalVvXBSXFuPPFAdMSFRLXMWZAAgvVPMNXHJIRVwvKAFFDTIJXPXBBRLDXNQ"
	messages := []provider.Message{
		provider.UserText("check the key"),
		assistantCall("c1", "exec", `{"command":["printenv"]}`),
		toolResult("c1", "exec", "ANTHROPIC_API_KEY="+token, false),
		assistantText("read the environment"),
	}

	e, err := gatherAuditEvidence(messages, nil)
	if err != nil {
		t.Fatal(err)
	}
	packet, redacted := renderAuditEvidence(e)
	if strings.Contains(packet, token) {
		t.Errorf("the evidence packet carries a credential:\n%s", packet)
	}
	if redacted == 0 {
		t.Error("the redaction was not counted, so the report cannot say it happened")
	}
}

// Silence about the record's edges reads as coverage. The auditor is told what
// it cannot see, in the same words the user is.
func TestAuditPacketStatesWhatTheRecordDoesNotCover(t *testing.T) {
	messages := []provider.Message{
		provider.UserText("do it"),
		assistantCall("c1", "exec", `{"script":"make build > out.txt"}`),
		toolResult("c1", "exec", "", false),
		assistantText("built"),
	}
	e, err := gatherAuditEvidence(messages, nil)
	if err != nil {
		t.Fatal(err)
	}
	packet, _ := renderAuditEvidence(e)
	if !strings.Contains(packet, "shell command") {
		t.Errorf("the packet does not tell the auditor what the recorder misses:\n%s", packet)
	}
}

// A model checking its own claims is the weakest reading of them, and a report
// that did not say so would present agreement it has no standing to report.
func TestAuditReportSaysWhenItRanOnTheRungThatMadeTheClaims(t *testing.T) {
	e := auditEvidence{label: "the last turn", calls: []auditCall{{name: "edit"}}}
	target := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "test:7b"}

	same := auditReport(e, "AGREED", target, true, 0)
	if !strings.Contains(same, "rung that made the claims") {
		t.Errorf("report does not disclose the weaker reading:\n%s", same)
	}
	if !strings.Contains(same, "auditor") {
		t.Errorf("report does not name the slot that would strengthen it:\n%s", same)
	}

	second := auditReport(e, "AGREED", target, false, 0)
	if strings.Contains(second, "rung that made the claims") {
		t.Errorf("report warns about a second rung it did use:\n%s", second)
	}
}

// AGREED is the auditor's whole vocabulary for "no findings", and a report that
// printed the bare word would read as a failure.
func TestAuditReportRendersAgreementAsASentence(t *testing.T) {
	e := auditEvidence{label: "the last turn", calls: []auditCall{{name: "edit"}}}
	target := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "test:7b"}

	report := auditReport(e, "AGREED", target, false, 0)
	if strings.Contains(report, "\nAGREED") {
		t.Errorf("the sentinel reached the user:\n%s", report)
	}
	if !strings.Contains(report, "agree on what this turn did") {
		t.Errorf("agreement was not stated:\n%s", report)
	}
}

// An empty session has no claim to check, and saying so is cheaper than a
// model call that discovers it.
func TestAuditRefusesASessionWithNoTurn(t *testing.T) {
	if _, err := gatherAuditEvidence(nil, nil); err == nil {
		t.Fatal("an empty session produced evidence")
	}
}

func TestAuditTakesNoArguments(t *testing.T) {
	m := testModel(t)
	cmd := cmdAudit(m, "3")
	if cmd == nil {
		t.Fatal("an argument was accepted silently")
	}
	notice, ok := cmd().(noticeMsg)
	if !ok || !strings.Contains(notice.text, "takes no arguments") {
		t.Errorf("msg = %#v, want the usage refusal", cmd())
	}
}
