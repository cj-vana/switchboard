package openaicompat

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/cjvana/switchboard/internal/provider"
)

// maxLineBytes bounds one SSE line. A single chunk carrying large tool
// arguments is legitimate, so the ceiling is generous; it exists to stop a
// server that never sends a newline from consuming memory without limit.
const maxLineBytes = 8 << 20

// stream decodes server-sent events into canonical events.
//
// Tool calls are the hard part. The format streams them as fragments tagged
// with an index: the name may arrive in one chunk and the arguments across
// several more, so nothing can be emitted until the choice finishes and the
// accumulated argument text parses as JSON.
type stream struct {
	ctx     context.Context
	body    io.ReadCloser
	scanner *bufio.Scanner
	profile Profile

	pending []provider.Event

	blockIndex int
	blockKind  provider.EventType
	blockOpen  bool

	tools        map[int]*toolAccum
	sawToolCalls bool
	finishReason string
	usage        provider.Usage

	toolsEmitted bool
	sawFinish    bool
	finished     bool

	// err is raised only after every event already decoded has been handed to
	// the caller, so a malformed tool call does not discard the text that
	// arrived before it.
	err error
}

type toolAccum struct {
	id   string
	name string
	args strings.Builder
}

func newStream(ctx context.Context, body io.ReadCloser, profile Profile) *stream {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), maxLineBytes)
	return &stream{
		ctx:     ctx,
		body:    body,
		scanner: sc,
		profile: profile,
		tools:   map[int]*toolAccum{},
	}
}

func (s *stream) Next() (provider.Event, error) {
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

func (s *stream) Close() error { return s.body.Close() }

func (s *stream) readLine() error {
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
		if s.sawFinish {
			// The turn completed; the server simply did not send [DONE], which
			// several compatible servers omit.
			s.finish()
			return nil
		}
		// The connection ended mid-turn. Whatever the caller already consumed
		// is real output and must not be discarded.
		return provider.ErrStreamIncomplete
	}

	line := strings.TrimSpace(s.scanner.Text())
	if line == "" || strings.HasPrefix(line, ":") {
		return nil // keep-alive or blank separator
	}
	payload, ok := strings.CutPrefix(line, "data:")
	if !ok {
		// Other SSE fields (event:, id:, retry:) carry nothing this format
		// uses, so they are skipped rather than treated as damage.
		return nil
	}
	payload = strings.TrimSpace(payload)

	if payload == "[DONE]" {
		s.finish()
		return nil
	}

	var chunk chatChunk
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return &provider.ProtocolError{Provider: Name, Detail: "decoding a stream chunk", Err: err}
	}
	if chunk.Error != nil {
		return &provider.APIError{Provider: Name, StatusCode: 0, Body: chunk.Error.Message}
	}

	if chunk.Usage != nil {
		s.usage.InputTokens = chunk.Usage.PromptTokens
		s.usage.OutputTokens = chunk.Usage.CompletionTokens
		if d := chunk.Usage.PromptTokensDetails; d != nil {
			// The format reports cached prompt tokens as a subset of the
			// prompt count, so the uncached remainder is what is left.
			s.usage.CacheReadTokens = d.CachedTokens
			s.usage.InputTokens -= d.CachedTokens
		}
	}

	for _, choice := range chunk.Choices {
		if reasoning := firstNonEmpty(choice.Delta.Reasoning, choice.Delta.ReasoningContent); reasoning != "" {
			s.pending = append(s.pending, provider.Event{
				Type:  provider.EventThinkingDelta,
				Index: s.indexFor(provider.EventThinkingDelta),
				Text:  reasoning,
			})
		}
		if choice.Delta.Content != "" {
			s.pending = append(s.pending, provider.Event{
				Type:  provider.EventTextDelta,
				Index: s.indexFor(provider.EventTextDelta),
				Text:  choice.Delta.Content,
			})
		}
		for _, call := range choice.Delta.ToolCalls {
			s.accumulate(call)
		}
		if choice.FinishReason != "" {
			s.finishReason = choice.FinishReason
			s.sawFinish = true
			// Tool calls are complete here, but the terminal event is not
			// emitted yet: the usage chunk arrives after finish_reason on a
			// real server, and reporting a turn's token counts as zero
			// because they had not landed yet is worse than waiting.
			s.emitToolCalls()
		}
	}
	return nil
}

