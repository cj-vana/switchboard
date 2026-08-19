package main

// questionRelay is the user's question channel, named before it exists.
//
// Two consumers need it at two different moments. The ask tool needs it when
// the registry is assembled, and an MCP client needs it earlier still: the
// elicitation capability is declared during initialize, which happens while
// the servers connect, and a capability is a promise that cannot be made
// retroactively. The surfaces that resolve a question are built later than
// both — the TUI's questioner needs a running Bubble Tea program, which does
// not exist until after assembly.
//
// So the surface promises a user at assembly and delivers the channel when it
// has one. What must not happen is the promise standing in for the channel: a
// relay nobody filled refuses, and the refusal is the one the ask tool already
// makes for an unattended surface, because the same thing is true — there is
// no one listening.
//
// A surface with no user never builds one of these at all, which is why
// headless runs still decline elicitation before the schema is read.

import (
	"context"
	"errors"
	"sync"

	"github.com/switchboard-code/switchboard/internal/tools"
)

type questionRelay struct {
	mu sync.Mutex
	to tools.Questioner
}

func (r *questionRelay) set(q tools.Questioner) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.to = q
}

func (r *questionRelay) AskUser(ctx context.Context, q tools.Question) (tools.Answer, error) {
	r.mu.Lock()
	to := r.to
	r.mu.Unlock()
	if to == nil {
		return tools.Answer{}, errors.New("no question channel is attached to this session")
	}
	return to.AskUser(ctx, q)
}
