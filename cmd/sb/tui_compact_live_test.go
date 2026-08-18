package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/provider/ollama"
)

// summarize against a real local model: the one call /compact makes outside
// the loop, so the one that no loop test covers. Free to run, but it needs a
// server, so it is gated like every live test here.
func TestLiveSummarizeCompressesAConversation(t *testing.T) {
	if os.Getenv("SB_LIVE") == "" {
		t.Skip("set SB_LIVE=1 to run against a local Ollama server")
	}

	client := ollama.New()
	target := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "qwen3.5:9b-mlx"}

	messages := []provider.Message{
		provider.UserText("Rename the config loader's Load function to LoadDefault across the repo."),
		{Role: provider.RoleAssistant, Content: []provider.Block{provider.Text{
			Text: "Renamed Load to LoadDefault in internal/config/config.go and updated the three call sites in cmd/sb/main.go. Tests pass.",
		}}},
		provider.UserText("Now the REPL flag parsing is broken, -tiers panics."),
		{Role: provider.RoleAssistant, Content: []provider.Block{provider.Text{
			Text: "The panic was a nil catalog in listTiers, not the rename. Fixed by loading the catalog before the flag dispatch. Verified -tiers prints the ladder.",
		}}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	summary, err := summarize(ctx, client, target, messages, "")
	if err != nil {
		t.Fatalf("summarize failed against a live server: %v", err)
	}

	// A summary that never mentions the function being renamed did not
	// summarize this conversation.
	if !strings.Contains(summary, "LoadDefault") {
		t.Errorf("summary lost the central identifier:\n%s", summary)
	}
	if len(summary) < 80 {
		t.Errorf("suspiciously short summary (%d bytes):\n%s", len(summary), summary)
	}
}
