package main

// Read drift at a round boundary: a file the model was shown, changed by
// something else while it worked.
//
// The registry already hashes every file the model reads, because write and
// edit refuse a file that moved since it was read. That refusal arrives after
// the model has decided and composed an edit, which costs a round to learn
// something the same evidence could have said before the round began. This is
// that, said earlier, through the seam /watch and the advisor already use.
//
// It reports and does nothing else. The stale check still fires at the write,
// unchanged, because a notice is not a guarantee that the model read it: this
// makes the common case cheap and leaves the guarantee where it was.

import (
	"fmt"
	"strings"

	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/tools"
)

// maxDriftNamed bounds the message. Past a handful of files the useful sentence
// is "a lot moved", and the count says that better than a list would.
const maxDriftNamed = 8

func (a *tuiApp) driftRound() []provider.Message {
	if a.loop == nil || a.loop.Tools == nil {
		return nil
	}
	drifted := a.loop.Tools.DriftedReads()
	if len(drifted) == 0 {
		return nil
	}
	return []provider.Message{provider.UserText(renderDrift(drifted))}
}

func renderDrift(drifted []tools.DriftedRead) string {
	var b strings.Builder
	b.WriteString("Files you read earlier have changed since, and not through your own write or edit calls:\n")

	named := drifted
	if len(named) > maxDriftNamed {
		named = named[:maxDriftNamed]
	}
	for _, d := range named {
		switch {
		case d.Gone:
			fmt.Fprintf(&b, "- %s no longer exists\n", d.Path)
		case d.Unverified:
			fmt.Fprintf(&b, "- %s was touched and is too large to re-check, so it may or may not differ\n", d.Path)
		default:
			fmt.Fprintf(&b, "- %s\n", d.Path)
		}
	}
	if len(drifted) > len(named) {
		fmt.Fprintf(&b, "- and %d more\n", len(drifted)-len(named))
	}

	b.WriteString("\nRead one again before you change it. A shell command, a formatter, a " +
		"branch switch, or the user's own editor can do this; the change is not in your " +
		"tool record, so this notice is the only evidence of it.")
	return b.String()
}
