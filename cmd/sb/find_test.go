package main

import (
	"strings"
	"testing"

	"github.com/cj-vana/switchboard/internal/provider"
	"github.com/cj-vana/switchboard/internal/session"
)

// /find greps what was said. It matches the conversation — user and
// assistant text — case-insensitively, hands back the id /resume takes,
// and stays quiet about tool payloads, because the question is about the
// conversation.
func TestFindSearchesTheConversation(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	target := provider.RouteTargetID("ollama/local/test:7b")

	hit, err := store.Create(workspace, target, "test")
	if err != nil {
		t.Fatal(err)
	}
	hit.AppendMessage(provider.UserText("make the runner test deterministic"))
	hit.AppendMessage(provider.Message{Role: provider.RoleAssistant,
		Content: []provider.Block{provider.Text{Text: "The Runner race is a wait on the process group."}}})
	hitID := hit.State().ID
	hit.Close()

	miss, err := store.Create(workspace, target, "test")
	if err != nil {
		t.Fatal(err)
	}
	miss.AppendMessage(provider.UserText("rename the config field"))
	miss.Close()

	out := strings.Join(findLines(store, workspace, "runner"), "\n")
	if !strings.Contains(out, hitID) {
		t.Fatalf("the matching session's id is absent:\n%s", out)
	}
	if !strings.Contains(out, "2 matches") {
		t.Fatalf("case-insensitive matching missed a hit:\n%s", out)
	}
	if !strings.Contains(out, "/resume") {
		t.Fatalf("the way to pick a session up is not stated:\n%s", out)
	}
	if strings.Contains(out, miss.State().ID) {
		t.Fatalf("a session that never said it was listed:\n%s", out)
	}

	none := strings.Join(findLines(store, workspace, "zeppelin"), "\n")
	if !strings.Contains(none, "nothing in 2 sessions") {
		t.Fatalf("an empty result did not say so:\n%s", none)
	}
}
