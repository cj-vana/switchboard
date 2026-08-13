// Package provider defines the canonical conversation types the harness
// operates on, and the interface adapters implement to translate them to and
// from a specific provider's wire format.
//
// The canonical types are deliberately not an OpenAI-compatible schema. That
// format is a lowest common denominator which discards cache breakpoints,
// reasoning-block replay rules, parallel tool-call fidelity, and per-model
// capability signals — precisely what the routing design depends on (§5.1).
package provider

import (
	"encoding/json"
	"fmt"
)

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type BlockKind string

const (
	KindText       BlockKind = "text"
	KindThinking   BlockKind = "thinking"
	KindToolUse    BlockKind = "tool_use"
	KindToolResult BlockKind = "tool_result"
	KindImage      BlockKind = "image"
	KindDocument   BlockKind = "document"
)

type Block interface {
	Kind() BlockKind
}

type Text struct {
	Text string `json:"text"`
}

func (Text) Kind() BlockKind { return KindText }

// Thinking carries a model's reasoning output. Signature holds any opaque
// verification token the provider requires to be echoed back on replay; an
// adapter that cannot faithfully replay a Thinking block returns a
// CapabilityError rather than dropping it.
type Thinking struct {
	Text      string `json:"text"`
	Signature string `json:"signature,omitempty"`
}

func (Thinking) Kind() BlockKind { return KindThinking }

// ToolUse is always a complete call. Providers that stream tool arguments as
// partial JSON accumulate them inside the adapter, because only the adapter
// knows that provider's partial-JSON semantics.
type ToolUse struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

func (ToolUse) Kind() BlockKind { return KindToolUse }

// ToolResult carries Name alongside ToolUseID because some providers correlate
// results by call ID and others only by tool name.
type ToolResult struct {
	ToolUseID string `json:"tool_use_id"`
	Name      string `json:"name"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error,omitempty"`
}

func (ToolResult) Kind() BlockKind { return KindToolResult }

type Image struct {
	MediaType string `json:"media_type"`
	Data      []byte `json:"data"`
}

func (Image) Kind() BlockKind { return KindImage }

type Document struct {
	MediaType string `json:"media_type"`
	Name      string `json:"name,omitempty"`
	Data      []byte `json:"data"`
}

func (Document) Kind() BlockKind { return KindDocument }

type Message struct {
	Role    Role    `json:"role"`
	Content []Block `json:"content"`

	// Incomplete marks an assistant message whose stream was interrupted. It is
	// retained in the session log for diagnosis but is never replayed to a
	// provider as a finished turn (§10.3).
	Incomplete bool `json:"incomplete,omitempty"`
}

// UserText is a shorthand for the overwhelmingly common single-text-block case.
func UserText(s string) Message {
	return Message{Role: RoleUser, Content: []Block{Text{Text: s}}}
}

func (m Message) Text() string {
	var out string
	for _, b := range m.Content {
		if t, ok := b.(Text); ok {
			out += t.Text
		}
	}
	return out
}

func (m Message) ToolUses() []ToolUse {
	var out []ToolUse
	for _, b := range m.Content {
		if t, ok := b.(ToolUse); ok {
			out = append(out, t)
		}
	}
	return out
}

// blockEnvelope tags each block with its kind so the session log round-trips
// through the Block interface. The wrapper is explicit rather than a flattened
// discriminator field so that no concrete block type carries a mutable copy of
// its own kind.
type blockEnvelope struct {
	Kind BlockKind       `json:"kind"`
	Data json.RawMessage `json:"data"`
}

type messageJSON struct {
	Role       Role            `json:"role"`
	Content    []blockEnvelope `json:"content"`
	Incomplete bool            `json:"incomplete,omitempty"`
}

func (m Message) MarshalJSON() ([]byte, error) {
	out := messageJSON{Role: m.Role, Incomplete: m.Incomplete}
	for _, b := range m.Content {
		data, err := json.Marshal(b)
		if err != nil {
			return nil, fmt.Errorf("marshal %s block: %w", b.Kind(), err)
		}
		out.Content = append(out.Content, blockEnvelope{Kind: b.Kind(), Data: data})
	}
	return json.Marshal(out)
}

func (m *Message) UnmarshalJSON(data []byte) error {
	var in messageJSON
	if err := json.Unmarshal(data, &in); err != nil {
		return err
	}
	m.Role = in.Role
	m.Incomplete = in.Incomplete
	m.Content = nil
	for _, env := range in.Content {
		b, err := decodeBlock(env)
		if err != nil {
			return err
		}
		m.Content = append(m.Content, b)
	}
	return nil
}

func decodeBlock(env blockEnvelope) (Block, error) {
	switch env.Kind {
	case KindText:
		return unmarshalBlock[Text](env.Data)
	case KindThinking:
		return unmarshalBlock[Thinking](env.Data)
	case KindToolUse:
		return unmarshalBlock[ToolUse](env.Data)
	case KindToolResult:
		return unmarshalBlock[ToolResult](env.Data)
	case KindImage:
		return unmarshalBlock[Image](env.Data)
	case KindDocument:
		return unmarshalBlock[Document](env.Data)
	default:
		// A block kind written by a newer binary must not be silently discarded:
		// dropping it would corrupt the conversation on the next request.
		return nil, fmt.Errorf("unknown block kind %q", env.Kind)
	}
}

func unmarshalBlock[T Block](data []byte) (Block, error) {
	var b T
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("unmarshal %s block: %w", b.Kind(), err)
	}
	return b, nil
}
