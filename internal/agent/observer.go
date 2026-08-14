package agent

import (
	"time"

	"github.com/cj-vana/switchboard/internal/permission"
	"github.com/cj-vana/switchboard/internal/session"
	"github.com/cj-vana/switchboard/internal/tools"
)

// Observer receives what a turn produces as it happens.
//
// It exists so the loop can stream without knowing where the output goes. The
// core is not permitted to write to a terminal, and the REPL is only the first
// of several consumers (design principle 1).
//
// Implementations are called from the loop's goroutine and should not block.
type Observer interface {
	ThinkingDelta(text string)
	TextDelta(text string)

	ToolStart(name string, req permission.Request)
	ToolEnd(name string, res tools.Result, took time.Duration)

	// Notice reports something the user needs to know that is not model output:
	// a retry, a truncated log, a denied call, an exhausted round budget.
	Notice(level, text string)

	TurnUsage(u session.Usage)
}

type NopObserver struct{}

func (NopObserver) ThinkingDelta(string)                        {}
func (NopObserver) TextDelta(string)                            {}
func (NopObserver) ToolStart(string, permission.Request)        {}
func (NopObserver) ToolEnd(string, tools.Result, time.Duration) {}
func (NopObserver) Notice(string, string)                       {}
func (NopObserver) TurnUsage(session.Usage)                     {}
