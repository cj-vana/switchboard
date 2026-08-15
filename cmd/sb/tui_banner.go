package main

// The opening frame. What a routing tool should show first is the ladder:
// every rung in its heat color, the active one barred, each priced in one
// word. That is the product stating its thesis before the first prompt, and
// it replaces a column of dim key-value lines nobody read.

import (
	"fmt"
	"strings"

	"github.com/cj-vana/switchboard/internal/catalog"
	"github.com/cj-vana/switchboard/internal/config"
	"github.com/cj-vana/switchboard/internal/session"
)

func (m *tuiModel) addBanner(sess *session.Session, resumed bool) {
	th, app := m.th, m.app
	state := sess.State()
	activeRank := m.activeRank()

	brand := th.bold.Render("switchboard")
	if v := currentVersion(); v != "" {
		brand += " " + th.dim.Render(v)
	}
	mark := th.accent
	if activeRank >= 0 {
		mark = th.rung(activeRank)
	}
	lines := []string{mark.Render("▌ ") + brand}

	if len(app.config.Tiers) > 0 {
		widest := 0
		for _, t := range app.config.Tiers {
			if n := len(t.Target.ModelID); n > widest {
				widest = n
			}
		}
		for rank, t := range app.config.Tiers {
			bar := "  "
			if rank == activeRank {
				bar = th.rung(rank).Render("▌ ")
			}
			row := bar + th.rung(rank).Render(t.ID) + "  " +
				th.text.Render(padRight(t.Target.ModelID, min(widest, 40))) +
				"  " + th.faint.Render(meteringWord(app.catalog, t))
			lines = append(lines, row)
		}
	} else {
		lines = append(lines, th.dim.Render("  no ladder configured; /models binds one"))
	}

	facts := []string{app.workspace, string(app.loop.Perms.Mode()), app.capability.Summary()}
	lines = append(lines,
		th.faint.Render("  "+strings.Join(facts, " · ")),
		th.faint.Render("  session "+state.ID+sessionNote(state, resumed)+" · /help for commands"),
	)
	if lost := sess.TruncatedBytes(); lost > 0 {
		lines = append(lines, th.warn.Render(fmt.Sprintf(
			"  recovered from an interrupted write; %d bytes at the end of the log were unreadable and were dropped", lost)))
	}
	lines = append(lines, "")

	m.tr.add(&entry{kind: kindRaw, text: strings.Join(lines, "\n"), rank: -1})
	m.tr.scrollToBottom()
}

func sessionNote(state session.State, resumed bool) string {
	if !resumed {
		return ""
	}
	return fmt.Sprintf(", resumed with %d messages", len(state.Messages))
}

// meteringWord is the one-word price of a rung: what choosing it consumes.
func meteringWord(cat *catalog.Catalog, t config.Tier) string {
	info, _, ok := cat.Lookup(t.Target)
	if !ok {
		return "unpriced"
	}
	switch info.Metering {
	case catalog.Local:
		return "local"
	case catalog.Plan:
		return "plan"
	}
	if info.Free() {
		return "free"
	}
	if band, bok := info.Band(0); bok {
		return band.InputPerMTok.String() + "/MTok in"
	}
	return "metered"
}
