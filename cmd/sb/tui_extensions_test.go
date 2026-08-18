package main

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/advisor"
)

func TestSplitExtensionActionPreservesSelectorWhitespace(t *testing.T) {
	got := splitExtensionAction("  inspect\t /tmp/a path  with   spaces  ")
	if len(got) != 2 || got[0] != "inspect" || got[1] != "/tmp/a path  with   spaces" {
		t.Fatalf("split action = %#v", got)
	}
	if got := splitExtensionAction(" list "); len(got) != 1 || got[0] != "list" {
		t.Fatalf("single action = %#v", got)
	}
}

func TestExtensionMutationCommandsAreNotBusySafe(t *testing.T) {
	m := testModel(t)
	m.busy = true
	if cmd := cmdMCP(m, "enable claude:docs"); cmd == nil {
		t.Fatal("busy MCP mutation did not return a refusal")
	} else if msg := cmd().(noticeMsg); !strings.Contains(msg.text, "already running") {
		t.Fatalf("busy MCP mutation = %#v", msg)
	}
	for _, command := range commands() {
		if command.name == "plugins" && command.busySafe {
			t.Fatal("plugin mutations remain registry-busy-safe")
		}
	}
	before := strings.Join(m.tr.flat, "\n")
	if cmd := cmdMCP(m, ""); cmd != nil {
		t.Fatal("bare live MCP status returned asynchronous work")
	}
	after := strings.Join(m.tr.flat, "\n")
	if before == after || !strings.Contains(after, "no MCP servers connected") {
		t.Fatalf("bare live MCP status was unavailable while busy:\n%s", after)
	}
}

func TestExtensionActionRunsOffUpdateAndCancellationRejectsResult(t *testing.T) {
	m := testModel(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	cmd := m.startExtensionAction("plugins install", "plugins", func(_ context.Context, w io.Writer) error {
		close(entered)
		<-release
		_, _ = io.WriteString(w, "must not render")
		return nil
	})
	if cmd == nil || !m.busy || !m.operationActive {
		t.Fatalf("action did not claim ownership: cmd=%v busy=%v active=%v", cmd != nil, m.busy, m.operationActive)
	}
	result := make(chan extensionActionMsg, 1)
	go func() { result <- cmd().(extensionActionMsg) }()
	<-entered

	// A blocked action command is not running on Update: ordinary stream
	// messages continue to land while its worker waits.
	m.Update(noticeMsg{text: "stream remains responsive"})
	if transcript := strings.Join(m.tr.flat, "\n"); !strings.Contains(transcript, "stream remains responsive") {
		t.Fatalf("Update was not responsive during action:\n%s", transcript)
	}
	m.interrupt()
	close(release)
	msg := <-result
	m.onExtensionAction(msg)
	if m.busy || m.operationActive {
		t.Fatal("cancelled action retained ownership")
	}
	if transcript := strings.Join(m.tr.flat, "\n"); strings.Contains(transcript, "must not render") {
		t.Fatalf("cancelled result mutated transcript:\n%s", transcript)
	}
}

func TestExtensionActionRejectsStaleSourceSession(t *testing.T) {
	m := testModel(t)
	source := m.app.loop.Session
	cmd := m.startExtensionAction("mcp list", "mcp", func(_ context.Context, w io.Writer) error {
		_, _ = io.WriteString(w, "stale output")
		return nil
	})
	msg := cmd().(extensionActionMsg)
	replacement, err := m.app.store.Create(m.app.workspace, m.app.tier.Target.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	m.app.loop.Session = replacement
	m.onExtensionAction(msg)
	if transcript := strings.Join(m.tr.flat, "\n"); strings.Contains(transcript, "stale output") {
		t.Fatalf("stale source mutated transcript:\n%s", transcript)
	}
	m.app.loop.Session = source
	m.finishOperation(msg.operation, false)
}

func TestExtensionActionCompletionAdvancesQueue(t *testing.T) {
	m := testModel(t)
	m.queue = append(m.queue, "next prompt")
	cmd := m.startExtensionAction("mcp list", "mcp", func(_ context.Context, w io.Writer) error {
		_, _ = io.WriteString(w, "inventory")
		return nil
	})
	next := m.onExtensionAction(cmd().(extensionActionMsg))
	if next == nil || !m.turnPlanning || !m.busy || m.operationActive || len(m.queue) != 0 {
		t.Fatalf("completion/queue: next=%v planning=%v busy=%v operation=%v queue=%#v",
			next != nil, m.turnPlanning, m.busy, m.operationActive, m.queue)
	}
	if transcript := strings.Join(m.tr.flat, "\n"); !strings.Contains(transcript, "inventory") {
		t.Fatalf("owned result missing:\n%s", transcript)
	}
	m.finishPlanning()
}

func TestAdvisorOffCancellationKeepsAdvisorBound(t *testing.T) {
	m := testModel(t)
	adv := advisor.New(nil, nil, m.app.tier.Target, nil)
	m.app.setAdvisor(adv)
	cmd := cmdAdvisor(m, "off")
	if cmd == nil || !m.operationActive || !m.busy {
		t.Fatal("advisor off did not claim exclusive ownership")
	}
	m.interrupt()
	msg := cmd().(advisorReadyMsg)
	m.onAdvisorReady(msg)
	if m.app.currentAdvisor() != adv {
		t.Fatal("cancelled advisor off detached the advisor")
	}
	if m.operationActive || m.busy {
		t.Fatal("cancelled advisor off retained ownership")
	}
}

func TestStaleAdvisorReadyCannotReplaceCurrentState(t *testing.T) {
	m := testModel(t)
	_, generation, sourceID, err := m.startOperation("advisor on")
	if err != nil {
		t.Fatal(err)
	}
	late := advisor.New(nil, nil, m.app.tier.Target, nil)
	m.finishOperation(generation, false)
	current := advisor.New(nil, nil, m.app.tier.Target, nil)
	m.app.setAdvisor(current)
	m.onAdvisorReady(advisorReadyMsg{adv: late, action: "on", operation: generation, sourceID: sourceID})
	if m.app.currentAdvisor() != current {
		t.Fatal("stale advisor result replaced current state")
	}
}

func TestAdvisorOffCancelsPendingOn(t *testing.T) {
	m := testModel(t)
	ctx, generation, sourceID, err := m.startOperation("advisor on")
	if err != nil {
		t.Fatal(err)
	}
	if cmd := cmdAdvisor(m, "off"); cmd != nil {
		t.Fatal("off returned separate work instead of cancelling pending on")
	}
	if !m.operationCancelling || ctx.Err() == nil {
		t.Fatal("pending advisor probe was not cancelled")
	}
	m.onAdvisorReady(advisorReadyMsg{
		action: "on", err: context.Canceled, operation: generation, sourceID: sourceID,
	})
	if m.app.currentAdvisor() != nil || m.operationActive || m.busy {
		t.Fatalf("cancelled pending on changed state: advisor=%v active=%v busy=%v",
			m.app.currentAdvisor() != nil, m.operationActive, m.busy)
	}
}
