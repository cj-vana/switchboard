package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/cj-vana/switchboard/internal/provider"
)

// statusLine is the always-on readout §14 requires, drawn as one continuous
// surface across the bottom: routing visible at rest. Left to right — the
// active rung's chip and target, then the ladder strip (every rung as a
// block in its heat color, the active one raised), mode, effort, spend, and
// the context gauge. Segments are separated by space on the shared ground,
// not by rows of middle dots.
//
//	▌t3 kimi▐ kimi/coding/kimi-for-coding-highspeed   ▂▂█▂  acceptEdits  plan  ▓▓▓░░░░░░░ 34%
func (m *tuiModel) statusLine() string {
	th := m.th
	width := m.width
	rank := m.activeRank()

	chip := m.app.tier.ID
	if m.app.tier.Label != "" {
		chip += " " + m.app.tier.Label
	}
	chipStyle := th.tierChip
	if rank >= 0 {
		chipStyle = th.rungChip(rank)
	}

	target := ""
	if i := strings.Index(m.tierLine, "  "); i >= 0 {
		target = strings.TrimSpace(m.tierLine[i:])
	}

	sep := th.onBar(lipgloss.NewStyle()).Render("  ")
	var right []string
	if strip := m.ladderStrip(); strip != "" {
		right = append(right, strip)
	}
	// One filled chip on the bar reads as deliberate; three read as a
	// toolbar. The rung chip keeps its fill; mode is its hue as text.
	if chipS, ok := th.modeChip[string(m.mode)]; ok {
		modeStyle := lipgloss.NewStyle().Foreground(chipS.GetBackground())
		right = append(right, th.onBar(modeStyle).Render(string(m.mode)))
	} else {
		right = append(right, th.onBar(th.dim).Render(string(m.mode)))
	}
	if effort := effortOf(m.app.tier.Target); effort != "" {
		right = append(right, th.onBar(th.dim).Render("think "+effort))
	}
	if chip := m.watchChip(); chip != "" {
		right = append(right, chip)
	}
	if m.updateAvail != "" {
		right = append(right, th.onBar(th.warn).Render("↑ "+m.updateAvail))
	}
	right = append(right, th.onBar(th.ok).Render(m.costLine))
	if g := m.ctxGauge(); g != "" {
		right = append(right, g)
	}
	if m.tr.offset > 0 {
		right = append(right, th.onBar(th.dim).Render(fmt.Sprintf("↑%d", m.tr.offset)))
	}

	rightStr := strings.Join(right, sep) + th.onBar(lipgloss.NewStyle()).Render(" ")
	rightW := lipgloss.Width(rightStr)

	// The target shrinks first on a narrow terminal: it is the longest thing
	// here and the chip already names the rung.
	avail := width - lipgloss.Width(chipStyle.Render(" "+chip+" ")) - rightW - 3
	if avail < len(target) {
		target = truncate(target, max(avail, 8))
	}
	left := chipStyle.Render(" "+chip+" ") + th.onBar(th.dim).Render(" "+target)

	gap := width - lipgloss.Width(left) - rightW
	if gap < 1 {
		gap = 1
	}
	return left + th.onBar(lipgloss.NewStyle()).Render(strings.Repeat(" ", gap)) + rightStr
}

// ladderStrip draws the whole ladder as one block per rung in its heat
// color, the active rung raised to a full block: position and depth in four
// cells. Each block is state, not decoration — its color is a rung, its
// height is whether work runs there now.
func (m *tuiModel) ladderStrip() string {
	tiers := m.app.config.Tiers
	if len(tiers) < 2 || len(tiers) > 8 {
		return ""
	}
	rank := m.activeRank()
	var b strings.Builder
	for i := range tiers {
		block := "▂"
		if i == rank {
			block = "█"
		}
		b.WriteString(m.th.onBar(m.th.rung(i)).Render(block))
	}
	return b.String()
}

// ctxGauge draws the context-window fill as a ten-cell bar, colored by how
// close it is to the wall: fine, warm, and about to be a problem.
func (m *tuiModel) ctxGauge() string {
	if m.ctxWindow <= 0 || m.callTokens <= 0 {
		return ""
	}
	pct := m.callTokens * 100 / m.ctxWindow
	if pct > 100 {
		pct = 100
	}
	fill := pct / 10
	bar := strings.Repeat("▓", fill) + strings.Repeat("░", 10-fill)

	style := m.th.barFill
	switch {
	case pct >= 85:
		style = m.th.err
	case pct >= 60:
		style = m.th.warn
	}
	return m.th.onBar(style).Render(bar) + m.th.onBar(m.th.dim).Render(fmt.Sprintf(" %d%%", pct))
}

// effortOf reports the reasoning effort riding on a target, or "".
func effortOf(t provider.RouteTarget) string {
	if t.Params.Reasoning == nil || !t.Params.Reasoning.Enabled {
		return ""
	}
	return t.Params.Reasoning.Effort
}
