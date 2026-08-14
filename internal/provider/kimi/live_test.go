package kimi

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cj-vana/switchboard/internal/credential"
	"github.com/cj-vana/switchboard/internal/provider"
)

const liveModel = "k3-256k"

func requireLive(t *testing.T) *provider.Provider {
	t.Helper()
	if os.Getenv("SB_LIVE") == "" {
		t.Skip("set SB_LIVE=1 to run against the live endpoint")
	}
	secret, err := credential.Chain(credential.Settings{}).Get(
		context.Background(), credential.Ref{Provider: Name, Account: Surface})
	if err != nil {
		t.Skipf("no credential: %v", err)
	}
	var p provider.Provider = New(secret.Expose())
	return &p
}

// This endpoint serves the Messages API, so the Anthropic adapter drives it.
// That is a claim about someone else's server, and it holds only as long as
// they keep serving that format.
func TestLiveKimiDrivesAToolCall(t *testing.T) {
	c := *requireLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	target := Target(liveModel)
	s, err := c.Stream(ctx, target, provider.Request{
		System:   []provider.Block{provider.Text{Text: "Use the read tool when asked about a file."}},
		Messages: []provider.Message{provider.UserText("What does main.go print? Use the read tool.")},
		Tools: []provider.ToolDefinition{{
			Name:        "read",
			Description: "Read a file from disk",
			Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var sawTool, sawDone bool
	var signature string
	for {
		ev, err := s.Next()
		if err != nil {
			break
		}
		switch ev.Type {
		case provider.EventThinkingDelta:
			if ev.Signature != "" {
				signature = ev.Signature
			}
		case provider.EventToolUse:
			sawTool = true
			if !json.Valid(ev.ToolUse.Input) {
				t.Errorf("tool input is not valid JSON: %s", ev.ToolUse.Input)
			}
		case provider.EventDone:
			sawDone = true
			if ev.StopReason != provider.StopToolUse && sawTool {
				t.Errorf("StopReason = %q on a turn that emitted a call", ev.StopReason)
			}
			if ev.Usage.OutputTokens == 0 {
				t.Error("the terminal event carried no output token count")
			}
		}
	}
	if !sawTool {
		t.Error("the model was asked to use a tool and did not")
	}
	if !sawDone {
		t.Error("the stream ended without a terminal event")
	}
	// This model always reasons, so an unsigned thinking block would fail replay
	// on the next turn the same way it does on Anthropic.
	if signature == "" {
		t.Error("thinking arrived unsigned, so it cannot be replayed")
	}
}

// Caching here is automatic: no marker is sent and a read still appears on a
// second turn with the same prefix. The catalog records that, and this is what
// would notice if it stopped being true.
func TestLiveKimiCachesWithoutAMarker(t *testing.T) {
	c := *requireLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	prefix := strings.Repeat(
		"You are a precise assistant. Follow the tool schema exactly. "+
			"Do not invent file paths. Prefer the smallest correct answer. ", 420)
	req := provider.Request{
		System:   []provider.Block{provider.Text{Text: prefix}},
		Messages: []provider.Message{provider.UserText("Reply with the single word OK.")},
	}

	usage := func() provider.Usage {
		s, err := c.Stream(ctx, Target(liveModel), req)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		var last provider.Usage
		for {
			ev, err := s.Next()
			if err != nil {
				return last
			}
			if ev.Type == provider.EventDone {
				last = ev.Usage
			}
		}
	}

	first := usage()
	second := usage()

	if first.CacheWriteTokens == 0 && second.CacheReadTokens == 0 {
		t.Errorf("no cache activity across two identical prefixes: first %+v, second %+v\n"+
			"the catalog claims this surface caches automatically", first, second)
	}
}
