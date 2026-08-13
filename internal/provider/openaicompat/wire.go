package openaicompat

import "encoding/json"

// The OpenAI chat-completions wire format. The differences from a native API
// are the point of this adapter: tool arguments travel as an escaped JSON
// string rather than an object, reasoning arrives under its own field name
// that varies by server, and usage comes in a final chunk with no choices.

type chatRequest struct {
	Model         string         `json:"model"`
	Messages      []wireMessage  `json:"messages"`
	Tools         []wireTool     `json:"tools,omitempty"`
	Stream        bool           `json:"stream"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`

	MaxTokens       *int     `json:"max_tokens,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
	ReasoningEffort string   `json:"reasoning_effort,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content,omitempty"`

	// Reasoning is echoed back on replay where the server understands it.
	Reasoning string `json:"reasoning,omitempty"`

	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireToolCall struct {
	ID    string `json:"id,omitempty"`
	Index int    `json:"index"`
	Type  string `json:"type,omitempty"`

	Function wireToolCallFunc `json:"function"`
}

type wireToolCallFunc struct {
	Name string `json:"name,omitempty"`

	// Arguments is a JSON document encoded as a string, and it may arrive in
	// fragments across several chunks. Only once the accumulated text parses
	// is there a tool call to report.
	Arguments string `json:"arguments,omitempty"`
}

type wireTool struct {
	Type     string       `json:"type"`
	Function wireToolFunc `json:"function"`
}

type wireToolFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      bool            `json:"strict,omitempty"`
}

// contentPart carries multimodal input. A plain string is used when the
// message is only text, because some compatible servers reject the array form.
type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type chatChunk struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role    string `json:"role"`
			Content string `json:"content"`

			// Servers disagree on the name. Ollama sends "reasoning"; several
			// others send "reasoning_content". Both are read so a profile does
			// not have to guess which one it will see.
			Reasoning        string `json:"reasoning"`
			ReasoningContent string `json:"reasoning_content"`

			ToolCalls []wireToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`

	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`

		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`

	Error *wireError `json:"error"`
}

// wireError is nested under an "error" key, unlike the flat string some native
// APIs return.
type wireError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

type modelList struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}
