package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/cjvana/switchboard/internal/provider"
)

// stream decodes Ollama's newline-delimited chat response. json.Decoder reads
// concatenated JSON values directly, which avoids imposing any line-length
// ceiling on a chunk carrying large tool arguments.
type stream struct {
	ctx  context.Context
	body io.ReadCloser
	dec  *json.Decoder

	pending []provider.Event

	// Ollama sends no block delimiters, so block boundaries are inferred: a
	// change of output kind starts a new block.
	blockIndex   int
	blockKind    provider.EventType
	blockOpen    bool
	toolCallSeq  int
	sawToolCalls bool

	finished bool
}

func newStream(ctx context.Context, body io.ReadCloser) *stream {
	return &stream{ctx: ctx, body: body, dec: json.NewDecoder(body)}
}

func (s *stream) Next() (provider.Event, error) {
	for {
		if len(s.pending) > 0 {
			ev := s.pending[0]
			s.pending = s.pending[1:]
			return ev, nil
		}
		if s.finished {
			return provider.Event{}, io.EOF
		}
		if err := s.readChunk(); err != nil {
			return provider.Event{}, err
		}
	}
}

func (s *stream) Close() error { return s.body.Close() }

func (s *stream) readChunk() error {
	var chunk chatChunk
	if err := s.dec.Decode(&chunk); err != nil {
		if ctxErr := s.ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			// The connection dropped before the terminal chunk. Whatever the
			// caller already consumed is real output and must not be discarded.
			return provider.ErrStreamIncomplete
		}
		return &provider.ProtocolError{Provider: Name, Detail: "decoding chat chunk", Err: err}
	}

	if chunk.Error != "" {
		return &provider.APIError{Provider: Name, StatusCode: 0, Body: chunk.Error}
	}

	if t := chunk.Message.Thinking; t != "" {
		s.pending = append(s.pending, provider.Event{
			Type:  provider.EventThinkingDelta,
			Index: s.indexFor(provider.EventThinkingDelta),
			Text:  t,
		})
	}
	if c := chunk.Message.Content; c != "" {
		s.pending = append(s.pending, provider.Event{
			Type:  provider.EventTextDelta,
			Index: s.indexFor(provider.EventTextDelta),
			Text:  c,
		})
	}
	for _, call := range chunk.Message.ToolCalls {
		use, err := s.toolUse(call)
		if err != nil {
			return err
		}
		s.sawToolCalls = true
		s.pending = append(s.pending, provider.Event{
			Type:    provider.EventToolUse,
			Index:   s.newBlock(),
			ToolUse: use,
		})
	}

	if chunk.Done {
		s.finished = true
		s.pending = append(s.pending, provider.Event{
			Type:       provider.EventDone,
			StopReason: s.stopReason(chunk.DoneReason),
			Usage: provider.Usage{
				InputTokens:  chunk.PromptEvalCount,
				OutputTokens: chunk.EvalCount,
			},
		})
	}
	return nil
}

func (s *stream) toolUse(call wireToolCall) (*provider.ToolUse, error) {
	if call.Function.Name == "" {
		return nil, &provider.ProtocolError{Provider: Name, Detail: "tool call with no function name"}
	}
	args := call.Function.Arguments
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	if !json.Valid(args) {
		return nil, &provider.ProtocolError{
			Provider: Name,
			Detail:   fmt.Sprintf("tool call %q carried malformed arguments", call.Function.Name),
		}
	}

	id := call.ID
	if id == "" {
		// Some models and older servers omit the ID. The loop correlates results
		// to calls by ID, so one is synthesized from the call's position, which
		// is stable for the life of the stream and of the session log.
		id = fmt.Sprintf("call_%s_%d", call.Function.Name, s.toolCallSeq)
	}
	s.toolCallSeq++

	return &provider.ToolUse{ID: id, Name: call.Function.Name, Input: args}, nil
}

// stopReason corrects for Ollama reporting done_reason "stop" on a turn that
// ended in a tool call. Treating that as end_turn would end the agent loop with
// the call unexecuted.
func (s *stream) stopReason(doneReason string) provider.StopReason {
	if s.sawToolCalls {
		return provider.StopToolUse
	}
	if doneReason == "length" {
		return provider.StopMaxTokens
	}
	return provider.StopEndTurn
}

func (s *stream) indexFor(kind provider.EventType) int {
	if s.blockOpen && s.blockKind == kind {
		return s.blockIndex
	}
	if s.blockOpen {
		s.blockIndex++
	}
	s.blockOpen = true
	s.blockKind = kind
	return s.blockIndex
}

// newBlock allocates an index for a block that arrives complete, and leaves no
// block open so the next delta of any kind starts one of its own.
func (s *stream) newBlock() int {
	if s.blockOpen {
		s.blockIndex++
	}
	idx := s.blockIndex
	s.blockIndex++
	s.blockOpen = false
	return idx
}
