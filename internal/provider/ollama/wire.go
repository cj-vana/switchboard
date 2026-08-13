package ollama

import "encoding/json"

// The wire types mirror Ollama's native /api/chat schema rather than its
// OpenAI-compatible shim. The native endpoint reports reasoning in a dedicated
// `thinking` field and sends tool arguments as a JSON object, both of which the
// shim flattens.

type chatRequest struct {
	Model    string         `json:"model"`
	Messages []wireMessage  `json:"messages"`
	Tools    []wireTool     `json:"tools,omitempty"`
	Stream   bool           `json:"stream"`
	Think    any            `json:"think,omitempty"`
	Options  map[string]any `json:"options,omitempty"`
}

type wireMessage struct {
	Role      string         `json:"role"`
	Content   string         `json:"content"`
	Thinking  string         `json:"thinking,omitempty"`
	Images    []string       `json:"images,omitempty"`
	ToolCalls []wireToolCall `json:"tool_calls,omitempty"`
	ToolName  string         `json:"tool_name,omitempty"`
}

type wireToolCall struct {
	ID       string           `json:"id,omitempty"`
	Function wireToolCallFunc `json:"function"`
}

type wireToolCallFunc struct {
	Index     int             `json:"index,omitempty"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type wireTool struct {
	Type     string       `json:"type"`
	Function wireToolFunc `json:"function"`
}

type wireToolFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// chatChunk is one NDJSON line. A mid-stream failure arrives as a chunk whose
// only populated field is Error, after the server has already sent 200.
type chatChunk struct {
	Model      string      `json:"model"`
	Message    wireMessage `json:"message"`
	Done       bool        `json:"done"`
	DoneReason string      `json:"done_reason"`
	Error      string      `json:"error"`

	PromptEvalCount int `json:"prompt_eval_count"`
	EvalCount       int `json:"eval_count"`
}

type errorResponse struct {
	Error string `json:"error"`
}

type showResponse struct {
	Capabilities []string `json:"capabilities"`
}

type tagsResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}