// accumulate folds a tool-call fragment into the call at its index. Ollama
// sends a complete call in one chunk; OpenAI and others split the arguments,
// so both shapes fold the same way.
func (s *stream) accumulate(call wireToolCall) {
	acc, ok := s.tools[call.Index]
	if !ok {
		acc = &toolAccum{}
		s.tools[call.Index] = acc
	}
	if call.ID != "" {
		acc.id = call.ID
	}
	if call.Function.Name != "" {
		acc.name = call.Function.Name
	}
	acc.args.WriteString(call.Function.Arguments)
	s.sawToolCalls = true
}

// emitToolCalls turns the accumulated fragments into canonical calls. It runs
// when the choice finishes, which is the first point at which the argument
// text is known to be complete.
func (s *stream) emitToolCalls() {
	if s.toolsEmitted {
		return
	}
	s.toolsEmitted = true

	indexes := make([]int, 0, len(s.tools))
	for i := range s.tools {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)

	for _, i := range indexes {
		use, err := s.tools[i].toolUse()
		if err != nil {
			// A malformed call cannot be executed. Reporting it rather than
			// dropping it keeps the turn from continuing as though the model
			// had asked for nothing.
			s.err = err
			return
		}
		s.pending = append(s.pending, provider.Event{
			Type:    provider.EventToolUse,
			Index:   s.newBlock(),
			ToolUse: use,
		})
	}
}

// finish emits the terminal event. It runs on [DONE], or at end of stream when
// a finish_reason was already seen, so any usage chunk sent after the choice
// finished has been folded in first.
func (s *stream) finish() {
	if s.finished {
		return
	}
	s.emitToolCalls()
	s.finished = true

	if s.err != nil {
		return
	}
	s.pending = append(s.pending, provider.Event{
		Type:       provider.EventDone,
		StopReason: s.stopReason(),
		Usage:      s.usage,
	})
}

func (acc *toolAccum) toolUse() (*provider.ToolUse, error) {
	if acc.name == "" {
		return nil, &provider.ProtocolError{Provider: Name, Detail: "tool call with no function name"}
	}

	args := strings.TrimSpace(acc.args.String())
	if args == "" {
		args = "{}"
	}
	if !json.Valid([]byte(args)) {
		// Arguments arrive as a string built from fragments, so an incomplete
		// or malformed accumulation is the failure mode this check exists for.
		return nil, &provider.ProtocolError{
			Provider: Name,
			Detail:   fmt.Sprintf("tool call %q carried arguments that are not valid JSON", acc.name),
		}
	}

	id := acc.id
	if id == "" {
		// Some servers omit the ID. The loop correlates results to calls by
		// ID, so one is synthesized from the call's name and position.
		id = "call_" + acc.name
	}
	return &provider.ToolUse{ID: id, Name: acc.name, Input: json.RawMessage(args)}, nil
}

// stopReason maps the format's finish_reason, and corrects for a server that
// reports "stop" on a turn that ended in a tool call. Reporting end_turn there
// would leave the call unexecuted.
func (s *stream) stopReason() provider.StopReason {
	if s.sawToolCalls {
		return provider.StopToolUse
	}
	switch s.finishReason {
	case "tool_calls":
		return provider.StopToolUse
	case "length":
		return provider.StopMaxTokens
	default:
		return provider.StopEndTurn
	}
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

// newBlock allocates an index for a block that arrives complete, leaving no
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

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
