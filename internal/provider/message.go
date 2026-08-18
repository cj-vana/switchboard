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

	// Injected marks a user-role message the harness placed at a round
	// boundary — advice, a watch report — rather than one that opened a
	// turn. The wire does not carry it (adapters render role and content);
	// it exists so a reader of the log can tell a turn's opening from what
	// rode in mid-turn, which /retry has to get right.
	Injected bool `json:"injected,omitempty"`

	// ContinuityRef names the durable continuity capsule folded into this
	// message by the harness. Like Injected, it is session metadata rather than
	// provider wire data: adapters render only role and content. Recording the
	// reference lets replay prove a capsule was already delivered without
	// parsing user-visible text or injecting it twice after another resume.
	ContinuityRef string `json:"continuity_ref,omitempty"`
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

// AuthoredText returns the text the user supplied, excluding the dedicated
// first block carrying a validated continuity capsule. Text deliberately
// remains the exact provider-visible wire text for request assembly, routing,
// and token estimation. Session append/replay is what validates the metadata
// contract before durable consumers rely on this projection.
func (m Message) AuthoredText() string {
	start := 0
	if m.ContinuityRef != "" && len(m.Content) > 0 {
		switch m.Content[0].(type) {
		case Text, *Text:
			start = 1
		}
	}
	var out string
	for _, block := range m.Content[start:] {
		switch text := block.(type) {
		case Text:
			out += text.Text
		case *Text:
			if text != nil {
				out += text.Text
			}
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

// CloneMessage returns an ownership-isolated canonical message. Interfaces
// hide the mutable byte slices carried by tool arguments and attachments, so
// copying only Message.Content would still let a caller change durable state
// after append or mutate a State snapshot behind the session lock.
func CloneMessage(in Message) Message {
	out := in
	if in.Content == nil {
		return out
	}
	out.Content = make([]Block, len(in.Content))
	for i, block := range in.Content {
		switch value := block.(type) {
		case Text, Thinking, ToolResult:
			out.Content[i] = value
		case ToolUse:
			value.Input = append(json.RawMessage(nil), value.Input...)
			out.Content[i] = value
		case Image:
			value.Data = append([]byte(nil), value.Data...)
			out.Content[i] = value
		case Document:
			value.Data = append([]byte(nil), value.Data...)
			out.Content[i] = value
		case *Text:
			if value != nil {
				out.Content[i] = *value
			}
		case *Thinking:
			if value != nil {
				out.Content[i] = *value
			}
		case *ToolResult:
			if value != nil {
				out.Content[i] = *value
			}
		case *ToolUse:
			if value != nil {
				copy := *value
				copy.Input = append(json.RawMessage(nil), value.Input...)
				out.Content[i] = copy
			}
		case *Image:
			if value != nil {
				copy := *value
				copy.Data = append([]byte(nil), value.Data...)
				out.Content[i] = copy
			}
		case *Document:
			if value != nil {
				copy := *value
				copy.Data = append([]byte(nil), value.Data...)
				out.Content[i] = copy
			}
		default:
			// MarshalJSON/replay remains the authority on unsupported block
			// kinds. Preserve an extension block rather than silently dropping
			// it; canonical built-ins above receive full ownership isolation.
			out.Content[i] = block
		}
	}
	return out
}

func CloneMessages(in []Message) []Message {
	if in == nil {
		return nil
	}
	out := make([]Message, len(in))
	for i := range in {
		out[i] = CloneMessage(in[i])
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
	Role          Role            `json:"role"`
	Content       []blockEnvelope `json:"content"`
	Incomplete    bool            `json:"incomplete,omitempty"`
	Injected      bool            `json:"injected,omitempty"`
	ContinuityRef string          `json:"continuity_ref,omitempty"`
}

func (m Message) MarshalJSON() ([]byte, error) {
	out := messageJSON{
		Role: m.Role, Incomplete: m.Incomplete, Injected: m.Injected,
		ContinuityRef: m.ContinuityRef,
	}
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
	m.Injected = in.Injected
	m.ContinuityRef = in.ContinuityRef
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
