package anthropic

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/provider"
)

const liveModel = "claude-haiku-4-5"

// liveAdaptiveModel is the cheapest target in adaptiveThinking. The dialect
// split is the point: liveModel refuses "adaptive" and this one refuses a
// budget, so a single request shape cannot satisfy both and only a live call
// settles which is which.
const liveAdaptiveModel = "claude-sonnet-5"

// requireLive keeps the default `go test ./...` run offline and unbilled. The
// recorded fixtures cover the mapping; these exist to catch the API changing
// underneath them, and to hold the claims the catalog makes about this target
// against the target itself.
func requireLive(t *testing.T) *Client {
	t.Helper()
	if os.Getenv("SB_LIVE") == "" {
		t.Skip("set SB_LIVE=1 to run against the live API (this spends money)")
	}
	secret, err := credential.Chain(credential.Settings{}).Get(
		context.Background(), credential.Ref{Provider: Name, Account: Surface})
	if err != nil {
		t.Skipf("no credential: %v", err)
	}
	return New(WithAPIKey(secret.Expose()))
}

// A prefix long enough to clear the 4,096-token minimum the catalog records.
// Below it the target declines to cache and reports nothing, which is the
// correct result rather than a failure, and would make this test measure
// nothing at all.
func longPrefix() string {
	return strings.Repeat(
		"You are a precise assistant. Follow the tool schema exactly. "+
			"Do not invent file paths. Prefer the smallest correct answer. ", 420)
}

// TestLiveCacheWriteThenRead is the observation §6.3 is built on: a cache entry
// has to be updated from what the provider reported, and a write and a read are
// different events. This is the only target here that can produce either.
func TestLiveCacheWriteThenRead(t *testing.T) {
	c := requireLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	target := Target(liveModel)
	req := provider.Request{
		System:   []provider.Block{provider.Text{Text: longPrefix()}},
		Messages: []provider.Message{provider.UserText("Reply with the single word OK.")},
		CachePlan: &provider.CachePlan{Breakpoints: []provider.Breakpoint{
			{Position: provider.CachePosition{MessageIndex: provider.SystemBlocks, BlockIndex: 0}},
		}},
	}

	first := liveUsage(ctx, t, c, target, req)
	if first.CacheWriteTokens == 0 {
		t.Fatalf("the first call wrote nothing to the cache: %+v\n"+
			"either the breakpoint did not render or the prefix is below the minimum", first)
	}

	second := liveUsage(ctx, t, c, target, req)
	if second.CacheReadTokens == 0 {
		t.Errorf("the second identical call read nothing back: %+v", second)
	}
	if second.CacheReadTokens != first.CacheWriteTokens {
		t.Errorf("wrote %d and read %d; a partial hit means the breakpoint moved between requests",
			first.CacheWriteTokens, second.CacheReadTokens)
	}
	if second.CacheWriteTokens != 0 {
		t.Errorf("the second call wrote %d tokens again, so it missed", second.CacheWriteTokens)
	}
}

// The estimator is 18 to 24 percent low on every other target (docs/estimator.md).
// Here the count comes from the server, so it should agree exactly with what the
// same request is then billed for.
func TestLiveTokenCountMatchesWhatIsBilled(t *testing.T) {
	c := requireLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	target := Target(liveModel)
	req := provider.Request{
		System:   []provider.Block{provider.Text{Text: "You are terse."}},
		Messages: []provider.Message{provider.UserText("Reply with the single word OK.")},
		Tools: []provider.ToolDefinition{{
			Name:        "read",
			Description: "Read a file from disk",
			Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		}},
	}

	est, err := c.CountTokens(ctx, target, req)
	if err != nil {
		t.Fatal(err)
	}
	if !est.Exact {
		t.Error("a count from the server reported itself inexact")
	}

	usage := liveUsage(ctx, t, c, target, req)
	billed := usage.InputTokens + usage.CacheReadTokens + usage.CacheWriteTokens
	if est.InputTokens != billed {
		t.Errorf("counted %d and was billed %d; the counting endpoint and the "+
			"generation endpoint disagree about the same request", est.InputTokens, billed)
	}
}

// The claim adaptiveThinking makes is that these models take the word and
// refuse the budget. The offline tests pin the bytes the adapter builds; only
// this one shows the server accepts them, and it is the test that would fail
// first if a model were added to that map on a guess.
func TestLiveAdaptiveModelAcceptsAnEffortWord(t *testing.T) {
	c := requireLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if !adaptiveThinking[liveAdaptiveModel] {
		t.Fatalf("%s is not in adaptiveThinking, so this test measures the wrong dialect", liveAdaptiveModel)
	}

	target := Target(liveAdaptiveModel)
	target.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: "xhigh"}

	usage := liveUsage(ctx, t, c, target, provider.Request{
		Messages: []provider.Message{provider.UserText("Reply with the single word OK.")},
	})
	if usage.InputTokens == 0 {
		t.Errorf("the call reported no input tokens: %+v", usage)
	}
}

// Extended thinking across a tool call is where the signature matters: the turn
// that follows replays the thinking block, and an unsigned one is refused.
func TestLiveThinkingSurvivesAToolCall(t *testing.T) {
	c := requireLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	target := Target(liveModel)
	target.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: "low"}

	tools := []provider.ToolDefinition{{
		Name:        "read",
		Description: "Read a file from disk",
		Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
	}}
	first := provider.Request{
		System:   []provider.Block{provider.Text{Text: "Use the read tool when asked about a file."}},
		Messages: []provider.Message{provider.UserText("What does main.go print? Use the read tool.")},
		Tools:    tools,
	}

	s, err := c.Stream(ctx, target, first)
	if err != nil {
		t.Fatal(err)
	}
	events, err := drain(t, s)
	s.Close()
	if err != nil {
		t.Fatalf("draining: %v", err)
	}

	// Reassemble the assistant turn the way the agent loop does.
	var thinking strings.Builder
	var signature string
	var use *provider.ToolUse
	for _, ev := range events {
		switch ev.Type {
		case provider.EventThinkingDelta:
			thinking.WriteString(ev.Text)
			if ev.Signature != "" {
				signature = ev.Signature
			}
		case provider.EventToolUse:
			use = ev.ToolUse
		}
	}
	if use == nil {
		t.Skip("the model answered without calling a tool")
	}
	if signature == "" {
		t.Fatal("the thinking block arrived unsigned, so it cannot be replayed")
	}

	second := first
	second.Messages = append(append([]provider.Message{}, first.Messages...),
		provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{
			provider.Thinking{Text: thinking.String(), Signature: signature},
			*use,
		}},
		provider.Message{Role: provider.RoleUser, Content: []provider.Block{
			provider.ToolResult{ToolUseID: use.ID, Name: use.Name, Content: `package main` + "\n\n" + `func main(){println("hi")}`},
		}},
	)

	s2, err := c.Stream(ctx, target, second)
	if err != nil {
		t.Fatalf("replaying a signed thinking block was refused: %v", err)
	}
	defer s2.Close()
	if _, err := drain(t, s2); err != nil {
		t.Fatalf("draining the second turn: %v", err)
	}
}

func liveUsage(ctx context.Context, t *testing.T, c *Client, target provider.RouteTarget, req provider.Request) provider.Usage {
	t.Helper()
	s, err := c.Stream(ctx, target, req)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	events, err := drain(t, s)
	if err != nil {
		t.Fatalf("draining: %v", err)
	}
	return done(t, events).Usage
}
