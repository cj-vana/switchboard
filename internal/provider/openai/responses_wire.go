package openai

import "encoding/json"

// The Responses API wire format, as this endpoint actually speaks it.
//
// It is a third shape, not a variant of the other two. Messages are "items"
// rather than messages, a tool call is an item in the output rather than a
// field on a message, and a tool result is an item in the input rather than a
// message with a role. Two ids travel with every call and they are not
// interchangeable.

type responsesRequest struct {
	Model  string          `json:"model"`
	Stream bool            `json:"stream"`
	Input  []responsesItem `json:"input"`

	// Instructions carries the system prompt. A message item with role "system"
	// is rejected outright here ("System messages are not allowed"), so this is
	// not a stylistic choice between two places to put it.
	Instructions string `json:"instructions,omitempty"`

	// Store must be false. The endpoint refuses the request outright when it is
	// not, which is a reasonable default to be forced into: server-side
	// retention of a coding session is not something to opt into silently.
	Store bool `json:"store"`

	Tools     []responsesTool     `json:"tools,omitempty"`
	Reasoning *responsesReasoning `json:"reasoning,omitempty"`

	MaxOutputTokens int      `json:"max_output_tokens,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`

	// PromptCacheKey is a caller-supplied cache routing key. No other target
	// here accepts one; §6.2 wants exactly this for cache affinity.
	PromptCacheKey string `json:"prompt_cache_key,omitempty"`
}

type responsesReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// responsesItem is the union of everything that can appear in the input list.
//
// A plain message carries content parts. A function_call replays a call the
// model made, and a function_call_output returns its result. The last two have
// no role, which is why this is not a message type with a role field.
type responsesItem struct {
	Type string `json:"type"`

	// Message fields.
	Role    string             `json:"role,omitempty"`
	Content []responsesContent `json:"content,omitempty"`

	// function_call fields. ID is the item's own identifier and CallID is what
	// a result refers to. Sending one where the other belongs is accepted and
	// then fails to correlate, so they are kept apart deliberately.
	ID        string `json:"id,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Status    string `json:"status,omitempty"`

	// function_call_output fields.
	Output string `json:"output,omitempty"`
}

type responsesContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`

	ImageURL string `json:"image_url,omitempty"`
}

// responsesTool is flat. The chat-completions format nests name and parameters
// under a "function" object and this one does not, which is the kind of
// difference that makes "OpenAI-compatible" not mean very much.
type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      bool            `json:"strict,omitempty"`
}

// responsesEvent is one server-sent event. Every event names its own type, and
// the terminal one carries the whole response object including usage.
type responsesEvent struct {
	Type string `json:"type"`

	// Delta carries incremental text, whether output text or tool arguments.
	Delta string `json:"delta"`

	ItemID      string `json:"item_id"`
	OutputIndex int    `json:"output_index"`

	Item *struct {
		ID        string `json:"id"`
		Type      string `json:"type"`
		Status    string `json:"status"`
		Name      string `json:"name"`
		CallID    string `json:"call_id"`
		Arguments string `json:"arguments"`
	} `json:"item"`

	Response *struct {
		ID     string          `json:"id"`
		Status string          `json:"status"`
		Usage  *responsesUsage `json:"usage"`
		Error  *struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	} `json:"response"`

	// Detail is what this endpoint returns for a rejected request, in place of
	// the nested error object the documented API uses.
	Detail string `json:"detail"`
}

// responsesUsage reports cached and written prompt tokens under details, and
// unlike the chat-completions shape it reports a write count at all.
type responsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	InputTokensDetails *struct {
		CachedTokens     int `json:"cached_tokens"`
		CacheWriteTokens int `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

// codexModelList is the discovery endpoint's answer. It carries per-model
// capabilities, which the chat-completions /models does not, so a probe here
// can report what a model actually does instead of what a profile guesses.
type codexModelList struct {
	Models []struct {
		Slug            string   `json:"slug"`
		InputModalities []string `json:"input_modalities"`
	} `json:"models"`
}
