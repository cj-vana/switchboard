package provider

type EventType string

const (
	// EventTextDelta and EventThinkingDelta carry incremental output for the
	// block at Index.
	EventTextDelta     EventType = "text_delta"
	EventThinkingDelta EventType = "thinking_delta"

	// EventToolUse carries one complete tool call. Adapters accumulate partial
	// argument JSON internally and emit this only once the call parses.
	EventToolUse EventType = "tool_use"

	// EventDone is the final event of a successful stream and carries StopReason
	// and Usage.
	EventDone EventType = "done"
)

type StopReason string

const (
	StopEndTurn   StopReason = "end_turn"
	StopToolUse   StopReason = "tool_use"
	StopMaxTokens StopReason = "max_tokens"
	StopCanceled  StopReason = "canceled"
)

type Event struct {
	Type       EventType
	Index      int
	Text       string
	ToolUse    *ToolUse
	StopReason StopReason
	Usage      Usage

	// Signature is an opaque, provider-issued attestation over a thinking
	// block. It is carried on the thinking deltas of the block at Index and has
	// to survive into the assembled message, because a target that issues one
	// verifies it on replay: Anthropic rejects a thinking block returned with a
	// missing or altered signature outright, which would break the second
	// request of every tool-use turn that had reasoning enabled.
	//
	// Dropping the whole thinking block is allowed and replaying it unsigned is
	// not, so this is the difference between preserving reasoning across a tool
	// call and discarding it.
	Signature string
}

// Usage is reported by the provider after the fact. Cache read and write counts
// are distinct because a write observation and a read observation mean different
// things to the cost model, and assuming one from the other is how an estimator
// starts believing in a cache it has never seen (§6.3).
type Usage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

// Sub reports what one turn added, given a running total before and after. It
// clamps at zero rather than reporting a negative count, because a resumed
// session replays a total the current turn did not produce.
func (u Usage) Sub(o Usage) Usage {
	nonNegative := func(a, b int) int {
		if a-b < 0 {
			return 0
		}
		return a - b
	}
	return Usage{
		InputTokens:      nonNegative(u.InputTokens, o.InputTokens),
		OutputTokens:     nonNegative(u.OutputTokens, o.OutputTokens),
		CacheReadTokens:  nonNegative(u.CacheReadTokens, o.CacheReadTokens),
		CacheWriteTokens: nonNegative(u.CacheWriteTokens, o.CacheWriteTokens),
	}
}

func (u Usage) Add(o Usage) Usage {
	return Usage{
		InputTokens:      u.InputTokens + o.InputTokens,
		OutputTokens:     u.OutputTokens + o.OutputTokens,
		CacheReadTokens:  u.CacheReadTokens + o.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens + o.CacheWriteTokens,
	}
}

// EventStream yields normalized events. Next returns io.EOF exactly once, after
// the EventDone that ends a clean stream; any other error ends the stream for
// good. Cancellation is driven by the context passed to Stream, so Next takes
// none of its own.
//
// Callers must Close, including after an error, so the adapter can release the
// underlying connection.
type EventStream interface {
	Next() (Event, error)
	Close() error
}
