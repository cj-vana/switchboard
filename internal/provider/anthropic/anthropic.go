// Package anthropic binds the Messages API as a route target.
//
// It is the first adapter here that can honor a cache plan, which is the whole
// reason it exists: §6 needs a target that reports cache reads and writes as
// separate observations, and no local server does. Everything the adapter
// claims about the target was confirmed against the live API and recorded in
// testdata, not read off a documentation page.
package anthropic

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/switchboard-code/switchboard/internal/provider"
)

const (
	Name    = "anthropic"
	Surface = "first-party"

	defaultBaseURL = "https://api.anthropic.com"

	// apiVersion is a required header. It pins the wire format independently of
	// the model, so a new model does not silently change the shapes below.
	apiVersion = "2023-06-01"

	// defaultMaxTokens is sent when the caller names no output limit, because
	// the API requires the field. It is not a claim about the model's ceiling:
	// the catalog owns that, and this package does not read the catalog.
	defaultMaxTokens = 8192
)

type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client

	// provider names who this client speaks for. Other vendors serve this exact
	// wire format, and a target reached from one of them is that vendor's
	// target: its own credential, its own catalog entry, its own price. Empty
	// means Anthropic itself.
	provider string
}

type Option func(*Client)

func WithBaseURL(raw string) Option {
	return func(c *Client) {
		if raw != "" {
			c.baseURL = strings.TrimSuffix(strings.TrimSpace(raw), "/")
		}
	}
}

// WithAPIKey supplies the credential. It is passed in rather than read from the
// environment here, so credential resolution stays in one place (§5.3).
func WithAPIKey(key string) Option {
	return func(c *Client) { c.apiKey = key }
}

func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http = h }
}

// WithProvider names the vendor this client serves, for a compatible endpoint
// that is not Anthropic. It changes attribution only: what the adapter sends
// and how it reads a response are properties of the format, not of who serves
// it.
func WithProvider(name string) Option {
	return func(c *Client) { c.provider = name }
}

