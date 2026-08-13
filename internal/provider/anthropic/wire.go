package anthropic

import "encoding/json"

// The Messages API wire format.
//
// Two differences from the OpenAI-compatible format shape the adapter. Cache
// markers are per-block rather than absent, so a cache plan has somewhere to
// land. And usage reports cache reads and writes as counts disjoint from
// input_tokens, where the other format reports cached tokens as a subset of the
// prompt count. Getting that backwards double-counts or loses a whole prefix.

type messagesRequest struct {
	Model     string        `json:"model"`
	MaxTokens int           `json:"max_tokens"`
	Stream    bool          `json:"stream"`
	System    []wireBlock   `json:"system,omitempty"`
	Tools     []wireTool    `json:"tools,omitempty"`
	Messages  []wireMessage `json:"messages"`

	Thinking    *wireThinking `json:"thinking,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
}

// countRequest is the same document without the fields that only make sense for
// generation. The endpoint rejects max_tokens and stream.
type countRequest struct {
	Model    string        `json:"model"`
	System   []wireBlock   `json:"system,omitempty"`
	Tools    []wireTool    `json:"tools,omitempty"`
	Messages []wireMessage `json:"messages"`

	Thinking *wireThinking `json:"thinking,omitempty"`
}

type countResponse struct {
	InputTokens int `json:"input_tokens"`
}

type wireThinking struct {
	Type string `json:"type"`

	// BudgetTokens is required by the "enabled" shape. The newer "adaptive"
	// shape takes no budget and is rejected outright by this model, which is
	// why the adapter asks the target rather than assuming a house style.
	BudgetTokens int `json:"budget_tokens,omitempty"`
}

type wireMessage struct {
	Role    string      `json:"role"`
	Content []wireBlock `json:"content"`
}

// wireBlock is the union of every content block shape. It is one struct with
// omitempty rather than a per-kind type because cache_control can attach to any
// of them, and a marker that silently fails to render is the failure this
// design exists to prevent.
type wireBlock struct {
	Type string `json:"type"`

	Text string `json:"text,omitempty"`

	// Thinking blocks carry a signature the server issues and verifies. Replay
	// without it is rejected, so it is carried through the canonical type
	// rather than discarded on the way in.
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`

	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   any    `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`

	Source *wireSource `json:"source,omitempty"`

	CacheControl *wireCacheControl `json:"cache_control,omitempty"`
}

type wireSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// wireCacheControl marks the end of a cacheable prefix. An absent TTL means the
// five-minute default; "1h" is the only other value the target accepts, and
// both were confirmed against the live API rather than read off a page.
type wireCacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`

	CacheControl *wireCacheControl `json:"cache_control,omitempty"`
}

// streamEvent is the decoded payload of one server-sent event. Every event
// carries its own type in the body, so the SSE `event:` line is redundant and
// the body is what the decoder trusts.
type streamEvent struct {
	Type string `json:"type"`

	Message *struct {
		ID         string     `json:"id"`
		Model      string     `json:"model"`
		StopReason string     `json:"stop_reason"`
		Usage      *wireUsage `json:"usage"`
	} `json:"message"`

	Index        int `json:"index"`
	ContentBlock *struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		Thinking  string          `json:"thinking"`
		Signature string          `json:"signature"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
	} `json:"content_block"`

	Delta *struct {
		Type string `json:"type"`

		Text      string `json:"text"`
		Thinking  string `json:"thinking"`
		Signature string `json:"signature"`

		// PartialJSON accumulates a tool call's input across events, the same
		// problem the OpenAI-compatible adapter solves under a different field
		// name.
		PartialJSON string `json:"partial_json"`

		StopReason string `json:"stop_reason"`
	} `json:"delta"`

	Usage *wireUsage `json:"usage"`

	Error *wireError `json:"error"`
}

// wireUsage reports three disjoint input counts. The prompt's true size is
// their sum, which is what a token estimate has to be compared against.
type wireUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`

	// CacheCreation splits the write by retention, which is the only way to
	// price it: the two TTLs bill at different rates.
	CacheCreation *struct {
		Ephemeral5m int `json:"ephemeral_5m_input_tokens"`
		Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

type wireError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type errorEnvelope struct {
	Type      string     `json:"type"`
	Error     *wireError `json:"error"`
	RequestID string     `json:"request_id"`
}

type modelList struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}
