package main

// The one line a session listing shows for a log, shared by the /resume
// picker and `sb -sessions`: an id names a file, the opening names a
// conversation.

import (
	"strings"

	"github.com/cj-vana/switchboard/internal/session"
)

// openingLabel is the first words the user sent, collapsed to one line and
// cut to listing width. A compacted session's first user message is the seed
// — a preamble every compacted session shares — so the label skips to the
// summary's own first words; auto-compaction means the users with the most
// sessions to tell apart are exactly the ones whose logs open this way.
// Empty means the caller keeps whatever it was already showing: a log with
// no user turn yet, or one that cannot be read, is not worth a label that
// lies about it.
func openingLabel(path string) string {
	opening, err := session.ReadOpening(path)
	if err != nil || opening == "" {
		return ""
	}
	if strings.HasPrefix(opening, compactSeedHead) {
		if _, summary, ok := strings.Cut(opening, "\n\n"); ok {
			opening = summary
		}
	}
	return truncate(strings.Join(strings.Fields(opening), " "), 56)
}