func New(opts ...Option) *Client {
	c := &Client{
		baseURL: defaultBaseURL,
		// No overall timeout: a long generation is not a stuck connection, and
		// the caller's context governs cancellation.
		http: &http.Client{},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func (c *Client) Name() string {
	if c.provider != "" {
		return c.provider
	}
	return Name
}

func Target(model string) provider.RouteTarget {
	return provider.RouteTarget{Provider: Name, Surface: Surface, ModelID: model}
}

func (c *Client) Stream(ctx context.Context, target provider.RouteTarget, req provider.Request) (provider.EventStream, error) {
	body, err := c.buildRequest(target, req, true)
	if err != nil {
		return nil, provider.MarkUnissued(err)
	}

	resp, err := c.do(ctx, "/v1/messages", body)
	if err != nil {
		return nil, err
	}
	return newStream(ctx, resp.Body), nil
}

// CountTokens asks the server.
//
// This is the only target here that can answer exactly. The other two estimate
// from character counts and are measurably wrong in the dangerous direction
// (docs/estimator.md), so where an exact answer is available it is worth a round
// trip: a budget check is only as good as the number under it.
func (c *Client) CountTokens(ctx context.Context, target provider.RouteTarget, req provider.Request) (provider.TokenEstimate, error) {
	body, err := c.buildRequest(target, req, false)
	if err != nil {
		return provider.TokenEstimate{}, err
	}

	resp, err := c.do(ctx, "/v1/messages/count_tokens", body)
	if err != nil {
		return provider.TokenEstimate{}, err
	}
	defer resp.Body.Close()

	var counted countResponse
	if err := json.NewDecoder(resp.Body).Decode(&counted); err != nil {
		return provider.TokenEstimate{}, &provider.ProtocolError{
			Provider: c.Name(), Detail: "decoding a token count", Err: err}
	}
	return provider.TokenEstimate{InputTokens: counted.InputTokens, Exact: true}, nil
}

func (c *Client) Probe(ctx context.Context, target provider.RouteTarget) (provider.ProbeResult, error) {
	var res provider.ProbeResult

	resp, err := c.do(ctx, "/v1/models?limit=1000", nil)
	if err != nil {
		var apiErr *provider.APIError
		// An API that answered with an error is reachable. Only a transport
		// failure means nothing responded.
		res.Reachable = errors.As(err, &apiErr)
		res.Detail = err.Error()
		return res, nil
	}
	defer resp.Body.Close()

	var list modelList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return res, &provider.ProtocolError{Provider: c.Name(), Detail: "decoding /v1/models", Err: err}
	}
	res.Reachable = true
	for _, m := range list.Data {
		// The list carries dated snapshots; an alias such as claude-haiku-4-5
		// resolves to one, so a prefix match is what answers "can this target
		// be served" without pinning the caller to a snapshot they did not ask
		// for.
		if m.ID == target.ModelID || strings.HasPrefix(m.ID, target.ModelID) {
			res.ModelPresent = true
			break
		}
	}
	if !res.ModelPresent {
		res.Detail = fmt.Sprintf("model %q is not offered to this account", target.ModelID)
		return res, nil
	}
	res.Tools = provider.ToolsParallel
	return res, nil
}

func (c *Client) do(ctx context.Context, path string, body []byte) (*http.Response, error) {
	method := http.MethodPost
	var reader io.Reader
	if body == nil {
		method = http.MethodGet
	} else {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("anthropic-version", apiVersion)
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("x-api-key", c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		resp.Body.Close()
		return nil, &provider.APIError{
			Provider:   c.Name(),
			StatusCode: resp.StatusCode,
			Body:       errorMessage(raw),
		}
	}
	return resp, nil
}

// errorMessage unwraps the nested error shape, so a caller sees the server's
// sentence rather than a JSON document.
func errorMessage(raw []byte) string {
	var env errorEnvelope
	if err := json.Unmarshal(raw, &env); err == nil && env.Error != nil && env.Error.Message != "" {
		return env.Error.Message
	}
	return strings.TrimSpace(string(raw))
}

func (c *Client) buildRequest(target provider.RouteTarget, req provider.Request, stream bool) ([]byte, error) {
	system, err := blocksToWire(target, req.System)
	if err != nil {
		return nil, err
	}
	messages, err := messagesToWire(target, req.Messages)
	if err != nil {
		return nil, err
	}
	tools := toolsToWire(req.Tools)

	thinking, err := thinkingFor(target)
	if err != nil {
		return nil, err
	}

	maxTokens := target.Params.MaxOutputTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	if thinking != nil && maxTokens <= thinking.BudgetTokens {
		// The API requires max_tokens to exceed the thinking budget. Raising it
		// here rather than failing keeps a caller who asked only for "high
		// effort" from having to know that rule, and the ceiling is still the
		// caller's if they set one large enough.
		maxTokens = thinking.BudgetTokens + defaultMaxTokens
	}

	temperature := target.Params.Temperature
	if thinking != nil && temperature != nil {
		return nil, &provider.CapabilityError{
			Target:     target.ID(),
			Capability: "temperature with extended thinking",
			Detail:     "this target rejects a temperature other than the default while thinking is enabled; drop one of the two rather than have the adapter pick",
		}
	}

	if err := applyCachePlan(target, req.CachePlan, system, tools, messages); err != nil {
		return nil, err
	}

	if stream {
		return json.Marshal(messagesRequest{
			Model:       target.ModelID,
			MaxTokens:   maxTokens,
			Stream:      true,
			System:      system,
			Tools:       tools,
			Messages:    messages,
			Thinking:    thinking,
			Temperature: temperature,
		})
	}
	// The counting endpoint rejects max_tokens and stream, so it takes the same
	// document minus the fields that only mean something for generation.
	return json.Marshal(countRequest{
		Model:    target.ModelID,
		System:   system,
		Tools:    tools,
		Messages: messages,
		Thinking: thinking,
	})
}

// effortBudgets maps the ladder's effort names onto thinking budgets.
//
// This target takes a token budget rather than an effort word: "adaptive" is
// rejected outright, which was confirmed against the live API. The mapping is
// therefore this adapter's policy and is named as such, not presented as
// something the provider defines.
var effortBudgets = map[string]int{
	"low":    1024,
	"medium": 4096,
	"high":   16384,
	"max":    32768,
}

func thinkingFor(target provider.RouteTarget) (*wireThinking, error) {
	r := target.Params.Reasoning
	if r == nil || !r.Enabled {
		return nil, nil
	}
	if r.Effort == "" {
		return &wireThinking{Type: "enabled", BudgetTokens: effortBudgets["medium"]}, nil
	}
	budget, ok := effortBudgets[r.Effort]
	if !ok {
		return nil, &provider.CapabilityError{
			Target:     target.ID(),
			Capability: "reasoning effort " + r.Effort,
			Detail:     "known efforts are low, medium, high, and max",
		}
	}
	return &wireThinking{Type: "enabled", BudgetTokens: budget}, nil
}

func messagesToWire(target provider.RouteTarget, msgs []provider.Message) ([]wireMessage, error) {
	out := make([]wireMessage, 0, len(msgs))
	for _, m := range msgs {
		blocks, err := blocksToWire(target, m.Content)
		if err != nil {
			return nil, err
		}
		role, err := roleToWire(target, m.Role)
		if err != nil {
			return nil, err
		}
		out = append(out, wireMessage{Role: role, Content: blocks})
	}
	return out, nil
}

// roleToWire maps the canonical roles onto the two this API accepts.
//
// There is no tool role here: a tool result is a user message carrying
// tool_result blocks, which is a genuine difference from the formats that give
// results a role of their own. Sending "tool" is rejected outright, so the
// canonical form is translated rather than passed through.
func roleToWire(target provider.RouteTarget, role provider.Role) (string, error) {
	switch role {
	case provider.RoleUser, provider.RoleTool:
		return "user", nil
	case provider.RoleAssistant:
		return "assistant", nil
	default:
		return "", &provider.CapabilityError{
			Target:     target.ID(),
			Capability: fmt.Sprintf("message role %q", role),
			Detail:     "this target accepts only user and assistant messages; a system prompt goes in the system field",
		}
	}
}

func blocksToWire(target provider.RouteTarget, blocks []provider.Block) ([]wireBlock, error) {
	if len(blocks) == 0 {
		return nil, nil
	}
	out := make([]wireBlock, 0, len(blocks))
	for _, b := range blocks {
		switch v := b.(type) {
		case provider.Text:
			out = append(out, wireBlock{Type: "text", Text: v.Text})

		case provider.Thinking:
			if v.Signature == "" {
				// The server verifies its own attestation and rejects a block
				// whose signature is missing. Dropping the block is allowed and
				// replaying it unsigned is not, so an unsigned block is dropped
				// rather than sent to be refused.
				continue
			}
			out = append(out, wireBlock{Type: "thinking", Thinking: v.Text, Signature: v.Signature})

		case provider.ToolUse:
			out = append(out, wireBlock{Type: "tool_use", ID: v.ID, Name: v.Name, Input: v.Input})

		case provider.ToolResult:
			out = append(out, wireBlock{
				Type:      "tool_result",
				ToolUseID: v.ToolUseID,
				Content:   v.Content,
				IsError:   v.IsError,
			})

		case provider.Image:
			out = append(out, wireBlock{Type: "image", Source: &wireSource{
				Type: "base64", MediaType: v.MediaType, Data: base64.StdEncoding.EncodeToString(v.Data),
			}})

		default:
			return nil, &provider.CapabilityError{
				Target:     target.ID(),
				Capability: fmt.Sprintf("content block %q", b.Kind()),
				Detail:     "this adapter has no wire form for that block",
			}
		}
	}
	return out, nil
}

func toolsToWire(tools []provider.ToolDefinition) []wireTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]wireTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, wireTool{Name: t.Name, Description: t.Description, InputSchema: t.Schema})
	}
	return out
}

