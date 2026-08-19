package main

// Pictures an external tool returned, delivered at the round boundary.
//
// They ride an injected user-role message rather than the tool result because
// every adapter already maps provider.Image inside a message and none has a
// captured mapping for an image inside a tool_result. Inventing one would be
// mapping a wire format nobody has run, which is the one thing the adapters
// are not allowed to do.
//
// Marked Injected like every other round-boundary message, so a log reader and
// /retry's opening check can tell a picture that rode in mid-turn from the
// message that opened the turn.

import (
	"fmt"

	"github.com/switchboard-code/switchboard/internal/provider"
)

func (a *tuiApp) toolImageRound() []provider.Message {
	if a.loop == nil || a.loop.Tools == nil {
		return nil
	}
	images := a.loop.Tools.TakeToolImages()
	if len(images) == 0 {
		return nil
	}

	blocks := make([]provider.Block, 0, len(images)+1)
	blocks = append(blocks, provider.Text{Text: fmt.Sprintf(
		"%s returned by the tool you just called:", imageCount(len(images)))})
	for _, image := range images {
		blocks = append(blocks, image)
	}
	return []provider.Message{{Role: provider.RoleUser, Content: blocks}}
}

// targetReadsImages is the gate the registry consults before queueing
// anything. It reads the live binding rather than a launch-time value, because
// an escalation or a relief substitution can change the answer mid-turn.
func (a *tuiApp) targetReadsImages() bool {
	if a.loop == nil || a.catalog == nil {
		return false
	}
	target := a.loop.Binding().Target
	if info, _, ok := a.catalog.Lookup(target); ok {
		return info.Vision
	}
	// Unknown is not yes. A rung nobody priced may well read images, but
	// sending one on that guess produces a provider refusal mid-turn, and the
	// tool result already says what was dropped and why.
	return false
}

func imageCount(n int) string {
	if n == 1 {
		return "1 image"
	}
	return fmt.Sprintf("%d images", n)
}
