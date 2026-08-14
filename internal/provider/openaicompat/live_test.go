package openaicompat

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/cj-vana/switchboard/internal/provider"
)

const liveModel = "qwen3.5:9b-mlx"

// requireLive keeps the default `go test ./...` run offline. The recorded
// fixture covers the mapping; this exists to catch the server changing its wire
// format underneath it.
func requireLive(t *testing.T) {
	t.Helper()
	if os.Getenv("SB_LIVE") == "" {
		t.Skip("set SB_LIVE=1 to run against a local Ollama server")
	}
}

// TestLivePortability runs the same request the native adapter's live test
// runs, against the same model on the same machine, through the compatibility
// endpoint. Anything that differs between the two is a portability cost of the
// format rather than a property of the model, which is the thing section 21.3
// asks about and the thing a second cloud provider would otherwise have to be
// paid for to learn.
func TestLivePortability(t *testing.T) {
	requireLive(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	c, err := New("ollama")
	if err != nil {
		t.Fatal(err)
	}
	target, err := Target("ollama", liveModel)
	if err != nil {
		t.Fatal(err)
	}
	target.Params.Reasoning = &provider.Reasoning{Enabled: true}

	if res, err := c.Probe(ctx, target); err != nil {
		t.Fatalf("probe: %v", err)
	} else if !res.ModelPresent {
		t.Skipf("%s is not served: %s", liveModel, res.Detail)
	}

	req := provider.Request{
		System:   []provider.Block{provider.Text{Text: "Use the read tool when asked about a file."}},
		Messages: []provider.Message{provider.UserText("What is in the file main.go?")},
		Tools: []provider.ToolDefinition{{
			Name:        "read",
			Description: "Read a file from disk",
			Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		}},
	}

	s, err := c.Stream(ctx, target, req)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	events, err := drain(t, s)
	if err != nil {
		t.Fatalf("draining live stream: %v", err)
	}

	var sawToolCall, sawDone bool
	for _, ev := range events {
		switch ev.Type {
		case provider.EventToolUse:
			sawToolCall = true
			if !json.Valid(ev.ToolUse.Input) {
				t.Errorf("tool input is not valid JSON: %s", ev.ToolUse.Input)
			}
			if ev.ToolUse.ID == "" {
				t.Error("tool call reached the caller without an ID")
			}
		case provider.EventDone:
			sawDone = true
			if ev.Usage.InputTokens == 0 {
				// stream_options.include_usage is what asks for this. A server
				// that ignores it leaves every turn unpriced, which is worth
				// failing over rather than discovering from a zeroed invoice.
				t.Error("done event carried no input token count")
			}
			if sawToolCall && ev.StopReason != provider.StopToolUse {
				t.Errorf("StopReason = %q on a turn that emitted tool calls", ev.StopReason)
			}
		}
	}
	if !sawToolCall {
		t.Error("the model was asked about a file and called no tool")
	}
	if !sawDone {
		t.Error("the stream ended without a terminal event")
	}
}

// TestLiveNoCacheAccounting pins the claim the catalog makes about this
// surface. The format has a field for cached prompt tokens; if this server
// starts populating it, the catalog entry saying it does not is wrong.
func TestLiveNoCacheAccounting(t *testing.T) {
	requireLive(t)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	c, err := New("ollama")
	if err != nil {
		t.Fatal(err)
	}
	target, err := Target("ollama", liveModel)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := c.Probe(ctx, target); err != nil || !res.ModelPresent {
		t.Skipf("%s is not served", liveModel)
	}

	req := provider.Request{Messages: []provider.Message{provider.UserText("Say OK.")}}
	for range 2 {
		s, err := c.Stream(ctx, target, req)
		if err != nil {
			t.Fatal(err)
		}
		events, err := drain(t, s)
		s.Close()
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range events {
			if ev.Type == provider.EventDone && ev.Usage.CacheReadTokens != 0 {
				t.Errorf("this server now reports %d cached prompt tokens; "+
					"the catalog entry for openaicompat/ollama claims none and needs updating",
					ev.Usage.CacheReadTokens)
			}
		}
	}
}
