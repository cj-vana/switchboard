package agent

import (
	"context"
	"time"

	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
	"github.com/switchboard-code/switchboard/internal/tools"
)

// Observer receives what a turn produces as it happens.
//
// It exists so the loop can stream without knowing where the output goes. The
// core is not permitted to write to a terminal, and the REPL is only the first
// of several consumers (design principle 1).
//
// Text, notice, usage, and batch callbacks are called from the loop's
// goroutine. Tool callbacks may be concurrent when every call in a batch is
// parallel-safe, so implementations must synchronize shared state.
type Observer interface {
	ThinkingDelta(text string)
	TextDelta(text string)

	// ToolStart and ToolEnd carry the complete provider call as well as the
	// resolved permission request. The call ID is the only sound correlation
	// key for concurrent calls with the same tool name, while Input preserves
	// arguments that do not happen to be represented by Path or Argv.
	ToolStart(call provider.ToolUse, req permission.Request)
	ToolEnd(call provider.ToolUse, req permission.Request, res tools.Result, took time.Duration)

	// ToolBatchEnd is emitted once after all calls requested by one model call
	// completed successfully and their results were committed to the session.
	// Cancelled or aborted batches do not emit it. It is the safe boundary for
	// routing, advice, and policy changes that must never split a parallel batch.
	ToolBatchEnd(ctx context.Context)

	// Notice reports something the user needs to know that is not model output:
	// a retry, a truncated log, a denied call, an exhausted round budget.
	Notice(level, text string)

	TurnUsage(u session.Usage)
}

type NopObserver struct{}

func (NopObserver) ThinkingDelta(string)                                                      {}
func (NopObserver) TextDelta(string)                                                          {}
func (NopObserver) ToolStart(provider.ToolUse, permission.Request)                            {}
func (NopObserver) ToolEnd(provider.ToolUse, permission.Request, tools.Result, time.Duration) {}
func (NopObserver) ToolBatchEnd(context.Context)                                              {}
func (NopObserver) Notice(string, string)                                                     {}
func (NopObserver) TurnUsage(session.Usage)                                                   {}
