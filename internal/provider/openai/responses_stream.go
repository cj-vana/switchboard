package openai

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/switchboard-code/switchboard/internal/provider"
)

// maxResponsesLine bounds one event. A single event carrying large tool
// arguments is legitimate, so the ceiling is generous; it exists to stop a
// server that never sends a newline from consuming memory without limit.
const maxResponsesLine = 8 << 20

// responsesStream decodes Responses API events into canonical events.
//
// The shape differs from both other formats in the same way: output is a list
// of items, and a tool call is an item rather than a field on a message. Items
// are tracked by their own id, because the argument deltas reference that and
// not the output index.
type responsesStream struct {
	ctx     context.Context
	body    io.ReadCloser
	scanner *bufio.Scanner

	pending []provider.Event

	// items maps an item id to what is being accumulated for it.
	items map[string]*responsesItemAccum

	// index assigns canonical block indexes in the order items appear, since
	// the wire's output_index counts items and the canonical index counts
	// blocks.
	index     map[string]int
	nextIndex int

	usage      provider.Usage
	sawToolUse bool
	status     string
	incomplete string

	finished bool
	err      error
}

type responsesItemAccum struct {
	kind      string
	callID    string
	name      string
	arguments strings.Builder
}

func newResponsesStream(ctx context.Context, body io.ReadCloser) *responsesStream {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), maxResponsesLine)
	return &responsesStream{
		ctx:     ctx,
		body:    body,
		scanner: sc,
		items:   map[string]*responsesItemAccum{},
		index:   map[string]int{},
	}
}

func (s *responsesStream) Next() (provider.Event, error) {
	for {
		if len(s.pending) > 0 {
			ev := s.pending[0]
			s.pending = s.pending[1:]
			return ev, nil
		}
		if s.err != nil {
			return provider.Event{}, s.err
		}
		if s.finished {
			return provider.Event{}, io.EOF
		}
		if err := s.readLine(); err != nil {
			return provider.Event{}, err
		}
	}
}

func (s *responsesStream) Close() error { return s.body.Close() }

func (s *responsesStream) readLine() error {
	if !s.scanner.Scan() {
		if err := s.scanner.Err(); err != nil {
			if ctxErr := s.ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return &provider.ProtocolError{Provider: Name, Detail: "reading the event stream", Err: err}
		}
		if ctxErr := s.ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if s.status != "" {
			// The response completed; the connection simply closed after it.
			s.finish()
			return nil
		}
		// The connection ended mid-turn. Whatever the caller already consumed
		// is real output and must not be discarded.
		return provider.ErrStreamIncomplete
	}

	line := strings.TrimSpace(s.scanner.Text())
	if line == "" || strings.HasPrefix(line, ":") {
		return nil
	}
	payload, ok := strings.CutPrefix(line, "data:")
	if !ok {
		// The `event:` line names a type the payload also carries, so the body
		// is what gets trusted.
		return nil
	}

	var ev responsesEvent
	if err := json.Unmarshal([]byte(strings.TrimSpace(payload)), &ev); err != nil {
		return &provider.ProtocolError{Provider: Name, Detail: "decoding a stream event", Err: err}
	}
	return s.handle(ev)
}

