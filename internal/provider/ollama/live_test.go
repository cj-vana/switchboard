package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/provider"
)

// liveModel is small enough to answer quickly and advertises tool support.
const liveModel = "qwen3.5:9b-mlx"

// requireLive keeps the default `go test ./...` run offline. Recorded fixtures
// cover the mapping; this exists to catch the server changing its wire format
// underneath them.
func requireLive(t *testing.T) {
	t.Helper()
	if os.Getenv("SB_LIVE") == "" {
		t.Skip("set SB_LIVE=1 to run against a local Ollama server")
	}
}

// TestLiveVision sends a generated solid-red PNG and asks its color: the
// round trip that proves an image block reaches the model itself, not just
// the wire format. This is the live verification behind letting an @mention
// attach an image on a probed local target.
func TestLiveVision(t *testing.T) {
	requireLive(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	c := New()
	target := Target(liveModel)
	res, err := c.Probe(ctx, target)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !res.ModelPresent {
		t.Skipf("%s is not pulled: %s", liveModel, res.Detail)
	}
	if !res.Vision {
		t.Skipf("%s does not advertise vision", liveModel)
	}

	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{R: 255, A: 255}}, image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}

	req := provider.Request{
		Messages: []provider.Message{{
			Role: provider.RoleUser,
			Content: []provider.Block{
				provider.Text{Text: "In one word: what color is this image?"},
				provider.Image{MediaType: "image/png", Data: buf.Bytes()},
			},
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
	var text strings.Builder
	for _, ev := range events {
		if ev.Type == provider.EventTextDelta {
			text.WriteString(ev.Text)
		}
	}
	if !strings.Contains(strings.ToLower(text.String()), "red") {
		t.Errorf("the model did not see the image: asked the color of solid red, answered %q", text.String())
	}
}

func TestLiveToolCall(t *testing.T) {
	requireLive(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	c := New()
	target := Target(liveModel)
	target.Params.Reasoning = &provider.Reasoning{Enabled: true}

	if res, err := c.Probe(ctx, target); err != nil {
		t.Fatalf("probe: %v", err)
	} else if !res.ModelPresent {
		t.Skipf("%s is not pulled: %s", liveModel, res.Detail)
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

	var sawThinking, sawToolCall, sawDone bool
	for _, ev := range events {
		switch ev.Type {
		case provider.EventThinkingDelta:
			sawThinking = true
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
				t.Error("done event carried no input token count")
			}
			if sawToolCall && ev.StopReason != provider.StopToolUse {
				t.Errorf("StopReason = %q after a tool call, want tool_use", ev.StopReason)
			}
		}
	}

	if !sawDone {
		t.Error("stream never produced a done event")
	}
	if !sawThinking {
		t.Error("reasoning was requested but no thinking arrived")
	}
	if !sawToolCall {
		t.Error("model did not call the tool; the loop cannot be driven by this target")
	}
}