// maxBreakpoints is the target's limit, and exceeding it is an error rather
// than a silent truncation: a dropped marker is a cache miss the caller was
// billed to discover.
const maxBreakpoints = 4

// applyCachePlan attaches markers at the canonical positions the breakpoint
// manager chose.
//
// The plan addresses positions, not blocks, so this is where an abstract
// "cache after the tool definitions" becomes a field on a particular wire
// block. Anything that cannot be placed exactly is refused, because a marker
// that lands one block off caches a different prefix than the one whose reuse
// was scored.
func applyCachePlan(target provider.RouteTarget, plan *provider.CachePlan, system []wireBlock, tools []wireTool, messages []wireMessage) error {
	if plan == nil {
		return nil
	}
	if plan.RoutingKey != "" {
		return &provider.CapabilityError{Target: target.ID(), Capability: "cache routing key",
			Detail: "the Anthropic Messages API accepts cache breakpoints, not a prompt routing key"}
	}
	if len(plan.Breakpoints) == 0 {
		return nil
	}
	if len(plan.Breakpoints) > maxBreakpoints {
		return &provider.CapabilityError{
			Target:     target.ID(),
			Capability: fmt.Sprintf("%d cache breakpoints", len(plan.Breakpoints)),
			Detail:     fmt.Sprintf("this target accepts at most %d", maxBreakpoints),
		}
	}

	for _, bp := range plan.Breakpoints {
		control, err := cacheControlFor(target, bp.TTL)
		if err != nil {
			return err
		}
		pos := bp.Position

		switch {
		case pos.MessageIndex == provider.SystemBlocks:
			if pos.BlockIndex < 0 || pos.BlockIndex >= len(system) {
				return outOfRange(target, "system blocks", pos.BlockIndex, len(system))
			}
			system[pos.BlockIndex].CacheControl = control

		case pos.MessageIndex == provider.ToolDefinitions:
			if pos.BlockIndex < 0 || pos.BlockIndex >= len(tools) {
				return outOfRange(target, "tool definitions", pos.BlockIndex, len(tools))
			}
			tools[pos.BlockIndex].CacheControl = control

		default:
			if pos.MessageIndex < 0 || pos.MessageIndex >= len(messages) {
				return outOfRange(target, "messages", pos.MessageIndex, len(messages))
			}
			content := messages[pos.MessageIndex].Content
			if pos.BlockIndex < 0 || pos.BlockIndex >= len(content) {
				return outOfRange(target, fmt.Sprintf("message %d", pos.MessageIndex), pos.BlockIndex, len(content))
			}
			content[pos.BlockIndex].CacheControl = control
		}
	}
	return nil
}

func outOfRange(target provider.RouteTarget, where string, index, length int) error {
	return &provider.CapabilityError{
		Target:     target.ID(),
		Capability: "cache breakpoint placement",
		Detail: fmt.Sprintf("position %d in %s, which holds %d", index, where, length) +
			"; a marker that cannot be placed exactly would cache a different prefix than the one that was scored",
	}
}

// cacheControlFor maps a requested retention onto what the target sells. Both
// values were confirmed against the live API, including that the longer one
// needs no beta header and lands in its own billing bucket.
func cacheControlFor(target provider.RouteTarget, ttl time.Duration) (*wireCacheControl, error) {
	switch ttl {
	case 0, 5 * time.Minute:
		return &wireCacheControl{Type: "ephemeral"}, nil
	case time.Hour:
		return &wireCacheControl{Type: "ephemeral", TTL: "1h"}, nil
	default:
		return nil, &provider.CapabilityError{
			Target:     target.ID(),
			Capability: fmt.Sprintf("cache retention of %s", ttl),
			Detail:     "this target sells 5m and 1h; rounding to the nearer one would bill a rate nobody chose",
		}
	}
}