func (s *responsesStream) handle(ev responsesEvent) error {
	switch ev.Type {
	case "error", "response.failed":
		message := ev.Detail
		if ev.Response != nil && ev.Response.Error != nil {
			message = ev.Response.Error.Message
		}
		if message == "" {
			message = "the endpoint reported a failure with no message"
		}
		return &provider.APIError{Provider: Name, StatusCode: 0, Body: message}

	case "response.output_item.added":
		if ev.Item == nil {
			return nil
		}
		acc := &responsesItemAccum{kind: ev.Item.Type, callID: ev.Item.CallID, name: ev.Item.Name}
		acc.arguments.WriteString(ev.Item.Arguments)
		s.items[ev.Item.ID] = acc
		s.indexFor(ev.Item.ID)

	case "response.output_text.delta":
		s.pending = append(s.pending, provider.Event{
			Type: provider.EventTextDelta, Index: s.indexFor(ev.ItemID), Text: ev.Delta,
		})

	case "response.reasoning_summary_text.delta":
		s.pending = append(s.pending, provider.Event{
			Type: provider.EventThinkingDelta, Index: s.indexFor(ev.ItemID), Text: ev.Delta,
		})

	case "response.function_call_arguments.delta":
		if acc, ok := s.items[ev.ItemID]; ok {
			acc.arguments.WriteString(ev.Delta)
		}

	case "response.output_item.done":
		if ev.Item == nil || ev.Item.Type != "function_call" {
			return nil
		}
		// The done event carries the complete arguments, so it is preferred
		// over the accumulated deltas rather than appended to them.
		acc := s.items[ev.Item.ID]
		if acc == nil {
			acc = &responsesItemAccum{kind: ev.Item.Type, callID: ev.Item.CallID, name: ev.Item.Name}
		}
		if ev.Item.Arguments != "" {
			acc.arguments.Reset()
			acc.arguments.WriteString(ev.Item.Arguments)
		}
		if ev.Item.CallID != "" {
			acc.callID = ev.Item.CallID
		}
		if ev.Item.Name != "" {
			acc.name = ev.Item.Name
		}

		use, err := acc.toolUse()
		if err != nil {
			s.err = err
			return nil
		}
		s.sawToolUse = true
		s.pending = append(s.pending, provider.Event{
			Type: provider.EventToolUse, Index: s.indexFor(ev.Item.ID), ToolUse: use,
		})

	case "response.completed", "response.incomplete":
		if ev.Response != nil {
			s.status = ev.Response.Status
			s.applyUsage(ev.Response.Usage)
			if ev.Response.IncompleteDetails != nil {
				s.incomplete = ev.Response.IncompleteDetails.Reason
			}
		}
		s.finish()
	}
	return nil
}

// indexFor assigns a canonical block index per item, in the order items are
// first seen. The wire's output_index counts items and cannot be used directly.
func (s *responsesStream) indexFor(itemID string) int {
	if idx, ok := s.index[itemID]; ok {
		return idx
	}
	idx := s.nextIndex
	s.index[itemID] = idx
	s.nextIndex++
	return idx
}

func (s *responsesStream) applyUsage(u *responsesUsage) {
	if u == nil {
		return
	}
	s.usage.InputTokens = u.InputTokens
	s.usage.OutputTokens = u.OutputTokens
	if d := u.InputTokensDetails; d != nil {
		// Cached tokens are a subset of input_tokens in this shape, the way
		// they are in chat-completions and unlike Anthropic where the counts
		// are disjoint. The remainder is what was actually processed fresh.
		s.usage.CacheReadTokens = d.CachedTokens
		s.usage.CacheWriteTokens = d.CacheWriteTokens
		s.usage.InputTokens -= d.CachedTokens
		if s.usage.InputTokens < 0 {
			s.usage.InputTokens = 0
		}
	}
}

func (s *responsesStream) finish() {
	if s.finished {
		return
	}
	s.finished = true
	if s.err != nil {
		return
	}
	s.pending = append(s.pending, provider.Event{
		Type:       provider.EventDone,
		StopReason: s.stop(),
		Usage:      s.usage,
	})
}

// stop maps the response's own status. A turn that emitted a call is reported
// as tool_use whatever else it says, because the loop executes calls only on
// that stop reason and a call left unexecuted breaks every later request.
func (s *responsesStream) stop() provider.StopReason {
	if s.sawToolUse {
		return provider.StopToolUse
	}
	if s.incomplete == "max_output_tokens" {
		return provider.StopMaxTokens
	}
	return provider.StopEndTurn
}

func (acc *responsesItemAccum) toolUse() (*provider.ToolUse, error) {
	if acc.name == "" {
		return nil, &provider.ProtocolError{Provider: Name, Detail: "tool call with no name"}
	}
	if acc.callID == "" {
		// The result refers to call_id, not to the item id. Without one there
		// is nothing to correlate a result to, and the turn cannot continue.
		return nil, &provider.ProtocolError{
			Provider: Name,
			Detail:   fmt.Sprintf("tool call %q arrived with no call_id to return a result against", acc.name),
		}
	}

	arguments := strings.TrimSpace(acc.arguments.String())
	if arguments == "" {
		arguments = "{}"
	}
	if !json.Valid([]byte(arguments)) {
		return nil, &provider.ProtocolError{
			Provider: Name,
			Detail:   fmt.Sprintf("tool call %q carried arguments that are not valid JSON", acc.name),
		}
	}
	return &provider.ToolUse{ID: acc.callID, Name: acc.name, Input: json.RawMessage(arguments)}, nil
}
